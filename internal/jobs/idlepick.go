package jobs

import (
	"context"
	"fmt"
	"strings"
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

// idleBeforeSend, when set (tests only), runs after an idle note was composed and before it is typed:
// the gap where a job may be enqueued.
var idleBeforeSend func()

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
// notes and recovery lines already delivered.
func (m *Manager) idleGate(now time.Time) (ok bool, err error) {
	c := m.Config().IdlePick
	last, err := m.Store.LastIdlePickEvent(store.IdleNote, store.IdleRecovery)
	if err != nil {
		return false, err
	}
	if last != nil && now.Sub(*last) < c.Cooldown() {
		return false, nil
	}
	n, err := m.Store.CountIdlePickEvents(now.Add(-24*time.Hour), store.IdleNote, store.IdleRecovery)
	if err != nil {
		return false, err
	}
	return n < c.MaxWakes(), nil
}

// idleNote returns the idle note for the job-done delivery of batch, "" when there is none to add:
// idle pick is off; a job is still queued, running or stopped, or another job-done line is still to
// come (the last one carries the note); this queue epoch (everything since the last evaluation) was
// already evaluated; nothing is ready; or the cooldown or the daily cap holds it back. The note is not
// recorded here but by idleNoteSent once tmux took the line, so a failed delivery composes it again.
func (m *Manager) idleNote(batch []*store.Job) string {
	if !m.Config().IdlePick.Enabled {
		return ""
	}
	if n, err := m.Store.CountActive(); err != nil || n > 0 {
		m.logIdle("queue check", err)
		return ""
	}
	pending, err := m.Store.PendingNotify()
	if err != nil {
		m.logIdle("pending lines", err)
		return ""
	}
	inBatch := map[string]bool{}
	var finished time.Time
	for _, j := range batch {
		inBatch[j.ID] = true
		if j.FinishedAt != nil && j.FinishedAt.After(finished) {
			finished = *j.FinishedAt
		}
	}
	for _, j := range pending {
		if !inBatch[j.ID] {
			return ""
		}
	}
	evaluated, err := m.Store.LastIdlePickEvent(store.IdleEval, store.IdleNote, store.IdleRecovery)
	if err != nil {
		m.logIdle("last evaluation", err)
		return ""
	}
	if evaluated != nil && !evaluated.Before(finished) {
		return ""
	}
	return m.idleEvaluate()
}

// idleEvaluate reads the ready work and applies the gate, recording an evaluation that found nothing
// (the epoch is spent) or one the gate held back (the epoch stays open for the recovery scan). It
// returns the note, or "".
func (m *Manager) idleEvaluate() string {
	now := m.Now()
	w, err := m.idleReadyWork(context.Background())
	if err != nil {
		m.logIdle("ready work", err)
		return ""
	}
	if w.empty() {
		m.recordIdle(store.IdleEval, now)
		return ""
	}
	ok, err := m.idleGate(now)
	if err != nil {
		m.logIdle("gate", err)
		return ""
	}
	if !ok {
		m.recordIdle(store.IdleSuppressed, now)
		return ""
	}
	return RedactPrintable(w.text(), 0)
}

// idleStillEmpty is the check right before a note is typed: a job enqueued since it was composed
// cancels it (the epoch goes on, the next queue-empty delivery carries the note).
func (m *Manager) idleStillEmpty() bool {
	if idleBeforeSend != nil {
		idleBeforeSend()
	}
	n, err := m.Store.CountActive()
	return err == nil && n == 0
}

func (m *Manager) recordIdle(kind string, at time.Time) {
	if err := m.Store.AddIdlePickEvent(kind, at); err != nil {
		m.Log.Printf("idle pick: %v", err)
	}
}

func (m *Manager) logIdle(what string, err error) {
	if err != nil {
		m.Log.Printf("idle pick: %s: %s", what, RedactText(err.Error()))
	}
}

// idleRecoveryLinePrefix starts the line of a recovery scan.
const idleRecoveryLinePrefix = "[jobs:idle] "

// idleRecovery is the low-frequency recovery scan: a queue-empty transition whose delivery never got
// an idle evaluation (idle pick was off then, the evaluation failed, the gate held its note back) is
// not lost. With the queue empty, no line pending and a job finished since the last evaluation, once
// per recovery_scan_seconds it reads the ready work and, if there is any and the gate allows, types
// one idle line of its own. It never starts work.
func (m *Manager) idleRecovery() error {
	cfg := m.Config()
	every := cfg.IdlePick.RecoveryEvery()
	target := cfg.NotifyTmuxTarget
	if !cfg.IdlePick.Enabled || every == 0 || target == "" {
		return nil
	}
	if n, err := m.Store.CountActive(); err != nil || n > 0 {
		return err
	}
	finished, err := m.Store.LastFinishedAt()
	if err != nil || finished == nil {
		return err
	}
	evaluated, err := m.Store.LastIdlePickEvent(store.IdleEval, store.IdleNote, store.IdleRecovery)
	if err != nil {
		return err
	}
	if evaluated != nil && !evaluated.Before(*finished) {
		return nil
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
	note := m.idleEvaluate()
	m.recordIdle(store.IdleScan, now)
	if note == "" || !m.idleStillEmpty() {
		return nil
	}
	if err := m.sendTmuxLocked(target, idleRecoveryLinePrefix+note); err != nil {
		m.Log.Printf("idle pick: recovery line not delivered, retry at the next scan: %s", RedactText(err.Error()))
		return nil
	}
	m.recordIdle(store.IdleRecovery, m.Now())
	return nil
}
