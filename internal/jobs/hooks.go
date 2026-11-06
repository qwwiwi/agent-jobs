package jobs

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"
	"unicode/utf8"
)

// Hook injections (#166). Text a hook prints becomes context of the orchestrator session, so it is
// deduplicated and capped:
//   - the pending-outcome block is emitted when its semantic state (job ids and statuses, never the
//     changing ages) differs from the last emission, after config hooks.repeat_after_seconds, or in a
//     new context epoch (every SessionStart, which also fires after a compaction);
//   - every injected block, and the Stop hook's owner-waits reason, is at most hooks.max_bytes, with an
//     explicit "+N more, see job_list" tail where items were dropped.
//
// The emission state is a small file in the agent's state dir (hook-emit.json), not a table: the
// prompt hook opens the store read-only and must never wait for a lock.

// Defaults and limits of the hooks section of config.json.
const (
	DefaultHookRepeatAfter = 1800 * time.Second
	DefaultHookMaxBytes    = 1500
	// MinHookMaxBytes keeps room for the fixed text of the owner-waits reason and a "+N more" tail.
	MinHookMaxBytes = 512
	// CeilingHookMaxBytes bounds how much one hook may push into the context.
	CeilingHookMaxBytes = 64 * 1024
	// CeilingHookRepeatAfterSeconds is one week.
	CeilingHookRepeatAfterSeconds = 7 * 24 * 3600
)

const (
	hookEmitFile    = "hook-emit.json"
	hookKindOutcome = "pending_outcome"
	// moreTailFormat is the explicit tail of a list cut short.
	moreTailFormat = "+%d more, see job_list"
	// truncationMarker ends a text cut at the byte cap that is not a list of items.
	truncationMarker = " [truncated]"
)

// HooksConfig is the hooks section of config.json (all fields optional).
type HooksConfig struct {
	// RepeatAfterSeconds is how long an unchanged pending-outcome block is not injected again.
	// 0 = DefaultHookRepeatAfter (1800).
	RepeatAfterSeconds int `json:"repeat_after_seconds"`
	// MaxBytes caps every block a hook injects. 0 = DefaultHookMaxBytes (1500).
	MaxBytes int `json:"max_bytes"`
}

func (h HooksConfig) validate() error {
	switch {
	case h.RepeatAfterSeconds < 0 || h.RepeatAfterSeconds > CeilingHookRepeatAfterSeconds:
		return fmt.Errorf("hooks.repeat_after_seconds %d must be 0 (default %d) or 1..%d", h.RepeatAfterSeconds,
			int(DefaultHookRepeatAfter/time.Second), CeilingHookRepeatAfterSeconds)
	case h.MaxBytes != 0 && (h.MaxBytes < MinHookMaxBytes || h.MaxBytes > CeilingHookMaxBytes):
		return fmt.Errorf("hooks.max_bytes %d must be 0 (default %d) or %d..%d", h.MaxBytes, DefaultHookMaxBytes,
			MinHookMaxBytes, CeilingHookMaxBytes)
	}
	return nil
}

// RepeatAfter returns the repeat interval of an unchanged block.
func (h HooksConfig) RepeatAfter() time.Duration {
	if h.RepeatAfterSeconds > 0 {
		return time.Duration(h.RepeatAfterSeconds) * time.Second
	}
	return DefaultHookRepeatAfter
}

// Max returns the byte cap of an injected block.
func (h HooksConfig) Max() int {
	if h.MaxBytes > 0 {
		return h.MaxBytes
	}
	return DefaultHookMaxBytes
}

// capItems joins the items with sep after head, as many whole items as fit in maxBytes together with
// foot, and adds the "+N more, see job_list" tail when it cut the list. total is the number of items the
// caller has in all: items it left out itself (a lower cap of its own) count in the tail. A first item that
// does not fit alone is cut on a rune boundary, so the result always respects maxBytes (the head and
// foot are the caller's fixed text and must leave room: MinHookMaxBytes).
func capItems(head string, items []string, total int, sep, foot string, maxBytes int) string {
	budget := maxBytes - len(head) - len(foot)
	tail := func(left int) string { return sep + fmt.Sprintf(moreTailFormat, left) }
	kept, used := 0, 0
	for i, it := range items {
		add := len(it)
		if i > 0 {
			add += len(sep)
		}
		reserve := 0
		if total-(i+1) > 0 {
			reserve = len(tail(total - (i + 1)))
		}
		if used+add+reserve > budget {
			break
		}
		used += add
		kept++
	}
	var b strings.Builder
	b.WriteString(head)
	b.WriteString(strings.Join(items[:kept], sep))
	left := total - kept
	if kept == 0 && len(items) > 0 {
		// The first item alone is too long: keep its beginning and count it as shown.
		if room := budget - len(tail(total-1)) - len("…"); room > 0 {
			b.WriteString(cutRunes(items[0], room))
			b.WriteString("…")
			left = total - 1
		}
	}
	if left > 0 {
		if kept > 0 || b.Len() > len(head) {
			b.WriteString(tail(left))
		} else {
			b.WriteString(fmt.Sprintf(moreTailFormat, left))
		}
	}
	b.WriteString(foot)
	return b.String()
}

// capText cuts s to at most maxBytes on a rune boundary and ends a cut text with truncationMarker.
func capText(s string, maxBytes int) string {
	if len(s) <= maxBytes {
		return s
	}
	return cutRunes(s, maxBytes-len(truncationMarker)) + truncationMarker
}

// HookMaxBytesOf returns hooks.max_bytes of agent's config without opening its state; the default when
// agent is empty or its config cannot be read (a hook must still answer).
func HookMaxBytesOf(agent string) int {
	if agent == "" {
		return DefaultHookMaxBytes
	}
	dir, err := StateDir(agent)
	if err != nil {
		return DefaultHookMaxBytes
	}
	cfg, err := LoadConfig(dir)
	if err != nil {
		return DefaultHookMaxBytes
	}
	return cfg.Hooks.Max()
}

// CapHookBlock is capText with the configured cap, for the blocks of the hooks.
func (m *Manager) CapHookBlock(s string) string { return capText(s, m.Config().Hooks.Max()) }

// cutRunes returns the longest prefix of s of at most n bytes that ends on a rune boundary.
func cutRunes(s string, n int) string {
	if n <= 0 {
		return ""
	}
	if len(s) <= n {
		return s
	}
	for n > 0 && !utf8.RuneStart(s[n]) {
		n--
	}
	return s[:n]
}

// hookEmitRecord is what one hook kind last emitted.
type hookEmitRecord struct {
	// Epoch is the context epoch of the last emission.
	Epoch int64 `json:"epoch"`
	// Hash is the semantic state of the last emission ("" = nothing pending).
	Hash string `json:"hash"`
	// LastEmitted is the unix time of the last emission.
	LastEmitted int64 `json:"last_emitted"`
}

// hookEmitState is the content of hook-emit.json: per bound session, its context epoch and the
// last emission of each hook kind.
type hookEmitState struct {
	SessionID string                    `json:"session_id"`
	Epoch     int64                     `json:"epoch"`
	Kinds     map[string]hookEmitRecord `json:"kinds"`
}

// hookStateTx is the emission state of one session held under the exclusive flock of hook-emit.json.lock.
// Errors of its users are theirs to treat as «emit anyway»: a duplicate is better than a silent loss.
type hookStateTx struct {
	lock *os.File
	path string
	st   hookEmitState
}

// lockHookState takes the lock (waiting at most half a second) and reads the state of sessionID; a state
// of another session starts empty.
func (m *Manager) lockHookState(sessionID string) (*hookStateTx, error) {
	lock, err := os.OpenFile(filepath.Join(m.StateDir, hookEmitFile+".lock"), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, fmt.Errorf("hook state lock: %w", err)
	}
	locked := false
	for range 50 {
		if err = syscall.Flock(int(lock.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err == nil {
			locked = true
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if !locked {
		_ = lock.Close()
		return nil, fmt.Errorf("hook state lock: %w", err)
	}
	tx := &hookStateTx{lock: lock, path: filepath.Join(m.StateDir, hookEmitFile)}
	b, err := os.ReadFile(tx.path)
	switch {
	case err == nil:
		if json.Unmarshal(b, &tx.st) != nil {
			tx.st = hookEmitState{}
		}
	case !errors.Is(err, os.ErrNotExist):
		tx.close()
		return nil, fmt.Errorf("hook state: %w", err)
	}
	if tx.st.SessionID != sessionID {
		tx.st = hookEmitState{SessionID: sessionID}
	}
	return tx, nil
}

// save writes the state (temp file and rename).
func (tx *hookStateTx) save() error {
	out, err := json.Marshal(tx.st)
	if err != nil {
		return fmt.Errorf("hook state: %w", err)
	}
	tmp := tx.path + ".tmp"
	if err := os.WriteFile(tmp, out, 0o600); err != nil {
		return fmt.Errorf("hook state: %w", err)
	}
	if err := os.Rename(tmp, tx.path); err != nil {
		return fmt.Errorf("hook state: %w", err)
	}
	return nil
}

// close releases the lock (idempotent).
func (tx *hookStateTx) close() {
	if tx == nil || tx.lock == nil {
		return
	}
	_ = syscall.Flock(int(tx.lock.Fd()), syscall.LOCK_UN)
	_ = tx.lock.Close()
	tx.lock = nil
}

// withHookState runs fn on the emission state of sessionID under the lock, and saves the state when fn
// reports a change.
func (m *Manager) withHookState(sessionID string, fn func(*hookEmitState) bool) error {
	tx, err := m.lockHookState(sessionID)
	if err != nil {
		return err
	}
	defer tx.close()
	if !fn(&tx.st) {
		return nil
	}
	return tx.save()
}

// StartHookEpoch begins a new context epoch of sessionID (SessionStart: startup, resume, clear and
// compaction all start one), so every deduplicated block is emitted once more.
//
// The new epoch must outlive a failed write and a failed delivery: when the state cannot be saved, the
// old record (previous epoch, hash, time) would keep suppressing the snapshot in every later process. So
// the old record is dropped from disk instead; an absent record means «emit», and stays that way until a
// delivery is recorded. Only when even that fails is the error returned (the caller then forces the
// block out of this process).
func (m *Manager) StartHookEpoch(sessionID string) error {
	err := m.withHookState(sessionID, func(st *hookEmitState) bool {
		st.Epoch++
		st.Kinds = nil
		return true
	})
	if err == nil {
		return nil
	}
	if rerr := os.Remove(filepath.Join(m.StateDir, hookEmitFile)); rerr == nil || errors.Is(rerr, os.ErrNotExist) {
		m.logf("hook dedupe: %v (dropped the old emission record instead)", err)
		return nil
	}
	return err
}

// HookEmission is a block a hook decided to inject. The decision is made under the state lock, and the
// lock stays held until the caller reports the outcome of delivery: Commit after the text reached
// stdout (it records the emission), Release when it did not (nothing is recorded, so the next run
// injects it again). A decision, a successful write and the record are thus one serialized step, and a
// run that never delivers (a write error, the hook's timeout) cannot mark an unseen block as emitted.
type HookEmission struct {
	// Text is the block to print.
	Text string

	tx   *hookStateTx // nil: the state was unavailable (fail open), so nothing can be recorded
	kind string
	rec  hookEmitRecord
}

// Line returns the text of e, "" for a nil emission (nothing to inject).
func (e *HookEmission) Line() string {
	if e == nil {
		return ""
	}
	return e.Text
}

// Commit records the emission and releases the lock. A state that cannot be written only means one more
// emission later. Safe on a nil emission and after Release.
func (e *HookEmission) Commit() error {
	if e == nil || e.tx == nil || e.tx.lock == nil {
		return nil
	}
	defer e.tx.close()
	if e.tx.st.Kinds == nil {
		e.tx.st.Kinds = map[string]hookEmitRecord{}
	}
	e.tx.st.Kinds[e.kind] = e.rec
	return e.tx.save()
}

// Release drops the emission without recording it and releases the lock. Safe on a nil emission and
// after Commit.
func (e *HookEmission) Release() {
	if e != nil {
		e.tx.close()
	}
}

// planHook decides whether the block of kind with the given semantic state hash is injected now. It
// returns nil for «not now». An empty hash is «nothing pending»: never emitted, but remembered, so the
// same jobs reappearing later count as a change. The returned emission (Text unset) holds the state lock
// until Commit or Release. When the state cannot be read the block is emitted (the error is logged)
// and cannot be recorded. A failed epoch reset (BindHookSession) forces the next block out.
func (m *Manager) planHook(sessionID, kind, hash string) *HookEmission {
	now := m.Now()
	force := m.forceHookSnapshot.Swap(false)
	if hash == "" && !m.hookRecordHasState(sessionID, kind) {
		// Nothing pending and nothing to forget: write no file (the prompt hook is otherwise read-only).
		return nil
	}
	tx, err := m.lockHookState(sessionID)
	if err != nil {
		m.logf("hook dedupe: %v (emitting)", err)
		if hash == "" {
			return nil
		}
		return &HookEmission{}
	}
	rec, ok := tx.st.Kinds[kind]
	if hash == "" {
		if !ok || rec.Hash != "" || rec.Epoch != tx.st.Epoch {
			if tx.st.Kinds == nil {
				tx.st.Kinds = map[string]hookEmitRecord{}
			}
			tx.st.Kinds[kind] = hookEmitRecord{Epoch: tx.st.Epoch}
			if err := tx.save(); err != nil {
				m.logf("hook dedupe: %v", err)
			}
		}
		tx.close()
		return nil
	}
	if !force && ok && rec.Epoch == tx.st.Epoch && rec.Hash == hash &&
		now.Sub(time.Unix(rec.LastEmitted, 0)) < m.Config().Hooks.RepeatAfter() {
		tx.close()
		return nil
	}
	return &HookEmission{tx: tx, kind: kind, rec: hookEmitRecord{Epoch: tx.st.Epoch, Hash: hash, LastEmitted: now.Unix()}}
}

func (m *Manager) logf(format string, args ...any) {
	if m.Log != nil {
		m.Log.Printf(format, args...)
	}
}

// hookRecordHasState reports, without the lock, whether the state file holds a non-empty record of kind
// for sessionID (the only case where an empty state must be written down). A file it cannot read counts
// as «yes», so the locked path reports the error.
func (m *Manager) hookRecordHasState(sessionID, kind string) bool {
	b, err := os.ReadFile(filepath.Join(m.StateDir, hookEmitFile))
	if errors.Is(err, os.ErrNotExist) {
		return false
	}
	var st hookEmitState
	if err != nil || json.Unmarshal(b, &st) != nil {
		return true
	}
	return st.SessionID == sessionID && st.Kinds[kind].Hash != ""
}
