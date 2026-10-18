package jobs

import (
	"os"
	"strings"
	"testing"
	"time"
)

// within runs f and fails the test when it does not return within d (a hung search or delivery).
func within(t *testing.T, d time.Duration, what string, f func()) {
	t.Helper()
	done := make(chan struct{})
	go func() {
		defer close(done)
		f()
	}()
	select {
	case <-done:
	case <-time.After(d):
		t.Fatalf("%s did not return within %s", what, d)
	}
}

// Codex review (PR #25): across the Berlin fall-back fold, a restart in the middle of it and two
// managers ticking the same store fire "*/30 2 * * *" exactly twice (02:00 and 02:30 CEST).
func TestScheduleDSTFoldRestartAndTwoManagers(t *testing.T) {
	loc := berlin(t)
	m, now, logf := scheduleManager(t, loc)
	half := addSchedule(t, m, ScheduleRequest{Cron: "*/30 2 * * *", Prompt: "half"})
	second := func() *Manager {
		m2, err := Open("test")
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { m2.Close() })
		m2.Cfg.NotifyTmuxTarget, m2.Loc, m2.Log = "fake-session", loc, m.Log
		return m2
	}
	other := second()
	var at time.Time
	other.Now = func() time.Time { return at }
	start := time.Date(2026, 10, 24, 23, 59, 0, 0, time.UTC) // 01:59 CEST
	for i := 0; i <= 3*60; i++ {
		at = start.Add(time.Duration(i) * time.Minute).In(loc)
		*now = at
		if i == 50 { // 02:49 CEST: the first manager restarts
			m.Close()
			m = second()
			m.Now = func() time.Time { return *now }
		}
		fire(t, m)
		fire(t, other)
	}
	var n int
	for _, l := range tmuxLines(t, logf) {
		n += strings.Count(l, "schedule "+itoa(half.ID)+": half")
	}
	if n != 2 {
		t.Fatalf("fold with a restart and two managers: %d firings, want 2 (02:00, 02:30 CEST)", n)
	}
}

// Codex review (PR #25): schedule_list never advertises a firing the watermark will suppress.
// Between the two passes of the fold (after 02:30 CEST fired) the next firing is the next day;
// before the fold it is the first occurrence (02:00 CEST), not the second (02:00 CET).
func TestScheduleNextFireRespectsWatermark(t *testing.T) {
	loc := berlin(t)
	m, now, _ := scheduleManager(t, loc)
	sc := addSchedule(t, m, ScheduleRequest{Cron: "*/30 2 * * *", Prompt: "half"})
	*now = time.Date(2026, 10, 24, 23, 30, 0, 0, time.UTC).In(loc) // 01:30 CEST
	if n := m.NextFire(sc); n == nil || !n.Equal(time.Date(2026, 10, 25, 0, 0, 0, 0, time.UTC)) {
		t.Fatalf("before the fold: next %v, want 02:00 CEST", n)
	}
	for _, u := range []time.Time{time.Date(2026, 10, 25, 0, 0, 0, 0, time.UTC), time.Date(2026, 10, 25, 0, 30, 0, 0, time.UTC)} {
		*now = u.In(loc)
		fire(t, m)
	}
	row, err := m.Store.GetSchedule(sc.ID)
	if err != nil {
		t.Fatal(err)
	}
	want := time.Date(2026, 10, 26, 2, 0, 0, 0, loc)
	for _, u := range []time.Time{
		time.Date(2026, 10, 25, 0, 40, 0, 0, time.UTC), // 02:40 CEST
		time.Date(2026, 10, 25, 1, 10, 0, 0, time.UTC), // 02:10 CET
	} {
		*now = u.In(loc)
		if n := m.NextFire(row); n == nil || !n.Equal(want) {
			t.Fatalf("at %s: next %v, want %s", now, n, want)
		}
	}
}

// Codex review (PR #25): America/Havana springs forward at midnight (2027-03-14 00:00 does not
// exist and normalises back to 03-13 23:00); add, set and list must not hang on such a day.
func TestScheduleHavanaMidnightGapNeverHangs(t *testing.T) {
	loc, err := time.LoadLocation("America/Havana")
	if err != nil {
		t.Fatal(err)
	}
	m, now, _ := scheduleManager(t, loc)
	*now = time.Date(2027, 3, 13, 22, 0, 0, 0, loc)
	within(t, 5*time.Second, "add, set and list", func() {
		sc, err := m.AddSchedule(ScheduleRequest{Cron: "0 0 * * *", Prompt: "midnight"})
		if err != nil {
			t.Error(err)
			return
		}
		if _, err := m.SetSchedule(sc.ID, "30 0 * * *", "", nil); err != nil {
			t.Error(err)
			return
		}
		list, err := m.ListSchedules()
		if err != nil || len(list) != 1 {
			t.Errorf("list: %v %v", list, err)
			return
		}
		if n := m.NextFire(list[0]); n == nil || !n.Equal(time.Date(2027, 3, 15, 0, 30, 0, 0, loc)) {
			t.Errorf("next %v, want 2027-03-15 00:30", n)
		}
	})
}

// Codex review (PR #25): a stalled tmux ends a session schedule's delivery at TmuxDeliveryTimeout
// (tmux.lock, process group killed), so the tick releases the lifecycle lock in time; the error is
// recorded and the firing is not retried.
func TestScheduleStalledTmuxBounded(t *testing.T) {
	m, now, _ := scheduleManager(t, time.UTC)
	sc := addSchedule(t, m, ScheduleRequest{Cron: "0 9 * * *", Prompt: "x;"})
	_, calls := stalledTmux(t)
	*now = time.Date(2026, 10, 19, 9, 0, 0, 0, time.UTC)
	within(t, 5*time.Second, "tick with a stalled tmux", func() {
		if err := m.Tick(); err != nil {
			t.Error(err)
		}
	})
	within(t, 2*time.Second, "lifecycle lock after the tick", func() {
		unlock, err := m.lock()
		if err != nil {
			t.Error(err)
			return
		}
		unlock()
	})
	row, err := m.Store.GetSchedule(sc.ID)
	if err != nil {
		t.Fatal(err)
	}
	if row.LastFiredAt == nil || !strings.Contains(row.LastError, "timed out") {
		t.Fatalf("row: %+v", row)
	}
	*now = time.Date(2026, 10, 19, 9, 0, 40, 0, time.UTC)
	within(t, 5*time.Second, "second tick", func() { fire(t, m) })
	if b, _ := os.ReadFile(calls); strings.Count(string(b), "call") != 1 {
		t.Fatalf("tmux called %d times, want 1 (no retry)", strings.Count(string(b), "call"))
	}
}
