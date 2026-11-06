package jobs

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/qwwiwi/agent-jobs/internal/store"
)

// Idle pick defaults and ceilings (#165).
const (
	DefaultIdleCooldown        = 10 * time.Minute
	DefaultIdleMaxWakes        = 8
	CeilingIdleMaxWakes        = 48
	DefaultIdleRecovery        = 6 * time.Hour
	idleCooldownCeilingSeconds = 86400
	// idleListed caps the tasks (and the intentions) a note names; the count is always the full one.
	idleListed = 5
	// idleTitleRunes caps a task title or an intention text inside the note.
	idleTitleRunes = 40
)

// IdlePickConfig is config.json's idle_pick: when the queue empties, the job-done delivery of the
// last job says what is ready to do next, so the orchestrator needs no hourly wake to look for it.
type IdlePickConfig struct {
	// Enabled turns idle pick on. Default false.
	Enabled bool `json:"enabled"`
	// CooldownSeconds is the least gap between two idle notes. 0 = 600.
	CooldownSeconds int `json:"cooldown_seconds"`
	// MaxWakesPerDay caps the idle notes (and recovery lines) in any 24 hours. 0 = 8, at most 48.
	MaxWakesPerDay int `json:"max_wakes_per_day"`
	// RecoveryScanSeconds is the gap between recovery scans, which look for a queue-empty transition
	// that was never evaluated. 0 = 21600 (6 h); -1 = no recovery scan.
	RecoveryScanSeconds int `json:"recovery_scan_seconds"`
}

func (c IdlePickConfig) validate() error {
	switch {
	case c.CooldownSeconds < 0 || c.CooldownSeconds > idleCooldownCeilingSeconds:
		return fmt.Errorf("idle_pick.cooldown_seconds %d must be 0 (default %d) .. %d", c.CooldownSeconds,
			int(DefaultIdleCooldown/time.Second), idleCooldownCeilingSeconds)
	case c.MaxWakesPerDay < 0 || c.MaxWakesPerDay > CeilingIdleMaxWakes:
		return fmt.Errorf("idle_pick.max_wakes_per_day %d must be 0 (default %d) .. %d", c.MaxWakesPerDay, DefaultIdleMaxWakes, CeilingIdleMaxWakes)
	case c.RecoveryScanSeconds < -1:
		return fmt.Errorf("idle_pick.recovery_scan_seconds %d must be -1 (off), 0 (default %d) or positive", c.RecoveryScanSeconds,
			int(DefaultIdleRecovery/time.Second))
	}
	return nil
}

// Cooldown is the least gap between two idle notes.
func (c IdlePickConfig) Cooldown() time.Duration {
	if c.CooldownSeconds > 0 {
		return time.Duration(c.CooldownSeconds) * time.Second
	}
	return DefaultIdleCooldown
}

// MaxWakes is the cap of idle notes in 24 hours.
func (c IdlePickConfig) MaxWakes() int {
	if c.MaxWakesPerDay > 0 {
		return c.MaxWakesPerDay
	}
	return DefaultIdleMaxWakes
}

// RecoveryEvery is the gap between recovery scans; 0 = no recovery scan.
func (c IdlePickConfig) RecoveryEvery() time.Duration {
	switch {
	case c.RecoveryScanSeconds < 0:
		return 0
	case c.RecoveryScanSeconds > 0:
		return time.Duration(c.RecoveryScanSeconds) * time.Second
	}
	return DefaultIdleRecovery
}

// idleBeforeSend, when set (tests only), runs inside the tmux delivery of a line that carries a note,
// after the pane is out of its mode and before the final check under the idle lock: a gap where a job
// may be enqueued.
var idleBeforeSend func()

// idleLockName is the file under the state dir that serializes the final queue check of an idle note
// (and the typing of the line it rides) with the insertion of a job into the queue, across processes.
const idleLockName = "idle.lock"

// takeIdleLock takes <state>/idle.lock (flock), polling until ctx ends. Held by a delivery from its
// final queue check to the end of its keystrokes, and by Enqueue and Resume only around the insert.
func (m *Manager) takeIdleLock(ctx context.Context) (release func(), err error) {
	f, err := os.OpenFile(filepath.Join(m.StateDir, idleLockName), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, fmt.Errorf("open idle lock: %w", err)
	}
	for {
		err = syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
		if err == nil {
			break
		}
		if !errors.Is(err, syscall.EWOULDBLOCK) && !errors.Is(err, syscall.EINTR) {
			_ = f.Close()
			return nil, fmt.Errorf("idle lock: %w", err)
		}
		select {
		case <-ctx.Done():
			_ = f.Close()
			return nil, fmt.Errorf("idle lock still held after %s", TmuxDeliveryTimeout)
		case <-time.After(5 * time.Millisecond):
		}
	}
	return func() {
		_ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
		_ = f.Close()
	}, nil
}

// underIdleLock runs fn (a job's insertion into the queue) holding the idle lock, so an idle note
// being checked and typed never races it. The wait is bounded by one tmux delivery: a lock that cannot
// be had (a stuck holder, an unwritable state dir) is logged and fn runs without it, because a job must
// never be refused for the sake of a note.
func (m *Manager) underIdleLock(fn func() error) error {
	ctx, cancel := context.WithTimeout(context.Background(), TmuxDeliveryTimeout+time.Second)
	defer cancel()
	release, err := m.takeIdleLock(ctx)
	if err != nil {
		m.Log.Printf("idle pick: queue insert without the idle lock: %s", RedactText(err.Error()))
		return fn()
	}
	defer release()
	return fn()
}

// idleWork is what the daemon could tell the orchestrator is ready: tasks of open milestones that
// have no job yet and whose dependencies are done (TaskStatuses: ready; a task with a job of its own,
// failed or in review, is the orchestrator's to handle), and the agent's open intentions.
type idleWork struct {
	Tasks      []string // "<milestone id>:<key> "title"", oldest milestone first
	Intentions []string // "#<id> "text""
}

func (w idleWork) empty() bool { return len(w.Tasks) == 0 && len(w.Intentions) == 0 }

// idleReadyWork reads the milestones and intentions. The daemon only reads: it never starts the work.
func (m *Manager) idleReadyWork(ctx context.Context) (idleWork, error) {
	var w idleWork
	mss, err := m.Store.ListMilestones(ctx, store.MilestoneFilter{Status: store.MilestoneOpen})
	if err != nil {
		return w, err
	}
	for _, ms := range mss {
		tasks, err := m.Store.MilestoneTasks(ctx, ms.ID)
		if err != nil {
			return w, err
		}
		jobs, err := m.Store.MilestoneJobs(ctx, ms.ID)
		if err != nil {
			return w, err
		}
		hasJob := map[string]bool{}
		for _, j := range jobs {
			hasJob[j.TaskKey] = true
		}
		status := TaskStatuses(tasks, jobs)
		for _, t := range tasks {
			if status[t.Key] == TaskReady && !hasJob[t.Key] {
				w.Tasks = append(w.Tasks, fmt.Sprintf("%d:%s %q", ms.ID, t.Key, RedactPrintable(t.Title, idleTitleRunes)))
			}
		}
	}
	its, err := m.Store.ListIntentions(store.IntentionOpen)
	if err != nil {
		return w, err
	}
	for _, it := range its {
		w.Intentions = append(w.Intentions, fmt.Sprintf("#%d %q", it.ID, RedactPrintable(it.Text, idleTitleRunes)))
	}
	return w, nil
}

// idleList is "N <noun>[s]: a, b, c, +K more" for a non-empty list.
func idleList(items []string, one, many string) string {
	noun := many
	if len(items) == 1 {
		noun = one
	}
	shown := items
	if len(shown) > idleListed {
		shown = shown[:idleListed]
	}
	s := fmt.Sprintf("%d %s: %s", len(items), noun, strings.Join(shown, ", "))
	if len(items) > len(shown) {
		s += fmt.Sprintf(", +%d more", len(items)-len(shown))
	}
	return s
}

// text is the note: "idle: 2 ready tasks: 4:T2 "title", 4:T3 "title" / 1 open intention: #7 "text"".
func (w idleWork) text() string {
	var parts []string
	if len(w.Tasks) > 0 {
		parts = append(parts, idleList(w.Tasks, "ready task", "ready tasks"))
	}
	if len(w.Intentions) > 0 {
		parts = append(parts, idleList(w.Intentions, "open intention", "open intentions"))
	}
	return "idle: " + strings.Join(parts, " / ")
}

// idleGate decides whether ready work may be told now. Cooldown and the rolling daily cap count the
// notes already delivered.
func (m *Manager) idleGate(now time.Time) (ok bool, err error) {
	c := m.Config().IdlePick
	last, err := m.Store.LastIdlePickEvent(store.IdleNote)
	if err != nil {
		return false, err
	}
	if last != nil && now.Sub(*last) < c.Cooldown() {
		return false, nil
	}
	n, err := m.Store.CountIdlePickEvents(now.Add(-24*time.Hour), store.IdleNote)
	if err != nil {
		return false, err
	}
	return n < c.MaxWakes(), nil
}

// idleNote returns the idle note for the job-done delivery of batch and the queue epoch it covers, ""
// when there is none to add: idle pick is off; a job is still queued, running or stopped, or another
// job-done line is still to come (the last one carries the note); this queue epoch (every job that
// ended up to now) was already evaluated; nothing is ready; or the cooldown or the daily cap holds it
// back. The note is not recorded here but, with the acknowledgement of the line, by deliverDone once
// tmux took the line, so a failed delivery composes it again.
func (m *Manager) idleNote(batch []*store.Job) (note string, epoch int64) {
	if !m.Config().IdlePick.Enabled {
		return "", 0
	}
	if n, err := m.Store.CountActive(); err != nil || n > 0 {
		m.logIdle("queue check", err)
		return "", 0
	}
	// The epoch is read before anything else: a job that ends while the note is composed makes the
	// final check fail instead of being folded into this epoch.
	epoch, err := m.Store.FinishSeq()
	if err != nil {
		m.logIdle("epoch", err)
		return "", 0
	}
	pending, err := m.Store.PendingNotify()
	if err != nil {
		m.logIdle("pending lines", err)
		return "", 0
	}
	inBatch := map[string]bool{}
	for _, j := range batch {
		inBatch[j.ID] = true
	}
	for _, j := range pending {
		if !inBatch[j.ID] {
			return "", 0
		}
	}
	spent, err := m.idleEpochSpent(epoch)
	if err != nil {
		m.logIdle("last evaluation", err)
		return "", 0
	}
	if spent {
		return "", 0
	}
	note, err = m.idleEvaluate(epoch)
	if err != nil {
		m.logIdle("evaluation", err)
		return "", 0
	}
	return note, epoch
}

// idleEpochSpent reports whether queue epoch epoch was already evaluated: an evaluation that found
// nothing, or a note, covers every epoch up to the one it recorded.
func (m *Manager) idleEpochSpent(epoch int64) (bool, error) {
	covered, err := m.Store.LastIdlePickEpoch(store.IdleEval, store.IdleNote)
	if err != nil {
		return false, err
	}
	return covered >= epoch, nil
}

// idleEvaluate reads the ready work of queue epoch epoch and applies the gate, recording an evaluation
// that found nothing (the epoch is spent) or one the gate held back (the epoch stays open for the
// recovery scan). It returns the note, or "". A failure to read or to record is returned: the epoch
// then stays open and the next delivery or scan evaluates it again.
func (m *Manager) idleEvaluate(epoch int64) (string, error) {
	now := m.Now()
	w, err := m.idleReadyWork(context.Background())
	if err != nil {
		return "", fmt.Errorf("ready work: %w", err)
	}
	if w.empty() {
		return "", m.Store.AddIdlePickEvent(store.IdleEval, now, epoch)
	}
	ok, err := m.idleGate(now)
	if err != nil {
		return "", fmt.Errorf("gate: %w", err)
	}
	if !ok {
		return "", m.Store.AddIdlePickEvent(store.IdleSuppressed, now, epoch)
	}
	return RedactPrintable(w.text(), 0), nil
}

// idleStillEmpty is the check right before a note is typed, under the idle lock: no job is queued,
// running or stopped and none has ended since the note was composed (it would have a line of its own to
// carry the note). A job enqueued since cancels the note (the epoch goes on, the next queue-empty
// delivery carries it).
func (m *Manager) idleStillEmpty(epoch int64) bool {
	n, err := m.Store.CountActive()
	if err != nil || n > 0 {
		return false
	}
	now, err := m.Store.FinishSeq()
	return err == nil && now == epoch
}

// typeLineWithNote types line (a job-done line) into target and appends " · note" to it when the note
// still holds. It runs inside the tmux delivery, so the pane is out of its mode; the final queue check
// and the keystrokes happen under the idle lock, so no job can be inserted between the check and the
// typing. rode reports whether the note went out with the line. A lock that cannot be had types the
// line without the note.
func (m *Manager) typeLineWithNote(ctx context.Context, target, line, note string, epoch int64) (rode bool, err error) {
	if idleBeforeSend != nil {
		idleBeforeSend()
	}
	release, lerr := m.takeIdleLock(ctx)
	if lerr != nil {
		m.logIdle("final check", lerr)
		return false, m.typeTmux(ctx, target, line)
	}
	defer release()
	if m.idleStillEmpty(epoch) {
		line += " · " + note
		rode = true
	}
	return rode, m.typeTmux(ctx, target, line)
}

func (m *Manager) logIdle(what string, err error) {
	if err != nil {
		m.Log.Printf("idle pick: %s: %s", what, RedactText(err.Error()))
	}
}

// idleRecovery is the low-frequency recovery scan: a queue-empty transition whose delivery never got
// an idle evaluation (idle pick was off then, the evaluation failed, the gate held its note back) is
// not lost. With the queue empty, no line pending and a job ended since the last evaluation, once per
// recovery_scan_seconds it reads the ready work and, if there is any and the gate allows, puts the last
// delivered job's line back on the job-done delivery (RearmLastNotified): the next delivery tick types
// that job-done line with the idle note on it. The scan never types a line of its own, so it is never
// a separate wake, and it never starts work.
func (m *Manager) idleRecovery() error {
	cfg := m.Config()
	every := cfg.IdlePick.RecoveryEvery()
	if !cfg.IdlePick.Enabled || every == 0 || cfg.NotifyTmuxTarget == "" {
		return nil
	}
	if n, err := m.Store.CountActive(); err != nil || n > 0 {
		return err
	}
	epoch, err := m.Store.FinishSeq()
	if err != nil || epoch == 0 {
		return err
	}
	if spent, err := m.idleEpochSpent(epoch); err != nil || spent {
		return err
	}
	now := m.Now()
	scanned, err := m.Store.LastIdlePickEvent(store.IdleScan)
	if err != nil {
		return err
	}
	if scanned != nil && now.Sub(*scanned) < every {
		return nil
	}
	if pending, err := m.Store.PendingNotify(); err != nil || len(pending) > 0 {
		return err // a line still to come carries the note
	}
	note, err := m.idleEvaluate(epoch)
	if err != nil {
		return err
	}
	if err := m.Store.AddIdlePickEvent(store.IdleScan, now, epoch); err != nil {
		return err
	}
	if note == "" {
		return nil
	}
	id, err := m.Store.RearmLastNotified()
	if err != nil {
		return err
	}
	if id == "" {
		m.Log.Printf("idle pick: recovery found ready work but no delivered job to carry the note")
		return nil
	}
	m.Log.Printf("idle pick: recovery re-armed the job-done line of job %s to carry the idle note", id)
	return nil
}
