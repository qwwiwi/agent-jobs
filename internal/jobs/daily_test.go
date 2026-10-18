package jobs

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/qwwiwi/agent-jobs/internal/store"
)

// dailyCounters replaces both daily steps with counters (the procs step finds one job ghost).
func dailyCounters(m *Manager, pruneErr error) (prunes, procs *atomic.Int32) {
	prunes, procs = new(atomic.Int32), new(atomic.Int32)
	m.dailyPruneFn = func(context.Context) (PruneReport, error) {
		prunes.Add(1)
		return PruneReport{Jobs: []PruneJob{{ID: "a", Archived: []PruneEntry{{Bytes: 5}}}}, Bytes: 100, ArchivedBytes: 5}, pruneErr
	}
	m.dailyProcsFn = func(context.Context) (ProcsReport, error) {
		procs.Add(1)
		return ProcsReport{Supported: true, Ghosts: []Ghost{{Kind: GhostJob}, {Kind: GhostSuspect}}}, nil
	}
	return prunes, procs
}

func at(day, hour, minute int) time.Time {
	return time.Date(2027, 3, day, hour, minute, 0, 0, time.Local)
}

func startAt(t *testing.T, m *Manager, now time.Time) bool {
	t.Helper()
	m.Now = func() time.Time { return now }
	ok := m.StartDaily(context.Background())
	m.WaitDaily()
	return ok
}

// Reference «Раз в сутки»: once per local day, at the first tick after daily_hour; a missed day
// runs once, not caught up.
func TestDailyOncePerDay(t *testing.T) {
	m := newManager(t)
	prunes, procs := dailyCounters(m, nil)
	for _, c := range []struct {
		now  time.Time
		want bool
	}{
		{at(10, 3, 59), false}, // before the hour
		{at(10, 4, 0), true},
		{at(10, 4, 1), false},
		{at(10, 23, 59), false},
		{at(11, 0, 30), false}, // the next day, before the hour
		{at(11, 4, 5), true},
		{at(11, 12, 0), false},
		{at(14, 9, 0), true}, // three days missed: one run, no catch-up
		{at(14, 9, 1), false},
	} {
		if got := startAt(t, m, c.now); got != c.want {
			t.Fatalf("%s: started %v, want %v", c.now, got, c.want)
		}
	}
	if prunes.Load() != 3 || procs.Load() != 3 {
		t.Fatalf("prune ran %d times, doctor procs %d, want 3 each", prunes.Load(), procs.Load())
	}
	d, err := m.Daily()
	if err != nil {
		t.Fatal(err)
	}
	r := d.Result
	if d.LastDay != "2027-03-14" || d.Running || d.FinishedAt == nil || r == nil || r.JobsPruned != 1 ||
		r.BytesFreed != 100 || r.JobsArchived != 1 || r.ArchivedBytes != 5 || r.Ghosts != 2 || r.JobGhosts != 1 ||
		r.Suspects != 1 || r.PruneError != "" || r.ProcsError != "" {
		t.Fatalf("daily status: %+v %+v", d, r)
	}
}

// daily_hour moves the run; 0 runs right after midnight.
func TestDailyHourConfig(t *testing.T) {
	m := newManager(t)
	dailyCounters(m, nil)
	h := 0
	m.Cfg.DailyHour = &h
	if !startAt(t, m, at(10, 0, 0)) {
		t.Fatal("daily_hour 0: not started at midnight")
	}
	h = 22
	if startAt(t, m, at(11, 21, 59)) || !startAt(t, m, at(11, 22, 0)) {
		t.Fatal("daily_hour 22")
	}
	for _, bad := range []string{`{"daily_hour": 24}`, `{"daily_hour": -1}`} {
		dir := t.TempDir()
		if err := os.WriteFile(filepath.Join(dir, "config.json"), []byte(bad), 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err := LoadConfig(dir); err == nil || !strings.Contains(err.Error(), "daily_hour") {
			t.Fatalf("%s: %v", bad, err)
		}
	}
}

// The claimed day lives in the store: a restarted daemon does not repeat it.
func TestDailyRestartDoesNotRepeat(t *testing.T) {
	m := newManager(t)
	prunes, _ := dailyCounters(m, nil)
	if !startAt(t, m, at(10, 5, 0)) {
		t.Fatal("first run not started")
	}
	m2, err := Open("test")
	if err != nil {
		t.Fatal(err)
	}
	defer m2.Close()
	m2.Log = m.Log
	prunes2, _ := dailyCounters(m2, nil)
	if startAt(t, m2, at(10, 6, 0)) {
		t.Fatal("restarted daemon repeated the day")
	}
	if !startAt(t, m2, at(11, 5, 0)) || prunes.Load() != 1 || prunes2.Load() != 1 {
		t.Fatalf("next day: prunes %d/%d", prunes.Load(), prunes2.Load())
	}
}

func TestDailyOff(t *testing.T) {
	m := newManager(t)
	prunes, procs := dailyCounters(m, nil)
	m.Cfg.DailyOff = true
	if startAt(t, m, at(10, 5, 0)) || prunes.Load() != 0 || procs.Load() != 0 {
		t.Fatal("daily_off still ran")
	}
	if d, err := m.Daily(); err != nil || d.Enabled || d.LastDay != "" {
		t.Fatalf("daily_off: %+v %v (the day must not be claimed)", d, err)
	}
	// A shutting-down daemon never starts one either.
	m.Cfg.DailyOff = false
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	m.Now = func() time.Time { return at(10, 5, 0) }
	if m.StartDaily(ctx) || prunes.Load() != 0 {
		t.Fatal("started while shutting down")
	}
}

// A failing prune does not stop doctor procs, and the tick goes on.
func TestDailyPruneFailureKeepsProcsAndTick(t *testing.T) {
	m := newManager(t)
	_, procs := dailyCounters(m, errors.New("disk on fire"))
	if !startAt(t, m, at(10, 5, 0)) {
		t.Fatal("not started")
	}
	d, err := m.Daily()
	if err != nil || d.Result == nil || d.Result.PruneError != "disk on fire" || procs.Load() != 1 || d.Result.Ghosts != 2 {
		t.Fatalf("after a failed prune: %+v %v procs=%d", d.Result, err, procs.Load())
	}
	// A panicking step is recovered and recorded; the process (the daemon) survives.
	m.dailyPruneFn = func(context.Context) (PruneReport, error) { panic("bug") }
	if !startAt(t, m, at(11, 5, 0)) {
		t.Fatal("not started")
	}
	if d, err = m.Daily(); err != nil || d.Result == nil || !strings.Contains(d.Result.Error, "bug") || d.Running {
		t.Fatalf("after a panic: %+v %v", d.Result, err)
	}
	j := enqueue(t, m, "FAKE_SLEEP=0", nil)
	m.Now = time.Now
	if err := m.Tick(); err != nil {
		t.Fatal(err)
	}
	if got, _ := m.Store.Get(j.ID); got.Status == store.StatusQueued {
		t.Fatal("tick did not launch after a failed daily")
	}
}

// Two daily runs never overlap, and a run in progress never blocks a tick from launching jobs.
func TestDailyNeverOverlaps(t *testing.T) {
	m := newManager(t)
	release, entered := make(chan struct{}), make(chan struct{}, 4)
	var running, maxRunning, runs atomic.Int32
	m.dailyPruneFn = func(context.Context) (PruneReport, error) {
		n := running.Add(1)
		defer running.Add(-1)
		if n > maxRunning.Load() {
			maxRunning.Store(n)
		}
		runs.Add(1)
		entered <- struct{}{}
		<-release
		return PruneReport{}, nil
	}
	m.dailyProcsFn = func(context.Context) (ProcsReport, error) { return ProcsReport{}, nil }
	now := at(10, 5, 0)
	m.Now = func() time.Time { return now }
	if !m.StartDaily(context.Background()) {
		t.Fatal("not started")
	}
	<-entered
	// The next day comes while the first run still hangs: no second run, and the day stays unclaimed.
	now = at(11, 5, 0)
	if m.StartDaily(context.Background()) {
		t.Fatal("a second daily run started while one was in progress")
	}
	if d, _ := m.Daily(); d.LastDay != "2027-03-10" || !d.Running {
		t.Fatalf("while running: %+v", d)
	}
	m.Now = time.Now
	j := enqueue(t, m, "FAKE_SLEEP=0", nil)
	if err := m.Tick(); err != nil {
		t.Fatal(err)
	}
	if got, _ := m.Store.Get(j.ID); got.Status == store.StatusQueued {
		t.Fatal("tick did not launch while the daily run was in progress")
	}
	close(release)
	m.WaitDaily()
	m.Now = func() time.Time { return now }
	if !m.StartDaily(context.Background()) {
		t.Fatal("the day after the overlap did not run once the first run ended")
	}
	m.WaitDaily()
	if runs.Load() != 2 || maxRunning.Load() != 1 {
		t.Fatalf("runs %d, max concurrent %d", runs.Load(), maxRunning.Load())
	}
}

// Each step is time-boxed; a shutdown during prune skips doctor procs.
func TestDailyTimeboxAndShutdown(t *testing.T) {
	m := newManager(t)
	old := DailyStepTimeout
	DailyStepTimeout = 100 * time.Millisecond
	t.Cleanup(func() { DailyStepTimeout = old })
	m.dailyPruneFn = func(ctx context.Context) (PruneReport, error) {
		<-ctx.Done()
		return PruneReport{}, ctx.Err()
	}
	hang := make(chan struct{})
	m.dailyProcsFn = func(context.Context) (ProcsReport, error) { <-hang; return ProcsReport{}, nil }
	if !startAt(t, m, at(10, 5, 0)) {
		t.Fatal("not started")
	}
	d, err := m.Daily()
	if err != nil || d.Result == nil || !strings.Contains(d.Result.PruneError, "deadline") ||
		!strings.Contains(d.Result.ProcsError, "deadline") || d.Running {
		t.Fatalf("time-boxed: %+v %v", d.Result, err)
	}
	// The abandoned scan holds the daily lock until it ends (TestDailyAbandonedScanHoldsLock).
	close(hang)
	for !m.dailyMu.TryLock() {
		time.Sleep(10 * time.Millisecond)
	}
	m.dailyMu.Unlock()
	// Shutdown: the daemon's ctx ends during prune; doctor procs is skipped and WaitDaily returns.
	DailyStepTimeout = time.Minute
	var procs atomic.Int32
	m.dailyProcsFn = func(context.Context) (ProcsReport, error) { procs.Add(1); return ProcsReport{}, nil }
	ctx, cancel := context.WithCancel(context.Background())
	m.Now = func() time.Time { return at(11, 5, 0) }
	if !m.StartDaily(ctx) {
		t.Fatal("not started")
	}
	cancel()
	m.WaitDaily()
	if d, err = m.Daily(); err != nil || d.Result == nil || !strings.Contains(d.Result.ProcsError, "shutting down") || procs.Load() != 0 {
		t.Fatalf("shutdown: %+v %v", d.Result, err)
	}
}

// The real steps: prune cleans an old job's folder for real, doctor procs only reports.
func TestDailyRealRun(t *testing.T) {
	m := newManager(t)
	finished := time.Now().Add(-72 * time.Hour)
	j := &store.Job{ID: "261001-000000-aaaa", Agent: m.Agent, Title: "old", Kind: "general", Brief: "b",
		Status: store.StatusSucceeded, Model: "sonnet", MaxTurns: 80, MaxBudgetUSD: 10, TimeoutMinutes: 45,
		CreatedAt: finished.Add(-time.Hour), StartedAt: &finished, FinishedAt: &finished, CleanedAt: &finished}
	if err := m.Store.Insert(j); err != nil {
		t.Fatal(err)
	}
	dir := m.JobDir(j.ID)
	if err := os.MkdirAll(filepath.Join(dir, "work", "node_modules"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "work", "node_modules", "big.bin"), make([]byte, 4096), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "report.md"), []byte("## Summary\nok\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	m.Now = time.Now
	now := time.Now()
	m.Cfg.DailyHour = new(int) // 0: due at any hour today
	if !m.StartDaily(context.Background()) {
		t.Fatal("not started")
	}
	m.WaitDaily()
	if _, err := os.Stat(filepath.Join(dir, "work")); !os.IsNotExist(err) {
		t.Fatalf("work dir not pruned: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "report.md")); err != nil {
		t.Fatalf("report gone: %v", err)
	}
	d, err := m.Daily()
	if err != nil || d.Result == nil || d.Result.JobsPruned != 1 || d.Result.BytesFreed < 4096 || d.Result.PruneError != "" ||
		d.LastDay != now.Format(time.DateOnly) {
		t.Fatalf("real run: %+v %+v %v", d, d.Result, err)
	}
}

// A panic in the doctor procs scan (its own goroutine) is recovered: the daemon survives, the
// summary records the error, the prune result is kept, and the next tick still launches jobs.
func TestDailyProcsPanicRecovered(t *testing.T) {
	m := newManager(t)
	prunes, _ := dailyCounters(m, nil)
	m.dailyProcsFn = func(context.Context) (ProcsReport, error) { panic("scan bug") }
	if !startAt(t, m, at(10, 5, 0)) {
		t.Fatal("not started")
	}
	d, err := m.Daily()
	if err != nil || d.Result == nil || d.Running || !strings.Contains(d.Result.ProcsError, "doctor procs panicked: scan bug") ||
		prunes.Load() != 1 || d.Result.JobsPruned != 1 || d.Result.BytesFreed != 100 {
		t.Fatalf("after a procs panic: %+v %+v %v", d, d.Result, err)
	}
	j := enqueue(t, m, "FAKE_SLEEP=0", nil)
	m.Now = time.Now
	if err := m.Tick(); err != nil {
		t.Fatal(err)
	}
	if got, _ := m.Store.Get(j.ID); got.Status == store.StatusQueued {
		t.Fatal("tick did not launch after a procs panic")
	}
}

// A doctor procs scan abandoned at its time-box keeps the daily lock until it really ends, so the
// next day's run never overlaps it; WaitDaily (shutdown) does not wait for it.
func TestDailyAbandonedScanHoldsLock(t *testing.T) {
	m := newManager(t)
	old := DailyStepTimeout
	DailyStepTimeout = 100 * time.Millisecond
	t.Cleanup(func() { DailyStepTimeout = old })
	dailyCounters(m, nil)
	hang, scanned := make(chan struct{}), make(chan struct{})
	m.dailyProcsFn = func(context.Context) (ProcsReport, error) {
		defer close(scanned)
		<-hang
		return ProcsReport{}, nil
	}
	if !startAt(t, m, at(10, 5, 0)) { // startAt returns after WaitDaily: shutdown is not held up
		t.Fatal("not started")
	}
	if d, _ := m.Daily(); d.Result == nil || !strings.Contains(d.Result.ProcsError, "deadline") {
		t.Fatalf("abandoned scan: %+v", d.Result)
	}
	if startAt(t, m, at(11, 5, 0)) {
		t.Fatal("the next day's run started while the abandoned scan still ran")
	}
	close(hang)
	<-scanned
	deadline := time.Now().Add(5 * time.Second)
	for !startAt(t, m, at(11, 5, 0)) {
		if time.Now().After(deadline) {
			t.Fatal("the lock was not released after the abandoned scan ended")
		}
		time.Sleep(10 * time.Millisecond)
	}
}
