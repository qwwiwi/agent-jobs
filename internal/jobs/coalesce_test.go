package jobs

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/qwwiwi/agent-jobs/internal/store"
)

// coalesceClock pins the manager's clock to a value the test moves by hand.
func coalesceClock(m *Manager) *time.Time {
	now := time.Unix(1_800_000_000, 0)
	m.Now = func() time.Time { return now }
	return &now
}

// finishAt cancels a queued job with the clock at at, so its finished_at is at.
func finishAt(t *testing.T, m *Manager, now *time.Time, at time.Time, mod func(*StartRequest)) *store.Job {
	t.Helper()
	*now = at
	j := enqueue(t, m, "brief", mod)
	if err := m.Cancel(j.ID); err != nil {
		t.Fatal(err)
	}
	return get(t, m, j.ID)
}

func notifyAt(t *testing.T, m *Manager, now *time.Time, at time.Time) {
	t.Helper()
	*now = at
	if err := m.notifyPending(); err != nil {
		t.Fatal(err)
	}
}

// donePart is a job's part of a coalesced line: its own job-done line without the prefix.
func donePart(t *testing.T, m *Manager, id string) string {
	t.Helper()
	line, ok := strings.CutPrefix(m.DoneLineFor(get(t, m, id)), doneLinePrefix)
	if !ok {
		t.Fatalf("job-done line without its prefix: %q", line)
	}
	return line
}

// #162: three jobs finishing inside the window reach the session as one line, after the window,
// with each job's fields as on its own line; all three are marked notified by that one delivery.
func TestCoalesceThreeJobsOneLine(t *testing.T) {
	m := newManager(t)
	logf := notifyTo(t, m)
	m.Cfg.NotifyCoalesceSeconds = 120
	now := coalesceClock(m)
	t0 := *now
	a := finishAt(t, m, now, t0, nil)
	b := finishAt(t, m, now, t0.Add(30*time.Second), func(r *StartRequest) { r.Title = "second" })
	notifyAt(t, m, now, t0.Add(40*time.Second))
	c := finishAt(t, m, now, t0.Add(60*time.Second), func(r *StartRequest) { r.Title = "third" })
	notifyAt(t, m, now, t0.Add(179*time.Second)) // the last one finished 119 s ago: still held
	if got := tmuxLines(t, logf); len(got) != 0 {
		t.Fatalf("lines inside the window: %q", got)
	}
	want := fmt.Sprintf("[jobs:job-done] 3 jobs: %s; %s; %s",
		donePart(t, m, a.ID), donePart(t, m, b.ID), donePart(t, m, c.ID))
	notifyAt(t, m, now, t0.Add(180*time.Second))
	got := tmuxLines(t, logf)
	if len(got) != 1 || got[0] != want {
		t.Fatalf("coalesced line:\n got %q\nwant %q", got, want)
	}
	for _, j := range []*store.Job{a, b, c} {
		if get(t, m, j.ID).NotifiedAt == nil {
			t.Fatalf("job %s not marked notified", j.ID)
		}
	}
	notifyAt(t, m, now, t0.Add(10*time.Minute))
	if got := tmuxLines(t, logf); len(got) != 1 {
		t.Fatalf("delivered again: %q", got)
	}
}

// #162: an owner-waits job is never held: its own line (today's format, with the marker) goes out on
// the first tick, while a plain job that finished before it keeps waiting for its window.
func TestCoalesceOwnerWaitsAtOnce(t *testing.T) {
	m := newManager(t)
	logf := notifyTo(t, m)
	m.Cfg.NotifyCoalesceSeconds = 120
	now := coalesceClock(m)
	t0 := *now
	plain := finishAt(t, m, now, t0, nil)
	w := finishAt(t, m, now, t0.Add(10*time.Second), func(r *StartRequest) { r.OwnerWaits = true })
	notifyAt(t, m, now, t0.Add(10*time.Second))
	got := tmuxLines(t, logf)
	if len(got) != 1 || got[0] != m.DoneLineFor(get(t, m, w.ID)) || !strings.HasSuffix(got[0], OwnerWaitsMarker) {
		t.Fatalf("owner-waits line: %q", got)
	}
	if get(t, m, plain.ID).NotifiedAt != nil {
		t.Fatal("the plain job was delivered inside its window")
	}
	if list, _, _ := m.OwnerPending(); len(list) != 1 || list[0].JobID != w.ID {
		t.Fatalf("owner pending: %+v", list)
	}
	// One held job alone goes out in today's single-job format.
	notifyAt(t, m, now, t0.Add(120*time.Second))
	got = tmuxLines(t, logf)
	if len(got) != 2 || got[1] != m.DoneLineFor(get(t, m, plain.ID)) {
		t.Fatalf("held single line: %q", got)
	}
}

// #162: jobs that keep finishing inside the window extend it, but never past
// notify_coalesce_max_seconds from the first held line.
func TestCoalesceMaxWindow(t *testing.T) {
	m := newManager(t)
	logf := notifyTo(t, m)
	m.Cfg.NotifyCoalesceSeconds = 60
	m.Cfg.NotifyCoalesceMaxSeconds = 150
	now := coalesceClock(m)
	t0 := *now
	var ids []string
	for _, sec := range []int{0, 50, 100, 140} { // each one inside the last one's window
		at := t0.Add(time.Duration(sec) * time.Second)
		ids = append(ids, finishAt(t, m, now, at, nil).ID)
		notifyAt(t, m, now, at)
	}
	notifyAt(t, m, now, t0.Add(149*time.Second)) // the last one's window runs to 200 s, the cap to 150 s
	if got := tmuxLines(t, logf); len(got) != 0 {
		t.Fatalf("lines before the cap: %q", got)
	}
	notifyAt(t, m, now, t0.Add(150*time.Second))
	got := tmuxLines(t, logf)
	if len(got) != 1 || !strings.HasPrefix(got[0], "[jobs:job-done] 4 jobs: ") {
		t.Fatalf("capped window: %q", got)
	}
	for _, id := range ids {
		if !strings.Contains(got[0], id) {
			t.Fatalf("job %s missing from %q", id, got[0])
		}
	}
}

// #162: the default (0) keeps one line per job in today's exact format, on the first tick.
func TestCoalesceOffOneLinePerJob(t *testing.T) {
	m := newManager(t)
	logf := notifyTo(t, m)
	now := coalesceClock(m)
	t0 := *now
	a := finishAt(t, m, now, t0, nil)
	b := finishAt(t, m, now, t0.Add(time.Second), nil)
	notifyAt(t, m, now, t0.Add(time.Second))
	got := tmuxLines(t, logf)
	if len(got) != 2 || got[0] != m.DoneLineFor(get(t, m, a.ID)) || got[1] != m.DoneLineFor(get(t, m, b.ID)) {
		t.Fatalf("lines with coalescing off: %q", got)
	}
	if !strings.HasPrefix(got[0], "[jobs:job-done] job "+a.ID+" test job cancelled · ") {
		t.Fatalf("single-job format changed: %q", got[0])
	}
}

// #162: a held line lives only in its notified_at-less row: a daemon that restarts inside the window
// (a fresh manager on the same state) delivers it on its first tick past the window.
func TestCoalesceSurvivesRestart(t *testing.T) {
	m := newManager(t)
	logf := notifyTo(t, m)
	m.Cfg.NotifyCoalesceSeconds = 120
	now := coalesceClock(m)
	t0 := *now
	a := finishAt(t, m, now, t0, nil)
	b := finishAt(t, m, now, t0.Add(5*time.Second), nil)
	notifyAt(t, m, now, t0.Add(10*time.Second))
	if got := tmuxLines(t, logf); len(got) != 0 {
		t.Fatalf("lines inside the window: %q", got)
	}
	m2, err := Open("test")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = m2.Close() })
	m2.Cfg.NotifyTmuxTarget = "fake-session"
	m2.Cfg.NotifyCoalesceSeconds = 120
	m2.Now = m.Now
	notifyAt(t, m2, now, t0.Add(125*time.Second))
	got := tmuxLines(t, logf)
	if len(got) != 1 || !strings.HasPrefix(got[0], "[jobs:job-done] 2 jobs: ") ||
		!strings.Contains(got[0], a.ID) || !strings.Contains(got[0], b.ID) {
		t.Fatalf("after restart: %q", got)
	}
}

// #162: a failed delivery of a coalesced line keeps every job in it pending for the next tick.
func TestCoalesceFailedDeliveryKeepsAll(t *testing.T) {
	m := newManager(t)
	notifyTo(t, m)
	t.Setenv("AGENT_JOBS_TMUX_BIN", "false")
	m.Cfg.NotifyCoalesceSeconds = 30
	now := coalesceClock(m)
	t0 := *now
	a := finishAt(t, m, now, t0, nil)
	b := finishAt(t, m, now, t0.Add(time.Second), nil)
	notifyAt(t, m, now, t0.Add(time.Minute))
	for _, id := range []string{a.ID, b.ID} {
		if get(t, m, id).NotifiedAt != nil {
			t.Fatalf("job %s marked notified after a failed delivery", id)
		}
	}
}

// Config: negative coalescing values are refused at load.
func TestCoalesceConfigValidation(t *testing.T) {
	for _, body := range []string{`{"notify_coalesce_seconds": -1}`, `{"notify_coalesce_max_seconds": -5}`} {
		if _, err := loadConfigBody(t, body); err == nil || !strings.Contains(err.Error(), "notify_coalesce") {
			t.Fatalf("%s: %v", body, err)
		}
	}
	c, err := loadConfigBody(t, `{"notify_coalesce_seconds": 90}`)
	if err != nil || c.NotifyCoalesceSeconds != 90 || c.NotifyCoalesceMax() != DefaultNotifyCoalesceMax {
		t.Fatalf("defaults: %+v %v", c.NotifyCoalesceMax(), err)
	}
}

// #162: a backlog of more than maxCoalescedJobs due jobs goes out as several lines, oldest first;
// owner-waits jobs come first, each alone.
func TestCoalesceBatches(t *testing.T) {
	m := newManager(t)
	m.Cfg.NotifyCoalesceSeconds = 60
	t0 := time.Unix(1_800_000_000, 0)
	var jobs []*store.Job
	for i := 0; i < maxCoalescedJobs+2; i++ {
		at := t0.Add(time.Duration(i) * time.Second)
		jobs = append(jobs, &store.Job{ID: fmt.Sprintf("j%02d", i), FinishedAt: &at, OwnerWaits: i == 5})
	}
	if got := m.notifyBatches(jobs, t0.Add(time.Minute)); len(got) != 1 || got[0][0].ID != "j05" {
		t.Fatalf("inside the window only the owner-waits job: %v", got)
	}
	got := m.notifyBatches(jobs, t0.Add(2*time.Minute))
	if len(got) != 3 || len(got[0]) != 1 || got[0][0].ID != "j05" || len(got[1]) != maxCoalescedJobs ||
		got[1][0].ID != "j00" || len(got[2]) != 1 || got[2][0].ID != "j11" {
		t.Fatalf("batches: %v", got)
	}
	m.Cfg.NotifyCoalesceSeconds = 0
	if got := m.notifyBatches(jobs, t0); len(got) != len(jobs) {
		t.Fatalf("coalescing off: %d batches", len(got))
	}
}

// #162: a window that expired before a later completion is not revived by it. With window 30 s, A at
// t0 and B at t0+60 s, the tick at t0+60 s delivers A (its own window ended at t0+30 s) and holds B.
func TestCoalesceExpiredWindowNotRevived(t *testing.T) {
	m := newManager(t)
	m.Cfg.NotifyCoalesceSeconds = 30
	m.Cfg.NotifyCoalesceMaxSeconds = 300
	t0 := time.Unix(1_800_000_000, 0)
	atB := t0.Add(60 * time.Second)
	a := &store.Job{ID: "ja", FinishedAt: &t0}
	b := &store.Job{ID: "jb", FinishedAt: &atB}
	got := m.notifyBatches([]*store.Job{a, b}, atB)
	if len(got) != 1 || len(got[0]) != 1 || got[0][0].ID != "ja" {
		t.Fatalf("want only A delivered, B held: %v", got)
	}
	// B alone is delivered once its own window ends.
	got = m.notifyBatches([]*store.Job{b}, atB.Add(30*time.Second))
	if len(got) != 1 || len(got[0]) != 1 || got[0][0].ID != "jb" {
		t.Fatalf("B after its window: %v", got)
	}
	// Two jobs inside one window stay one group, delivered together at the window's end.
	c := &store.Job{ID: "jc", FinishedAt: &atB}
	at := t0.Add(50 * time.Second)
	d := &store.Job{ID: "jd", FinishedAt: &at}
	got = m.notifyBatches([]*store.Job{d, c}, atB.Add(30*time.Second))
	if len(got) != 1 || len(got[0]) != 2 {
		t.Fatalf("one window, two jobs: %v", got)
	}
}
