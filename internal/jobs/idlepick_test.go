package jobs

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/qwwiwi/agent-jobs/internal/store"
)

// idleFixture is a manager with a milestone whose T1 is accepted: T2 and T3 are ready, T4 (after T2)
// and T5 (after T4) are blocked. Its notify target is the fake tmux and idle pick is on.
type idleFixture struct {
	m    *Manager
	logf string
	now  *time.Time
	t0   time.Time
}

func newIdleFixture(t *testing.T) *idleFixture {
	t.Helper()
	m := newManager(t)
	ctx := context.Background()
	dep := func(keys ...string) *[]string { return &keys }
	// An edge without a reason is dropped (#119), so every edge carries one.
	why := func(key string) *map[string]string { return &map[string]string{key: "shares files"} }
	v, err := m.CreateMilestone(ctx, MilestoneCreate{Title: "Stage", Tasks: []MilestoneTaskInput{
		{Key: "T1", Title: "first"},
		{Key: "T2", Title: "second", DependsOn: dep("T1"), DependsWhy: why("T1")},
		{Key: "T3", Title: "third"},
		{Key: "T4", Title: "fourth", DependsOn: dep("T2"), DependsWhy: why("T2")},
		{Key: "T5", Title: "fifth", DependsOn: dep("T4"), DependsWhy: why("T4")},
	}})
	if err != nil {
		t.Fatal(err)
	}
	j1 := enqueue(t, m, "t1", func(r *StartRequest) { r.Milestone, r.Task = v.ID, "T1" })
	runToEnd(t, m, j1.ID) // no notify target yet: marked notified on the spot
	if err := m.SetOutcome(j1.ID, OutcomeAccepted, ""); err != nil {
		t.Fatal(err)
	}
	f := &idleFixture{m: m, logf: notifyTo(t, m)}
	f.now = coalesceClock(m)
	f.t0 = *f.now
	m.Cfg.IdlePick.Enabled = true
	return f
}

// finish ends one job at t0+sec and returns it (cancelled while queued, so its line is pending).
func (f *idleFixture) finish(t *testing.T, sec int) *store.Job {
	t.Helper()
	return finishAt(t, f.m, f.now, f.t0.Add(time.Duration(sec)*time.Second), nil)
}

func (f *idleFixture) notify(t *testing.T, sec int) {
	t.Helper()
	notifyAt(t, f.m, f.now, f.t0.Add(time.Duration(sec)*time.Second))
}

// #165: idle pick is off unless configured: a queue-empty delivery is today's line, byte for byte.
func TestIdlePickDisabledByDefault(t *testing.T) {
	if (Config{}).IdlePick.Enabled {
		t.Fatal("idle_pick.enabled is on by default")
	}
	f := newIdleFixture(t)
	f.m.Cfg.IdlePick.Enabled = false
	j := f.finish(t, 0)
	f.notify(t, 1)
	got := tmuxLines(t, f.logf)
	if len(got) != 1 || got[0] != f.m.DoneLineFor(get(t, f.m, j.ID)) || strings.Contains(got[0], "idle") {
		t.Fatalf("lines: %q", got)
	}
}

// #165: the last job's line carries one idle note naming the ready tasks (not blocked ones, not
// accepted ones); repeated ticks and later deliveries in the same epoch say nothing more.
func TestIdlePickNoteOncePerEpoch(t *testing.T) {
	f := newIdleFixture(t)
	j := f.finish(t, 0)
	f.notify(t, 1)
	f.notify(t, 2)
	f.notify(t, 3)
	got := tmuxLines(t, f.logf)
	if len(got) != 1 {
		t.Fatalf("lines: %q", got)
	}
	head := f.m.DoneLineFor(get(t, f.m, j.ID))
	note, ok := strings.CutPrefix(got[0], head+" · ")
	if !ok || !strings.HasPrefix(note, "idle: 2 ready tasks: ") {
		t.Fatalf("line %q does not end with an idle note after %q", got[0], head)
	}
	if !strings.Contains(note, ":T2 ") || !strings.Contains(note, ":T3 ") {
		t.Fatalf("ready tasks missing: %q", note)
	}
	for _, key := range []string{":T1 ", ":T4 ", ":T5 "} {
		if strings.Contains(note, key) {
			t.Fatalf("%s listed (done or blocked): %q", strings.TrimSpace(key), note)
		}
	}
	// A new epoch inside the cooldown is held back; past it, the note comes again.
	f.finish(t, 10)
	f.notify(t, 11)
	if got := tmuxLines(t, f.logf); len(got) != 2 || strings.Contains(got[1], "idle:") {
		t.Fatalf("note inside the cooldown: %q", got)
	}
	f.finish(t, 3600)
	f.notify(t, 3601)
	if got := tmuxLines(t, f.logf); len(got) != 3 || !strings.Contains(got[2], " · idle: 2 ready tasks: ") {
		t.Fatalf("note after the cooldown: %q", got)
	}
}

// #165: with nothing ready (every task done or running, no intention) nothing is added.
func TestIdlePickNothingReady(t *testing.T) {
	m := newManager(t)
	logf := notifyTo(t, m)
	now := coalesceClock(m)
	m.Cfg.IdlePick.Enabled = true
	j := finishAt(t, m, now, *now, nil)
	notifyAt(t, m, now, now.Add(time.Second))
	got := tmuxLines(t, logf)
	if len(got) != 1 || got[0] != m.DoneLineFor(get(t, m, j.ID)) {
		t.Fatalf("lines: %q", got)
	}
}

// #165: a dropped intention is not work; an open one is, even with no milestone.
func TestIdlePickIntentions(t *testing.T) {
	m := newManager(t)
	logf := notifyTo(t, m)
	now := coalesceClock(m)
	m.Cfg.IdlePick.Enabled = true
	closed, err := m.AddIntention("old goal", "", "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := m.SetIntention(closed.ID, "", store.IntentionDropped); err != nil {
		t.Fatal(err)
	}
	t0 := *now
	finishAt(t, m, now, t0, nil)
	notifyAt(t, m, now, t0.Add(time.Second))
	if got := tmuxLines(t, logf); len(got) != 1 || strings.Contains(got[0], "idle:") {
		t.Fatalf("dropped intention counted: %q", got)
	}
	if _, err := m.AddIntention("write the release notes", "", "high"); err != nil {
		t.Fatal(err)
	}
	finishAt(t, m, now, t0.Add(time.Hour), nil)
	notifyAt(t, m, now, t0.Add(time.Hour+time.Second))
	got := tmuxLines(t, logf)
	if len(got) != 2 || !strings.Contains(got[1], ` · idle: 1 open intention: #2 "write the release notes"`) {
		t.Fatalf("open intention: %q", got)
	}
}

// #165: the note rides the SAME coalesced delivery, once, at its end.
func TestIdlePickRidesCoalescedLine(t *testing.T) {
	f := newIdleFixture(t)
	f.m.Cfg.NotifyCoalesceSeconds = 60
	f.finish(t, 0)
	f.finish(t, 10)
	f.notify(t, 20)
	if got := tmuxLines(t, f.logf); len(got) != 0 {
		t.Fatalf("lines inside the window: %q", got)
	}
	f.notify(t, 70)
	got := tmuxLines(t, f.logf)
	if len(got) != 1 || !strings.HasPrefix(got[0], "[jobs:job-done] 2 jobs: ") || strings.Count(got[0], "idle:") != 1 ||
		!strings.Contains(got[0], " · idle: 2 ready tasks: ") {
		t.Fatalf("coalesced line: %q", got)
	}
}

// #165: a restart between the finish and the delivery, or after it, never says it twice.
func TestIdlePickRestartDoesNotReEmit(t *testing.T) {
	f := newIdleFixture(t)
	f.m.Cfg.NotifyCoalesceSeconds = 60
	f.finish(t, 0)
	f.notify(t, 10)
	m2 := reopenForIdle(t, f.m, f.now)
	notifyAt(t, m2, f.now, f.t0.Add(70*time.Second))
	got := tmuxLines(t, f.logf)
	if len(got) != 1 || strings.Count(got[0], "idle:") != 1 {
		t.Fatalf("after the first restart: %q", got)
	}
	m3 := reopenForIdle(t, m2, f.now)
	for _, sec := range []int{71, 600, 8 * 3600} {
		notifyAt(t, m3, f.now, f.t0.Add(time.Duration(sec)*time.Second))
	}
	if got := tmuxLines(t, f.logf); len(got) != 1 {
		t.Fatalf("re-emitted after a restart: %q", got)
	}
}

// reopenForIdle simulates a daemon restart: a new manager on the same state, with the test's settings.
func reopenForIdle(t *testing.T, old *Manager, now *time.Time) *Manager {
	t.Helper()
	cfg, log, loc := old.Cfg, old.Log, old.Loc
	if err := old.Close(); err != nil {
		t.Fatal(err)
	}
	m, err := Open("test")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = m.Close() })
	m.Cfg, m.Log, m.Loc = cfg, log, loc
	m.Now = func() time.Time { return *now }
	return m
}

// #165: a job enqueued after the note was composed and before it is typed cancels the note; the
// epoch is not spent, so the next queue-empty delivery carries it.
func TestIdlePickConcurrentEnqueueCancelsNote(t *testing.T) {
	f := newIdleFixture(t)
	var late *store.Job
	idleBeforeSend = func() { late = enqueue(t, f.m, "late", nil) }
	t.Cleanup(func() { idleBeforeSend = nil })
	j := f.finish(t, 0)
	f.notify(t, 1)
	idleBeforeSend = nil
	got := tmuxLines(t, f.logf)
	if late == nil || len(got) != 1 || got[0] != f.m.DoneLineFor(get(t, f.m, j.ID)) {
		t.Fatalf("note not cancelled: late=%v lines=%q", late, got)
	}
	if err := f.m.Cancel(late.ID); err != nil {
		t.Fatal(err)
	}
	f.notify(t, 2)
	got = tmuxLines(t, f.logf)
	if len(got) != 2 || !strings.Contains(got[1], " · idle: 2 ready tasks: ") {
		t.Fatalf("note after the queue emptied again: %q", got)
	}
}

// #165: a delivery that leaves other work running (or other lines pending) carries no note.
func TestIdlePickQueueNotEmpty(t *testing.T) {
	f := newIdleFixture(t)
	running := enqueue(t, f.m, "still queued", nil)
	f.finish(t, 0)
	f.notify(t, 1)
	if got := tmuxLines(t, f.logf); len(got) != 1 || strings.Contains(got[0], "idle:") {
		t.Fatalf("note with a job queued: %q", got)
	}
	if err := f.m.Cancel(running.ID); err != nil {
		t.Fatal(err)
	}
	f.notify(t, 2)
	if got := tmuxLines(t, f.logf); len(got) != 2 || !strings.Contains(got[1], " · idle: ") {
		t.Fatalf("note on the last job's line: %q", got)
	}
}

// #165: max_wakes_per_day caps the notes inside a rolling day.
func TestIdlePickDailyCap(t *testing.T) {
	f := newIdleFixture(t)
	f.m.Cfg.IdlePick.CooldownSeconds = 60
	f.m.Cfg.IdlePick.MaxWakesPerDay = 2
	for i := 0; i < 4; i++ {
		f.finish(t, i*120)
		f.notify(t, i*120+1)
	}
	got := tmuxLines(t, f.logf)
	noted := 0
	for _, l := range got {
		if strings.Contains(l, "idle:") {
			noted++
		}
	}
	if len(got) != 4 || noted != 2 {
		t.Fatalf("notes %d of %d lines: %q", noted, len(got), got)
	}
	f.finish(t, 25*3600)
	f.notify(t, 25*3600+1)
	if got := tmuxLines(t, f.logf); !strings.Contains(got[len(got)-1], "idle:") {
		t.Fatalf("cap did not roll over a day: %q", got)
	}
}

// #165 F1: an epoch whose delivery got no idle note (idle pick was off then) stays missed: nothing
// replays a delivered job-done line, however many ticks, days or restarts pass. A note rides only a
// genuine pending job-done delivery.
func TestIdlePickMissedNoteIsNotReplayed(t *testing.T) {
	f := newIdleFixture(t)
	f.m.Cfg.IdlePick.Enabled = false
	j := f.finish(t, 0)
	f.notify(t, 1)
	f.m.Cfg.IdlePick.Enabled = true
	m := f.m
	for _, sec := range []int{2, 600, 7 * 3600, 30 * 3600} {
		m = reopenForIdle(t, m, f.now)
		notifyAt(t, m, f.now, f.t0.Add(time.Duration(sec)*time.Second))
	}
	got := tmuxLines(t, f.logf)
	if len(got) != 1 || got[0] != f.m.DoneLineFor(get(t, m, j.ID)) {
		t.Fatalf("a missed note produced another delivery: %q", got)
	}
	// The next genuine completion carries its note again.
	finishAt(t, m, f.now, f.t0.Add(31*time.Hour), nil)
	notifyAt(t, m, f.now, f.t0.Add(31*time.Hour+time.Second))
	if got := tmuxLines(t, f.logf); len(got) != 2 || !strings.Contains(got[1], " · idle: 2 ready tasks: ") {
		t.Fatalf("next completion: %q", got)
	}
}

// resetIdleSchema returns the database of m to the schema of a version without idle pick (no counter,
// no events, no delivery record), then reopens it: the upgrade.
func resetIdleSchema(t *testing.T, m *Manager, now *time.Time) *Manager {
	t.Helper()
	db, err := sql.Open("sqlite", filepath.Join(m.StateDir, "jobs.db"))
	if err != nil {
		t.Fatal(err)
	}
	for _, q := range []string{`DROP TRIGGER idle_pick_job_ended`, `DROP TABLE idle_pick_seq`,
		`DROP TABLE idle_pick_events`, `DROP TABLE idle_pick_delivery`} {
		if _, err := db.Exec(q); err != nil {
			t.Fatal(err)
		}
	}
	_ = db.Close()
	return reopenForIdle(t, m, now)
}

// #165 F3: on upgrade the finished history is covered (no note for it), and a completion whose job-done
// line was still pending across the upgrade gets its note on its own line.
func TestIdlePickUpgradePendingCompletionGetsNote(t *testing.T) {
	f := newIdleFixture(t)
	f.m.Cfg.IdlePick.Enabled = false
	f.finish(t, 0)
	f.notify(t, 1) // delivered, never evaluated: history
	pending := f.finish(t, 10)
	m2 := resetIdleSchema(t, f.m, f.now)
	m2.Cfg.IdlePick.Enabled = true
	notifyAt(t, m2, f.now, f.t0.Add(20*time.Second))
	got := tmuxLines(t, f.logf)
	if len(got) != 2 || !strings.HasPrefix(got[1], m2.DoneLineFor(get(t, m2, pending.ID))+" · idle: 2 ready tasks: ") {
		t.Fatalf("pending completion across the upgrade: %q", got)
	}
	if strings.Contains(got[0], "idle:") {
		t.Fatalf("history noted: %q", got)
	}
}

// #165 F3: with every finished job delivered at the upgrade, nothing is noted for the history, and the
// first completion after it is a new epoch with its note.
func TestIdlePickUpgradeHistoryIsNotNoted(t *testing.T) {
	f := newIdleFixture(t)
	f.m.Cfg.IdlePick.Enabled = false
	f.finish(t, 0)
	f.notify(t, 1)
	m2 := resetIdleSchema(t, f.m, f.now)
	m2.Cfg.IdlePick.Enabled = true
	for _, sec := range []int{2, 3600} {
		notifyAt(t, m2, f.now, f.t0.Add(time.Duration(sec)*time.Second))
	}
	if got := tmuxLines(t, f.logf); len(got) != 1 {
		t.Fatalf("history produced a delivery: %q", got)
	}
	finishAt(t, m2, f.now, f.t0.Add(2*time.Hour), nil)
	notifyAt(t, m2, f.now, f.t0.Add(2*time.Hour+time.Second))
	if got := tmuxLines(t, f.logf); len(got) != 2 || !strings.Contains(got[1], " · idle: ") {
		t.Fatalf("first completion after the upgrade: %q", got)
	}
}

// gateTmux installs a fake tmux that logs its keystrokes like the default one and can be held: while
// <dir>/hold-probe exists the pane-mode probe (display-message) waits for <dir>/release, and while
// <dir>/hold-send exists the first send-keys waits for it too. <dir>/waiting names what is waiting.
func gateTmux(t *testing.T) (dir string) {
	t.Helper()
	dir = t.TempDir()
	t.Setenv("FAKE_TMUX_LOG", filepath.Join(dir, "tmux.log"))
	script := fmt.Sprintf(`#!/bin/bash
for a in "$@"; do
  if [[ $a == display-message ]]; then
    if [[ -e %[1]s/hold-probe ]]; then echo probe > %[1]s/waiting; while [[ ! -e %[1]s/release ]]; do sleep 0.02; done; fi
    echo 0
    exit 0
  fi
done
if [[ $2 == -t && $4 == -l && -e %[1]s/hold-send ]]; then echo send > %[1]s/waiting; while [[ ! -e %[1]s/release ]]; do sleep 0.02; done; fi
printf '%%s|' "$@" >> "$FAKE_TMUX_LOG"
printf '\n' >> "$FAKE_TMUX_LOG"
`, dir)
	bin := filepath.Join(dir, "tmux")
	if err := os.WriteFile(bin, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("AGENT_JOBS_TMUX_BIN", bin)
	return dir
}

func waitForFile(t *testing.T, path string) {
	t.Helper()
	for i := 0; i < 500; i++ {
		if _, err := os.Stat(path); err == nil {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("%s never appeared", path)
}

// #165 F2: a job enqueued while the delivery waits on tmux (the pane-mode probe, after the final-check
// window the old hook covered) still cancels the note.
func TestIdlePickEnqueueDuringTmuxWaitCancelsNote(t *testing.T) {
	f := newIdleFixture(t)
	dir := gateTmux(t)
	f.logf = filepath.Join(dir, "tmux.log")
	if err := os.WriteFile(filepath.Join(dir, "hold-probe"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	j := f.finish(t, 0)
	done := make(chan struct{})
	go func() {
		defer close(done)
		*f.now = f.t0.Add(time.Second)
		if err := f.m.notifyPending(); err != nil {
			t.Error(err)
		}
	}()
	waitForFile(t, filepath.Join(dir, "waiting"))
	late := enqueue(t, f.m, "late", nil)
	if err := os.WriteFile(filepath.Join(dir, "release"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	<-done
	got := tmuxLines(t, f.logf)
	if len(got) != 1 || got[0] != f.m.DoneLineFor(get(t, f.m, j.ID)) {
		t.Fatalf("stale note typed with job %s queued: %q", late.ID, got)
	}
}

// #165 F2: an enqueue that comes while the note is being typed waits for the keystrokes, so the note
// never lands after the job it was cancelled by.
func TestIdlePickEnqueueWaitsForTypedNote(t *testing.T) {
	f := newIdleFixture(t)
	dir := gateTmux(t)
	f.logf = filepath.Join(dir, "tmux.log")
	if err := os.WriteFile(filepath.Join(dir, "hold-send"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	f.finish(t, 0)
	delivered := make(chan struct{})
	go func() {
		defer close(delivered)
		*f.now = f.t0.Add(time.Second)
		if err := f.m.notifyPending(); err != nil {
			t.Error(err)
		}
	}()
	waitForFile(t, filepath.Join(dir, "waiting"))
	enqueued := make(chan struct{})
	go func() {
		defer close(enqueued)
		enqueue(t, f.m, "late", nil)
	}()
	select {
	case <-enqueued:
		t.Fatal("enqueue committed while the idle note was being typed")
	case <-time.After(300 * time.Millisecond):
	}
	if err := os.WriteFile(filepath.Join(dir, "release"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	<-delivered
	<-enqueued
	if got := tmuxLines(t, f.logf); len(got) != 1 || !strings.Contains(got[0], " · idle: ") {
		t.Fatalf("lines: %q", got)
	}
}

// #165 F3: Unix-second stamps must not merge epochs: a job enqueued and cancelled in the very second
// the previous epoch was evaluated is a new epoch, and its delivery carries the note after the cooldown.
func TestIdlePickSameSecondEpochsStaySeparate(t *testing.T) {
	f := newIdleFixture(t)
	f.finish(t, 0)
	f.notify(t, 1) // epoch 1 evaluated and noted at t0+1s
	f.finish(t, 1) // epoch 2: enqueued and cancelled in that same second
	f.notify(t, 3600)
	got := tmuxLines(t, f.logf)
	if len(got) != 2 || !strings.Contains(got[1], " · idle: 2 ready tasks: ") {
		t.Fatalf("second epoch lost its note: %q", got)
	}
	// The same after a restart between the epochs: a later epoch past the cooldown is noted once.
	m2 := reopenForIdle(t, f.m, f.now)
	finishAt(t, m2, f.now, f.t0.Add(2*time.Hour), nil)
	notifyAt(t, m2, f.now, f.t0.Add(2*time.Hour+time.Second))
	notifyAt(t, m2, f.now, f.t0.Add(2*time.Hour+2*time.Second))
	if got := tmuxLines(t, f.logf); len(got) != 3 || !strings.Contains(got[2], " · idle: ") {
		t.Fatalf("third epoch after a restart: %q", got)
	}
}

// failIdleWrites makes every insert into the idle_pick_events table fail (or, with fail false, work
// again) from a second connection to the daemon's database.
func failIdleWrites(t *testing.T, m *Manager, fail bool) {
	t.Helper()
	db, err := sql.Open("sqlite", filepath.Join(m.StateDir, "jobs.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	q := `DROP TRIGGER IF EXISTS fail_idle_events`
	if fail {
		q = `CREATE TRIGGER fail_idle_events BEFORE INSERT ON idle_pick_events BEGIN SELECT RAISE(ABORT, 'disk full'); END`
	}
	if _, err := db.Exec(q); err != nil {
		t.Fatal(err)
	}
}

// #165 F4: a note whose bookkeeping cannot be written is not forgotten: the job stays pending (the
// line is retried like any failed acknowledgement), and once the write works the note is recorded with
// the job's mark in one step, so a restart never says it a second time.
func TestIdlePickPersistenceFailureKeepsAcknowledgement(t *testing.T) {
	f := newIdleFixture(t)
	failIdleWrites(t, f.m, true)
	j := f.finish(t, 0)
	*f.now = f.t0.Add(time.Second)
	if err := f.m.notifyPending(); err == nil || !strings.Contains(err.Error(), "idle pick event") {
		t.Fatalf("the write failure was swallowed: %v", err)
	}
	if got := get(t, f.m, j.ID); got.NotifiedAt != nil {
		t.Fatal("job marked notified although the note could not be recorded")
	}
	failIdleWrites(t, f.m, false)
	f.notify(t, 2)
	if got := get(t, f.m, j.ID); got.NotifiedAt == nil {
		t.Fatal("job not marked notified after the write works again")
	}
	if at, err := f.m.Store.LastIdlePickEvent(store.IdleNote); err != nil || at == nil {
		t.Fatalf("note not recorded: %v %v", at, err)
	}
	lines := len(tmuxLines(t, f.logf))
	m2 := reopenForIdle(t, f.m, f.now)
	for _, sec := range []int{3, 7 * 3600, 30 * 3600} {
		*f.now = f.t0.Add(time.Duration(sec) * time.Second)
		if err := m2.notifyPending(); err != nil {
			t.Fatal(err)
		}
	}
	if got := tmuxLines(t, f.logf); len(got) != lines {
		t.Fatalf("delivered epoch said again after a restart: %q", got)
	}
}

// #165 F4: the note's metadata is saved before typing and the retry acknowledges that record, never a
// recomputation. The note is typed, the acknowledgement fails, the daemon restarts, a job is enqueued,
// the line is retried: the note still counts for the cooldown, so the next epoch inside it carries none.
func TestIdlePickRetryAcknowledgesStoredNote(t *testing.T) {
	f := newIdleFixture(t)
	failIdleWrites(t, f.m, true)
	j := f.finish(t, 0)
	*f.now = f.t0.Add(time.Second)
	if err := f.m.notifyPending(); err == nil {
		t.Fatal("the acknowledgement failure was swallowed")
	}
	if got := tmuxLines(t, f.logf); len(got) != 1 || !strings.Contains(got[0], " · idle: ") {
		t.Fatalf("first attempt: %q", got)
	}
	epoch, err := f.m.Store.FinishSeq()
	if err != nil {
		t.Fatal(err)
	}
	if rec, err := f.m.Store.IdleDeliveryFor(j.ID); err != nil || rec == nil || rec.Epoch != epoch || !strings.HasPrefix(rec.Note, "idle: ") {
		t.Fatalf("delivery record before the retry: %+v %v", rec, err)
	}
	failIdleWrites(t, f.m, false)
	m2 := reopenForIdle(t, f.m, f.now)
	late := enqueue(t, m2, "late", nil) // the queue is no longer empty when the line is retried
	notifyAt(t, m2, f.now, f.t0.Add(2*time.Second))
	if got := get(t, m2, j.ID); got.NotifiedAt == nil {
		t.Fatal("retry did not acknowledge the line")
	}
	if n, err := m2.Store.LastIdlePickEpoch(store.IdleNote); err != nil || n != epoch {
		t.Fatalf("the delivered note was not recorded: epoch %d, want %d (%v)", n, epoch, err)
	}
	if rec, err := m2.Store.IdleDeliveryFor(j.ID); err != nil || rec != nil {
		t.Fatalf("delivery record kept after the acknowledgement: %+v %v", rec, err)
	}
	if err := m2.Cancel(late.ID); err != nil {
		t.Fatal(err)
	}
	notifyAt(t, m2, f.now, f.t0.Add(4*time.Second))
	got := tmuxLines(t, f.logf)
	if len(got) != 3 || strings.Contains(got[1], "idle:") || strings.Contains(got[2], "idle:") {
		t.Fatalf("lines after the retry (no note on a busy queue, none inside the cooldown): %q", got)
	}
}

// #193: a restart with notify off acknowledges a failed coalesced delivery one member at a time.
func TestIdlePickNotifyOffClearsStoredCoalescedDelivery(t *testing.T) {
	f := newIdleFixture(t)
	f.m.Cfg.NotifyCoalesceSeconds = 10
	a := f.finish(t, 0)
	d := f.finish(t, 1)
	failIdleWrites(t, f.m, true)
	*f.now = f.t0.Add(20 * time.Second)
	if err := f.m.notifyPending(); err == nil || !strings.Contains(err.Error(), "idle pick event") {
		t.Fatalf("expected idle event write failure: %v", err)
	}
	if got := tmuxLines(t, f.logf); len(got) != 1 || !strings.Contains(got[0], "2 jobs:") || !strings.Contains(got[0], " · idle: ") {
		t.Fatalf("first coalesced delivery: %q", got)
	}
	batch := batchKey([]*store.Job{a, d})
	if saved, err := f.m.Store.IdleDeliveryFor(batch); err != nil || saved == nil {
		t.Fatalf("coalesced delivery record missing: %+v %v", saved, err)
	}
	m := reopenForIdle(t, f.m, f.now)
	m.Cfg.NotifyTmuxTarget = ""
	notifyAt(t, m, f.now, f.t0.Add(21*time.Second))
	for _, id := range []string{a.ID, d.ID} {
		if get(t, m, id).NotifiedAt == nil {
			t.Fatalf("%s not acknowledged with notify off", id)
		}
	}
	if saved, err := m.Store.IdleDeliveryFor(batch); err != nil || saved != nil {
		t.Fatalf("coalesced record kept after split acknowledgements: %+v %v", saved, err)
	}
	notifyAt(t, m, f.now, f.t0.Add(22*time.Second))
	if got := tmuxLines(t, f.logf); len(got) != 1 {
		t.Fatalf("notify-off retry sent another line: %q", got)
	}
}

// #193: an owner-waits acknowledgement delivered before an overdue retry must preserve the
// retry's saved idle note, even after a restart and while another job keeps the queue busy.
func TestIdlePickUnrelatedAcknowledgementKeepsStoredNote(t *testing.T) {
	f := newIdleFixture(t)
	f.m.Cfg.NotifyCoalesceSeconds = 10
	a := f.finish(t, 0)
	failIdleWrites(t, f.m, true)
	*f.now = f.t0.Add(10 * time.Second)
	if err := f.m.notifyPending(); err == nil || !strings.Contains(err.Error(), "idle pick event") {
		t.Fatalf("expected idle event write failure: %v", err)
	}
	if got := tmuxLines(t, f.logf); len(got) != 1 || !strings.Contains(got[0], " · idle: ") {
		t.Fatalf("first delivery did not carry an idle note: %q", got)
	}
	if get(t, f.m, a.ID).NotifiedAt != nil {
		t.Fatal("A acknowledged despite the event write failure")
	}
	saved, err := f.m.Store.IdleDeliveryFor(a.ID)
	if err != nil || saved == nil || saved.Note == "" {
		t.Fatalf("A's delivery record missing: %+v %v", saved, err)
	}

	m := reopenForIdle(t, f.m, f.now)
	failIdleWrites(t, m, false)
	b := finishAt(t, m, f.now, f.t0.Add(11*time.Second), func(r *StartRequest) { r.OwnerWaits = true })
	c := enqueue(t, m, "active C", nil)
	// Coalescing sends B first, then retries overdue A in the same tick.
	notifyAt(t, m, f.now, f.t0.Add(12*time.Second))
	got := tmuxLines(t, f.logf)
	if len(got) != 3 || !strings.Contains(got[1], b.ID) || !strings.HasSuffix(got[1], OwnerWaitsMarker) ||
		!strings.Contains(got[2], a.ID) || strings.Contains(got[2], "idle:") {
		t.Fatalf("expected B's line before A's retry on a busy queue: %q", got)
	}
	for _, id := range []string{a.ID, b.ID} {
		if get(t, m, id).NotifiedAt == nil {
			t.Fatalf("%s not acknowledged after writes recovered", id)
		}
	}
	if epoch, err := m.Store.LastIdlePickEpoch(store.IdleNote); err != nil || epoch != saved.Epoch {
		t.Fatalf("A's delivered note was lost after B's acknowledgement: epoch %d, want %d (%v)", epoch, saved.Epoch, err)
	}
	if rec, err := m.Store.IdleDeliveryFor(a.ID); err != nil || rec != nil {
		t.Fatalf("A's record kept after its acknowledgement: %+v %v", rec, err)
	}
	if n, err := m.Store.CountIdlePickEvents(f.t0, store.IdleNote); err != nil || n != 1 {
		t.Fatalf("expected one recorded idle note: %d %v", n, err)
	}

	if err := m.Cancel(c.ID); err != nil {
		t.Fatal(err)
	}
	notifyAt(t, m, f.now, f.t0.Add(22*time.Second))
	got = tmuxLines(t, f.logf)
	if len(got) != 4 || !strings.Contains(got[3], c.ID) || strings.Contains(got[3], "idle:") {
		t.Fatalf("C's completion must respect A's idle cooldown: %q", got)
	}
}

// #165: config values are checked at load; absent means off with the documented defaults.
func TestIdlePickConfig(t *testing.T) {
	for _, body := range []string{
		`{"idle_pick": {"cooldown_seconds": -1}}`,
		`{"idle_pick": {"max_wakes_per_day": -1}}`,
		`{"idle_pick": {"max_wakes_per_day": 1000}}`,
	} {
		if _, err := loadConfigBody(t, body); err == nil || !strings.Contains(err.Error(), "idle_pick.") {
			t.Fatalf("%s: %v", body, err)
		}
	}
	c, err := loadConfigBody(t, `{}`)
	if err != nil || c.IdlePick.Enabled || c.IdlePick.Cooldown() != DefaultIdleCooldown ||
		c.IdlePick.MaxWakes() != DefaultIdleMaxWakes {
		t.Fatalf("defaults: %+v %v", c.IdlePick, err)
	}
	c, err = loadConfigBody(t, `{"idle_pick": {"enabled": true, "cooldown_seconds": 30, "max_wakes_per_day": 3}}`)
	if err != nil || !c.IdlePick.Enabled || c.IdlePick.Cooldown() != 30*time.Second || c.IdlePick.MaxWakes() != 3 {
		t.Fatalf("values: %+v %v", c.IdlePick, err)
	}
}

// #165: a failed delivery spends nothing: the retry composes the note again and says it once.
func TestIdlePickFailedDeliveryRetried(t *testing.T) {
	f := newIdleFixture(t)
	good := os.Getenv("AGENT_JOBS_TMUX_BIN")
	t.Setenv("AGENT_JOBS_TMUX_BIN", "/usr/bin/false")
	f.finish(t, 0)
	f.notify(t, 1)
	if got := tmuxLines(t, f.logf); len(got) != 0 {
		t.Fatalf("delivered through a failing tmux: %q", got)
	}
	if at, err := f.m.Store.LastIdlePickEvent(store.IdleNote); err != nil || at != nil {
		t.Fatalf("note recorded without a delivery: %v %v", at, err)
	}
	t.Setenv("AGENT_JOBS_TMUX_BIN", good)
	f.notify(t, 2)
	f.notify(t, 3)
	if got := tmuxLines(t, f.logf); len(got) != 1 || strings.Count(got[0], "idle:") != 1 {
		t.Fatalf("after the retry: %q", got)
	}
}

// #165: a queue epoch that was evaluated (an event covers its epoch number) is spent: a line for a job
// of that epoch that still comes out adds nothing.
func TestIdlePickEvaluatedEpochSpent(t *testing.T) {
	f := newIdleFixture(t)
	f.m.Cfg.IdlePick.CooldownSeconds = 1 // the cooldown must not be what holds the note back
	f.finish(t, 0)
	epoch, err := f.m.Store.FinishSeq()
	if err != nil || epoch == 0 {
		t.Fatalf("epoch %d %v", epoch, err)
	}
	if err := f.m.Store.AddIdlePickEvent(store.IdleNote, f.t0.Add(5*time.Second), epoch); err != nil {
		t.Fatal(err)
	}
	f.notify(t, 10)
	if got := tmuxLines(t, f.logf); len(got) != 1 || strings.Contains(got[0], "idle:") {
		t.Fatalf("spent epoch said again: %q", got)
	}
}
