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

// withHookState runs fn on the emission state of sessionID under an exclusive flock, and saves the
// state when fn reports a change. A state of another session starts empty. Errors are the caller's
// to treat as «emit anyway»: a duplicate is better than a silent loss.
func (m *Manager) withHookState(sessionID string, fn func(*hookEmitState) bool) error {
	lock, err := os.OpenFile(filepath.Join(m.StateDir, hookEmitFile+".lock"), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return fmt.Errorf("hook state lock: %w", err)
	}
	defer lock.Close()
	locked := false
	for range 50 {
		if err = syscall.Flock(int(lock.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err == nil {
			locked = true
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if !locked {
		return fmt.Errorf("hook state lock: %w", err)
	}
	defer func() { _ = syscall.Flock(int(lock.Fd()), syscall.LOCK_UN) }()

	path := filepath.Join(m.StateDir, hookEmitFile)
	var st hookEmitState
	b, err := os.ReadFile(path)
	switch {
	case err == nil:
		if json.Unmarshal(b, &st) != nil {
			st = hookEmitState{}
		}
	case !errors.Is(err, os.ErrNotExist):
		return fmt.Errorf("hook state: %w", err)
	}
	if st.SessionID != sessionID {
		st = hookEmitState{SessionID: sessionID}
	}
	if !fn(&st) {
		return nil
	}
	out, err := json.Marshal(st)
	if err != nil {
		return fmt.Errorf("hook state: %w", err)
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, out, 0o600); err != nil {
		return fmt.Errorf("hook state: %w", err)
	}
	if err := os.Rename(tmp, path); err != nil {
		return fmt.Errorf("hook state: %w", err)
	}
	return nil
}

// StartHookEpoch begins a new context epoch of sessionID (SessionStart: startup, resume, clear and
// compaction all start one), so every deduplicated block is emitted once more.
func (m *Manager) StartHookEpoch(sessionID string) error {
	return m.withHookState(sessionID, func(st *hookEmitState) bool {
		st.Epoch++
		st.Kinds = nil
		return true
	})
}

// shouldEmitHook decides whether the block of kind with the given semantic state hash is injected now,
// and records the decision. An empty hash is «nothing pending»: never emitted, but remembered, so the
// same jobs reappearing later count as a change. When the state cannot be read or written the block is
// emitted (the error is logged).
func (m *Manager) shouldEmitHook(sessionID, kind, hash string) bool {
	now := m.Now()
	if hash == "" && !m.hookRecordHasState(sessionID, kind) {
		// Nothing pending and nothing to forget: write no file (the prompt hook is otherwise read-only).
		return false
	}
	emit := true
	err := m.withHookState(sessionID, func(st *hookEmitState) bool {
		rec, ok := st.Kinds[kind]
		if hash == "" {
			emit = false
			if ok && rec.Hash == "" && rec.Epoch == st.Epoch {
				return false
			}
			rec.Hash, rec.Epoch = "", st.Epoch
		} else {
			emit = !ok || rec.Epoch != st.Epoch || rec.Hash != hash ||
				now.Sub(time.Unix(rec.LastEmitted, 0)) >= m.Config().Hooks.RepeatAfter()
			if !emit {
				return false
			}
			rec = hookEmitRecord{Epoch: st.Epoch, Hash: hash, LastEmitted: now.Unix()}
		}
		if st.Kinds == nil {
			st.Kinds = map[string]hookEmitRecord{}
		}
		st.Kinds[kind] = rec
		return true
	})
	if err != nil {
		if m.Log != nil {
			m.Log.Printf("hook dedupe: %v (emitting)", err)
		}
		return hash != ""
	}
	return emit
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
