package jobs

import (
	"context"
	"os"
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
	v, err := m.CreateMilestone(ctx, MilestoneCreate{Title: "Stage", Tasks: []MilestoneTaskInput{
		{Key: "T1", Title: "first"},
		{Key: "T2", Title: "second", DependsOn: dep("T1")},
		{Key: "T3", Title: "third"},
		{Key: "T4", Title: "fourth", DependsOn: dep("T2")},
		{Key: "T5", Title: "fifth", DependsOn: dep("T4")},
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
		if err := m3.idleRecovery(); err != nil {
			t.Fatal(err)
		}
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

// #165: the recovery scan finds an epoch whose delivery never got an idle evaluation (idle pick was
// off then) and says it once as a line of its own; later scans, and a queue that is not empty, say nothing.
func TestIdlePickRecoveryScanBusyQueue(t *testing.T) {
	f := newIdleFixture(t)
	f.m.Cfg.IdlePick.Enabled = false
	f.finish(t, 0)
	f.notify(t, 1)
	f.m.Cfg.IdlePick.Enabled = true
	busy := enqueue(t, f.m, "busy", nil)
	*f.now = f.t0.Add(2 * time.Minute)
	if err := f.m.idleRecovery(); err != nil {
		t.Fatal(err)
	}
	if got := tmuxLines(t, f.logf); len(got) != 1 {
		t.Fatalf("scan with a busy queue: %q", got)
	}
	if err := f.m.Cancel(busy.ID); err != nil {
		t.Fatal(err)
	}
	f.notify(t, 3) // the cancelled job's own line: it carries the note, so the epoch is evaluated
	if err := f.m.idleRecovery(); err != nil {
		t.Fatal(err)
	}
	got := tmuxLines(t, f.logf)
	if len(got) != 2 || !strings.Contains(got[1], " · idle: ") {
		t.Fatalf("lines: %q", got)
	}
}

func TestIdlePickRecoveryScanOwnLine(t *testing.T) {
	f := newIdleFixture(t)
	f.m.Cfg.IdlePick.Enabled = false
	f.finish(t, 0)
	f.notify(t, 1)
	f.m.Cfg.IdlePick.Enabled = true
	*f.now = f.t0.Add(2 * time.Minute)
	if err := f.m.idleRecovery(); err != nil {
		t.Fatal(err)
	}
	got := tmuxLines(t, f.logf)
	if len(got) != 2 || !strings.HasPrefix(got[1], "[jobs:idle] idle: 2 ready tasks: ") {
		t.Fatalf("recovery line: %q", got)
	}
	for _, d := range []time.Duration{3 * time.Minute, 7 * time.Hour, 30 * time.Hour} {
		*f.now = f.t0.Add(d)
		if err := f.m.idleRecovery(); err != nil {
			t.Fatal(err)
		}
	}
	if got := tmuxLines(t, f.logf); len(got) != 2 {
		t.Fatalf("scanned epoch said again: %q", got)
	}
}

// #165: recovery_scan_seconds -1 turns the scan off.
func TestIdlePickRecoveryOff(t *testing.T) {
	f := newIdleFixture(t)
	f.m.Cfg.IdlePick.Enabled = false
	f.finish(t, 0)
	f.notify(t, 1)
	f.m.Cfg.IdlePick.Enabled = true
	f.m.Cfg.IdlePick.RecoveryScanSeconds = -1
	*f.now = f.t0.Add(10 * time.Hour)
	if err := f.m.idleRecovery(); err != nil {
		t.Fatal(err)
	}
	if got := tmuxLines(t, f.logf); len(got) != 1 {
		t.Fatalf("scan ran while off: %q", got)
	}
}

// #165: config values are checked at load; absent means off with the documented defaults.
func TestIdlePickConfig(t *testing.T) {
	for _, body := range []string{
		`{"idle_pick": {"cooldown_seconds": -1}}`,
		`{"idle_pick": {"max_wakes_per_day": -1}}`,
		`{"idle_pick": {"max_wakes_per_day": 1000}}`,
		`{"idle_pick": {"recovery_scan_seconds": -2}}`,
	} {
		if _, err := loadConfigBody(t, body); err == nil || !strings.Contains(err.Error(), "idle_pick.") {
			t.Fatalf("%s: %v", body, err)
		}
	}
	c, err := loadConfigBody(t, `{}`)
	if err != nil || c.IdlePick.Enabled || c.IdlePick.Cooldown() != DefaultIdleCooldown ||
		c.IdlePick.MaxWakes() != DefaultIdleMaxWakes || c.IdlePick.RecoveryEvery() != DefaultIdleRecovery {
		t.Fatalf("defaults: %+v %v", c.IdlePick, err)
	}
	c, err = loadConfigBody(t, `{"idle_pick": {"enabled": true, "cooldown_seconds": 30, "max_wakes_per_day": 3, "recovery_scan_seconds": -1}}`)
	if err != nil || !c.IdlePick.Enabled || c.IdlePick.Cooldown() != 30*time.Second || c.IdlePick.MaxWakes() != 3 || c.IdlePick.RecoveryEvery() != 0 {
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

// #165: a queue epoch evaluated at or after its last finish is spent: a line for a job of that epoch
// that still comes out adds nothing.
func TestIdlePickEvaluatedEpochSpent(t *testing.T) {
	f := newIdleFixture(t)
	f.m.Cfg.IdlePick.CooldownSeconds = 1 // the cooldown must not be what holds the note back
	f.finish(t, 0)
	if err := f.m.Store.AddIdlePickEvent(store.IdleNote, f.t0.Add(5*time.Second)); err != nil {
		t.Fatal(err)
	}
	f.notify(t, 10)
	if got := tmuxLines(t, f.logf); len(got) != 1 || strings.Contains(got[0], "idle:") {
		t.Fatalf("spent epoch said again: %q", got)
	}
}
