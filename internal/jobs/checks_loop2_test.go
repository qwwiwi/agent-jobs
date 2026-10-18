package jobs

import (
	"strings"
	"testing"
	"time"

	"github.com/qwwiwi/agent-jobs/internal/store"
)

// missedTexts are the MissedChecks lines (status, system_doctor) of m.
func missedTexts(t *testing.T, m *Manager) []string {
	t.Helper()
	list, err := m.MissedChecks()
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, v := range list {
		out = append(out, CheckViewText(v))
	}
	return out
}

// Fix loop 2: a mark belongs to the earliest expected time whose window holds it, whatever was
// evaluated before. Hourly, grace 60, the only mark at 10:59 and the daemon back at 12:00:01:
// E1=10:00 is ok (10:59 is in its grace), E2=11:00 is missed.
func TestCheckRestartJudgesEveryExpectedTime(t *testing.T) {
	m, now, logf := scheduleManager(t, time.UTC) // 2026-10-19 09:00 UTC
	addCheck(t, m, "hourly", "0 * * * *", 60)
	markAt(t, m, now, time.Date(2026, 10, 19, 10, 59, 0, 0, time.UTC), "hourly")
	*now = time.Date(2026, 10, 19, 12, 0, 1, 0, time.UTC)
	watch(t, m)
	got := missedLines(t, logf)
	want := "[jobs:check-missed] hourly expected 2026-10-19 11:00 UTC (no mark within 60m)"
	if len(got) != 1 || got[0] != want {
		t.Fatalf("lines %q, want [%q]", got, want)
	}
}

// Ownership does not depend on the insertion order: with grace 58 the 10:58 mark belongs to
// E1=10:00 and the 10:59 one to E2=11:00, also when 10:59 is stored first.
func TestCheckReversedInsertionOrder(t *testing.T) {
	m, now, logf := scheduleManager(t, time.UTC)
	addCheck(t, m, "hourly", "0 * * * *", 58)
	markAt(t, m, now, time.Date(2026, 10, 19, 10, 59, 0, 0, time.UTC), "hourly")
	markAt(t, m, now, time.Date(2026, 10, 19, 10, 58, 0, 0, time.UTC), "hourly")
	*now = time.Date(2026, 10, 19, 10, 58, 1, 0, time.UTC)
	watch(t, m)
	*now = time.Date(2026, 10, 19, 11, 58, 1, 0, time.UTC)
	watch(t, m)
	if got := missedLines(t, logf); len(got) != 0 {
		t.Fatalf("false miss: %q", got)
	}
}

// One valid mark and 10000 later ones (a looping marker): the valid mark is never pruned before
// its expected time is judged, so no false miss.
func TestCheckManyLaterMarksNeverFalseMiss(t *testing.T) {
	m, now, logf := scheduleManager(t, time.UTC)
	addCheck(t, m, "daily", "0 10 * * *", 30)
	markAt(t, m, now, time.Date(2026, 10, 19, 9, 30, 0, 0, time.UTC), "daily")
	base := time.Date(2026, 10, 19, 10, 31, 0, 0, time.UTC)
	for i := 0; i < 10000; i++ {
		markAt(t, m, now, base.Add(time.Duration(i)*time.Second), "daily")
	}
	*now = time.Date(2026, 10, 19, 13, 30, 0, 0, time.UTC)
	watch(t, m)
	if got := missedLines(t, logf); len(got) != 0 {
		t.Fatalf("false miss after many marks: %q", got)
	}
	if v, _ := m.CheckViews(); v[0].State != CheckOK {
		t.Fatalf("state %q, want ok", v[0].State)
	}
}

// An every-minute check with an unavailable target: each missed time keeps its own attempts, so
// a newer miss never resets an older alert's attempts and the oldest reaches failed.
func TestCheckMinutelyUnavailableTargetKeepsAttempts(t *testing.T) {
	m, now, _ := scheduleManager(t, time.UTC)
	addCheck(t, m, "dense", "* * * * *", 1)
	t.Setenv("AGENT_JOBS_TMUX_BIN", "/usr/bin/false")
	for i := 0; i < 40; i++ {
		*now = time.Date(2026, 10, 19, 9, 2+i, 1, 0, time.UTC)
		watch(t, m)
	}
	txt := missedTexts(t, m)
	if len(txt) != 1 || !strings.Contains(txt[0], "not delivered after 5 attempts") {
		t.Fatalf("no alert reached failed: %q", txt)
	}
	ck, _ := m.Store.GetCheck("dense")
	rows, err := m.Store.CheckAlerts(ck.ID)
	if err != nil || len(rows) != 40 {
		t.Fatalf("one row per missed minute: %d %v", len(rows), err)
	}
	// The oldest row (09:01) made its five attempts at 09:02, :03, :05, :09 and :17.
	if r := rows[0]; r.State != store.AlertFailed || r.Attempts != MaxCheckAlertAttempts ||
		!r.Expected.Equal(time.Date(2026, 10, 19, 9, 1, 0, 0, time.UTC)) {
		t.Fatalf("oldest row: %+v", r)
	}
	for _, r := range rows {
		if r.Attempts < 1 || r.Attempts > MaxCheckAlertAttempts {
			t.Fatalf("row attempts: %+v", r)
		}
	}
}

// A failed alert stays visible in MissedChecks after a later expected time is ok, until check_set
// acknowledges it.
func TestCheckFailedAlertVisibleAfterHealthyExpected(t *testing.T) {
	m, now, _ := scheduleManager(t, time.UTC)
	addCheck(t, m, "daily", "0 10 * * *", 30)
	good := "AGENT_JOBS_TMUX_BIN"
	t.Setenv(good, "/usr/bin/false")
	for i := 0; i < 30; i++ {
		*now = time.Date(2026, 10, 19, 10, 30+i, 1, 0, time.UTC)
		watch(t, m)
	}
	markAt(t, m, now, time.Date(2026, 10, 20, 9, 50, 0, 0, time.UTC), "daily")
	*now = time.Date(2026, 10, 20, 10, 30, 1, 0, time.UTC)
	watch(t, m)
	if v, _ := m.CheckViews(); v[0].State != CheckOK {
		t.Fatalf("state %q, want ok", v[0].State)
	}
	txt := missedTexts(t, m)
	if len(txt) != 1 || !strings.Contains(txt[0], "not delivered after 5 attempts") {
		t.Fatalf("failed alert hidden by a healthy E: %q", txt)
	}
	if _, err := m.SetCheck("daily", "", 45, nil); err != nil {
		t.Fatal(err)
	}
	if txt := missedTexts(t, m); len(txt) != 0 {
		t.Fatalf("check_set did not acknowledge the failed alert: %q", txt)
	}
}

// A cron or grace change re-arms the check: a mark made before the arm time never satisfies an
// expected time of the new configuration.
func TestCheckReArmIgnoresPreArmMarks(t *testing.T) {
	m, now, logf := scheduleManager(t, time.UTC)
	addCheck(t, m, "hourly", "0 * * * *", 60)
	markAt(t, m, now, time.Date(2026, 10, 19, 9, 30, 0, 0, time.UTC), "hourly")
	*now = time.Date(2026, 10, 19, 10, 0, 1, 0, time.UTC)
	watch(t, m) // E=09:00 ok
	markAt(t, m, now, time.Date(2026, 10, 19, 10, 10, 0, 0, time.UTC), "hourly")
	*now = time.Date(2026, 10, 19, 10, 20, 0, 0, time.UTC)
	if _, err := m.SetCheck("hourly", "", 120, nil); err != nil {
		t.Fatal(err)
	}
	*now = time.Date(2026, 10, 19, 13, 0, 1, 0, time.UTC)
	watch(t, m)
	got := missedLines(t, logf)
	want := "[jobs:check-missed] hourly expected 2026-10-19 11:00 UTC (no mark within 120m)"
	if len(got) != 1 || got[0] != want {
		t.Fatalf("lines %q, want [%q]", got, want)
	}
}

// After a re-arm the old configuration's evaluation watermark never prunes a mark that belongs to
// an expected time of the new one.
func TestCheckReArmKeepsNewEvidence(t *testing.T) {
	m, now, logf := scheduleManager(t, time.UTC)
	addCheck(t, m, "hourly", "0 * * * *", 60)
	markAt(t, m, now, time.Date(2026, 10, 19, 9, 30, 0, 0, time.UTC), "hourly")
	*now = time.Date(2026, 10, 19, 10, 0, 1, 0, time.UTC)
	watch(t, m) // E=09:00 ok, watermark 09:00
	*now = time.Date(2026, 10, 19, 10, 5, 0, 0, time.UTC)
	if _, err := m.SetCheck("hourly", "", 180, nil); err != nil {
		t.Fatal(err)
	}
	markAt(t, m, now, time.Date(2026, 10, 19, 10, 10, 0, 0, time.UTC), "hourly") // owner E=11:00
	for i := 0; i < 5; i++ {
		markAt(t, m, now, time.Date(2026, 10, 19, 14, 30+i, 0, 0, time.UTC), "hourly") // later windows
	}
	*now = time.Date(2026, 10, 19, 14, 0, 1, 0, time.UTC) // E=11:00 + 180m passed
	watch(t, m)
	if got := missedLines(t, logf); len(got) != 0 {
		t.Fatalf("new evidence pruned: %q", got)
	}
}

// The fence: an attempt paused after its claim (before tmux.lock) whose rows were claimed again and
// delivered by another daemon types nothing when it resumes; nor does one whose rows were only
// claimed again (the newer attempt types them).
func TestCheckStaleAttemptDoesNotSend(t *testing.T) {
	for _, delivered := range []bool{true, false} {
		m, now, logf := scheduleManager(t, time.UTC)
		addCheck(t, m, "daily", "0 10 * * *", 30)
		other, err := Open("test")
		if err != nil {
			t.Fatal(err)
		}
		other.Cfg.NotifyTmuxTarget, other.Loc, other.Log = "fake-session", time.UTC, m.Log
		other.Now = func() time.Time { return *now }
		*now = time.Date(2026, 10, 19, 10, 30, 1, 0, time.UTC)
		if err := m.watchChecks(); err != nil {
			t.Fatal(err)
		}
		stale, err := m.claimCheckAlerts() // attempt 1; this daemon pauses here
		if err != nil || stale == "" {
			t.Fatalf("claim: %q %v", stale, err)
		}
		*now = now.Add(2 * time.Minute) // past attempt 1's backoff
		fresh, err := other.claimCheckAlerts()
		if err != nil || fresh == "" {
			t.Fatalf("second claim: %q %v", fresh, err)
		}
		if delivered {
			if err := other.sendCheckAlerts(fresh); err != nil {
				t.Fatal(err)
			}
		}
		if err := m.sendCheckAlerts(stale); err != nil { // the paused attempt resumes
			t.Fatal(err)
		}
		if !delivered {
			if got := missedLines(t, logf); len(got) != 0 {
				t.Fatalf("the stale attempt typed: %q", got)
			}
			if err := other.sendCheckAlerts(fresh); err != nil {
				t.Fatal(err)
			}
		}
		if got := missedLines(t, logf); len(got) != 1 {
			t.Fatalf("delivered=%t: lines %q", delivered, got)
		}
		ck, _ := m.Store.GetCheck("daily")
		if ck.AlertState != store.AlertDelivered || ck.AlertAttempts != 2 || ck.AlertsPending != 0 {
			t.Fatalf("delivered=%t: row %+v", delivered, ck)
		}
		other.Close()
	}
}

// Without a notify target every attempt fails the same way: five attempts, then failed, shown in
// MissedChecks. A disabled check's pending alert is still delivered.
func TestCheckNotifyOffFailsAfterFiveAttempts(t *testing.T) {
	m, now, logf := scheduleManager(t, time.UTC)
	addCheck(t, m, "x", "0 10 * * *", 30)
	addCheck(t, m, "y", "0 10 * * *", 30)
	m.Cfg.NotifyTmuxTarget = ""
	for i := 0; i < 20; i++ {
		*now = time.Date(2026, 10, 19, 10, 30+i, 1, 0, time.UTC)
		watch(t, m)
	}
	ck, _ := m.Store.GetCheck("x")
	if ck.AlertState != store.AlertFailed || ck.AlertAttempts != MaxCheckAlertAttempts || !strings.Contains(ck.LastAlertError, "notify off") {
		t.Fatalf("row: %+v", ck)
	}
	if txt := missedTexts(t, m); len(txt) != 2 || !strings.Contains(txt[0], "x missed") {
		t.Fatalf("missed: %q", txt)
	}
	// A new miss of y, then y is disabled: its pending alert is still typed.
	m.Cfg.NotifyTmuxTarget = "fake-session"
	*now = time.Date(2026, 10, 20, 10, 30, 1, 0, time.UTC)
	if err := m.watchChecks(); err != nil {
		t.Fatal(err)
	}
	off := false
	if _, err := m.SetCheck("y", "", 0, &off); err != nil {
		t.Fatal(err)
	}
	if err := m.deliverCheckAlerts(); err != nil {
		t.Fatal(err)
	}
	if got := missedLines(t, logf); len(got) != 1 || !strings.Contains(got[0], "y expected 2026-10-20 10:00 UTC") {
		t.Fatalf("lines %q", got)
	}
}

// A window stores at most store.MaxMarksPerWindow marks; later ones answer "ok, already marked",
// and a mark of the next window is stored again.
func TestCheckMarkAdmission(t *testing.T) {
	m, now, _ := scheduleManager(t, time.UTC)
	addCheck(t, m, "loop", "0 10 * * *", 30)
	for i := 0; i < store.MaxMarksPerWindow+3; i++ {
		*now = time.Date(2026, 10, 19, 9, 10, i, 0, time.UTC)
		c, err := m.MarkCheck("loop", "", "")
		want := store.MarkStored
		if i >= store.MaxMarksPerWindow {
			want = store.MarkNotStored
		}
		if err != nil || c.MarkResult != want || !c.LastMarkAt.Equal(*now) {
			t.Fatalf("mark %d: %+v %v, want %q", i, c, err, want)
		}
	}
	*now = time.Date(2026, 10, 19, 10, 30, 1, 0, time.UTC) // the next window
	if c, err := m.MarkCheck("loop", "", ""); err != nil || c.MarkResult != store.MarkStored {
		t.Fatalf("next window: %+v %v", c, err)
	}
	if v, _ := m.CheckViews(); v[0].State != CheckOK {
		t.Fatalf("state %q", v[0].State)
	}
}

// Ownership around the arm time and DST: a check created at 09:00:00 does not expect 09:00 (no
// time to report), the first window starts at the arm time, and a mark in the repeated hour
// belongs to the single expected 02:30.
func TestCheckOwnerWindows(t *testing.T) {
	m, now, _ := scheduleManager(t, time.UTC) // 09:00:00
	ck := addCheck(t, m, "hourly", "0 * * * *", 60)
	s, err := m.specOf(ck)
	if err != nil {
		t.Fatal(err)
	}
	at := func(h, mi, sec int) time.Time { return time.Date(2026, 10, 19, h, mi, sec, 0, time.UTC) }
	for _, c := range []struct {
		mark, owner time.Time
	}{
		{at(9, 0, 0), at(10, 0, 0)},
		{at(9, 59, 0), at(10, 0, 0)},
		{at(11, 0, 0), at(10, 0, 0)},
		{at(11, 0, 1), at(11, 0, 0)},
		{at(12, 0, 0), at(11, 0, 0)},
	} {
		if e, ok := s.owner(c.mark); !ok || !e.Equal(c.owner) {
			t.Fatalf("owner(%v) = %v %t, want %v", c.mark, e, ok, c.owner)
		}
	}
	if _, ok := s.owner(at(8, 59, 59)); ok {
		t.Fatal("a mark before the arm time has an owner")
	}
	if lo, hi := s.window(at(10, 0, 0)); !lo.Equal(*now) || !hi.Equal(at(11, 0, 0)) {
		t.Fatalf("first window [%v, %v]", lo, hi)
	}
	if lo, hi := s.window(at(11, 0, 0)); !lo.Equal(at(11, 0, 1)) || !hi.Equal(at(12, 0, 0)) {
		t.Fatalf("second window [%v, %v]", lo, hi)
	}
	loc := berlin(t)
	m2, now2, _ := scheduleManager(t, loc)
	*now2 = time.Date(2026, 10, 24, 12, 0, 0, 0, loc)
	fold := addCheck(t, m2, "fold", "30 2 * * *", 20)
	s2, err := m2.specOf(fold)
	if err != nil {
		t.Fatal(err)
	}
	second := time.Date(2026, 10, 25, 1, 40, 0, 0, time.UTC) // 02:40 CET, the repeated hour
	if e, ok := s2.owner(second); !ok || !e.Equal(time.Date(2026, 10, 26, 2, 30, 0, 0, loc)) {
		t.Fatalf("a mark after 02:30 CEST + 20m belongs to the next day: %v %t", e, ok)
	}
	first := time.Date(2026, 10, 25, 0, 45, 0, 0, time.UTC) // 02:45 CEST
	if e, ok := s2.owner(first); !ok || !e.Equal(time.Date(2026, 10, 25, 0, 30, 0, 0, time.UTC)) {
		t.Fatalf("02:45 CEST: %v %t", e, ok)
	}
}
