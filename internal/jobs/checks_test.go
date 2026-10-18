package jobs

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/qwwiwi/agent-jobs/internal/store"
)

func addCheck(t *testing.T, m *Manager, name, cron string, grace int) *store.Check {
	t.Helper()
	c, err := m.AddCheck(name, cron, grace)
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func watch(t *testing.T, m *Manager) {
	t.Helper()
	if err := m.watchChecks(); err != nil {
		t.Fatal(err)
	}
	if err := m.deliverCheckAlerts(); err != nil {
		t.Fatal(err)
	}
}

func markAt(t *testing.T, m *Manager, now *time.Time, at time.Time, name string) {
	t.Helper()
	saved := *now
	*now = at
	if _, err := m.MarkCheck(name, "", ""); err != nil {
		t.Fatal(err)
	}
	*now = saved
}

func missedLines(t *testing.T, logf string) []string {
	t.Helper()
	var out []string
	for _, l := range tmuxLines(t, logf) {
		if strings.HasPrefix(l, CheckMissedTag) {
			out = append(out, l)
		}
	}
	return out
}

// A mark in (P, E+grace] (P the previous expected time) satisfies E; the boundary E+grace is
// inclusive and the watchdog looks at E only once E+grace has passed. A mark late in E's grace
// does not also cover the next expected time (each mark satisfies one expected time).
func TestCheckMarkMatchesExpectedWithGrace(t *testing.T) {
	m, now, logf := scheduleManager(t, time.UTC) // 2026-10-19 09:00 UTC
	addCheck(t, m, "backup", "0 10 * * *", 30)
	e := time.Date(2026, 10, 19, 10, 0, 0, 0, time.UTC)

	// Until one second past E+grace nothing is evaluated, even without a mark.
	*now = e.Add(30 * time.Minute)
	watch(t, m)
	if got := missedLines(t, logf); len(got) != 0 {
		t.Fatalf("alerted before E+grace: %q", got)
	}
	// A mark exactly at E+grace (also later within that second) counts.
	markAt(t, m, now, e.Add(30*time.Minute+900*time.Millisecond), "backup")
	*now = e.Add(30*time.Minute + time.Second)
	watch(t, m)
	if got := missedLines(t, logf); len(got) != 0 {
		t.Fatalf("a mark at E+grace was not counted: %q", got)
	}
	v, err := m.CheckViews()
	if err != nil || len(v) != 1 || v[0].State != CheckOK {
		t.Fatalf("views: %+v %v", v, err)
	}
	// The next day: yesterday's mark at E+grace belongs to yesterday's window only.
	e2 := e.Add(24 * time.Hour)
	*now = e2.Add(30*time.Minute + time.Second)
	watch(t, m)
	got := missedLines(t, logf)
	want := "[jobs:check-missed] backup expected 2026-10-20 10:00 UTC (no mark within 30m)"
	if len(got) != 1 || got[0] != want {
		t.Fatalf("lines %q, want [%q]", got, want)
	}
	// An early mark (after the previous window, before E) counts.
	markAt(t, m, now, e2.Add(time.Hour), "backup")
	*now = e2.Add(24*time.Hour + 30*time.Minute + time.Second)
	watch(t, m)
	if got := missedLines(t, logf); len(got) != 1 {
		t.Fatalf("an early mark was not counted: %q", got)
	}
}

// A mark one second after E+grace is too late: E is missed.
func TestCheckMarkAfterGraceIsLate(t *testing.T) {
	m, now, logf := scheduleManager(t, time.UTC)
	addCheck(t, m, "late", "0 10 * * *", 30)
	e := time.Date(2026, 10, 19, 10, 0, 0, 0, time.UTC)
	markAt(t, m, now, e.Add(30*time.Minute+time.Second), "late")
	*now = e.Add(30*time.Minute + 2*time.Second)
	watch(t, m)
	if got := missedLines(t, logf); len(got) != 1 {
		t.Fatalf("late mark: %q", got)
	}
}

// A missed expected time alerts once: later ticks, a restart and a second manager on the same
// store do not repeat it; the row and the views record it.
func TestCheckAlertsOnceAcrossRestartAndManagers(t *testing.T) {
	m, now, logf := scheduleManager(t, time.UTC)
	addCheck(t, m, "nightly", "0 10 * * *", 15)
	e := time.Date(2026, 10, 19, 10, 0, 0, 0, time.UTC)
	other, err := Open("test")
	if err != nil {
		t.Fatal(err)
	}
	defer other.Close()
	other.Cfg.NotifyTmuxTarget, other.Loc, other.Log = "fake-session", time.UTC, m.Log
	other.Now = func() time.Time { return *now }
	for i := 0; i < 10; i++ {
		*now = e.Add(15*time.Minute + time.Duration(i)*time.Minute)
		watch(t, m)
		watch(t, other)
	}
	m.Close()
	m2, err := Open("test")
	if err != nil {
		t.Fatal(err)
	}
	defer m2.Close()
	m2.Cfg.NotifyTmuxTarget, m2.Loc, m2.Log = "fake-session", time.UTC, other.Log
	m2.Now = func() time.Time { return *now }
	watch(t, m2)
	if got := missedLines(t, logf); len(got) != 1 {
		t.Fatalf("alerted %d times: %q", len(got), got)
	}
	ck, err := m2.Store.GetCheck("nightly")
	if err != nil || ck.LastMissedAt == nil || !ck.LastMissedAt.Equal(e) || ck.LastAlertError != "" ||
		ck.AlertState != store.AlertDelivered || ck.AlertAttempts != 1 {
		t.Fatalf("row: %+v %v", ck, err)
	}
	missed, err := m2.MissedChecks()
	if err != nil || len(missed) != 1 || missed[0].Name != "nightly" {
		t.Fatalf("missed: %+v %v", missed, err)
	}
	// A late mark keeps the time missed, with the late mark shown.
	if _, err := m2.MarkCheck("nightly", "done late", ""); err != nil {
		t.Fatal(err)
	}
	if v, _ := m2.CheckViews(); len(v) != 1 || v[0].State != CheckMissed || v[0].LateMarkAt == nil {
		t.Fatalf("views: %+v", v)
	}
}

// A check added now never alerts for an expected time before its creation.
func TestCheckNoAlertBeforeCreation(t *testing.T) {
	m, now, logf := scheduleManager(t, time.UTC)
	*now = time.Date(2026, 10, 19, 10, 0, 30, 0, time.UTC) // just after the 10:00 expected time
	addCheck(t, m, "fresh", "0 * * * *", 60)
	*now = time.Date(2026, 10, 19, 11, 0, 30, 0, time.UTC) // 10:00 + 60m passed
	watch(t, m)
	if got := missedLines(t, logf); len(got) != 0 {
		t.Fatalf("alerted for a time before creation: %q", got)
	}
	if v, _ := m.CheckViews(); v[0].State != CheckWaiting {
		t.Fatalf("state %q, want waiting", v[0].State)
	}
	*now = time.Date(2026, 10, 19, 12, 0, 1, 0, time.UTC) // 11:00 + 60m: after creation
	watch(t, m)
	if got := missedLines(t, logf); len(got) != 1 || !strings.Contains(got[0], "fresh expected 2026-10-19 11:00 UTC") {
		t.Fatalf("lines %q", got)
	}
	// Re-arming (a new grace) does not alert for times before it either.
	if _, err := m.SetCheck("fresh", "", 30, nil); err != nil {
		t.Fatal(err)
	}
	*now = time.Date(2026, 10, 19, 12, 31, 0, 0, time.UTC) // 12:00 + 30m, armed at 12:00:01
	watch(t, m)
	if got := missedLines(t, logf); len(got) != 1 {
		t.Fatalf("alerted for 12:00, before the re-arm: %q", got)
	}
	*now = time.Date(2026, 10, 19, 13, 31, 0, 0, time.UTC)
	watch(t, m)
	if got := missedLines(t, logf); len(got) != 2 || !strings.Contains(got[1], "fresh expected 2026-10-19 13:00 UTC (no mark within 30m)") {
		t.Fatalf("after the re-arm: %q", got)
	}
}

// After a long downtime every missed expected time is judged (10:00 on the first day through 08:00
// two days later) and the alert collapses them into one line, once.
func TestCheckDowntimeCollapsesToOneAlert(t *testing.T) {
	m, now, logf := scheduleManager(t, time.UTC)
	addCheck(t, m, "hourly", "0 * * * *", 10)
	*now = time.Date(2026, 10, 21, 9, 5, 0, 0, time.UTC) // two days later
	watch(t, m)
	got := missedLines(t, logf)
	if len(got) != 1 || got[0] != "[jobs:check-missed] hourly: 47 missed, latest 2026-10-21 08:00 UTC (no mark within 10m)" {
		t.Fatalf("lines %q", got)
	}
	watch(t, m)
	if got := missedLines(t, logf); len(got) != 1 {
		t.Fatalf("repeated: %q", got)
	}
}

// Several checks missed in the same tick go as one line; a disabled check and a marked one do not.
func TestCheckBatchesMissedIntoOneLine(t *testing.T) {
	m, now, logf := scheduleManager(t, time.UTC)
	addCheck(t, m, "a", "0 10 * * *", 0) // default grace 60
	addCheck(t, m, "b", "30 10 * * *", 30)
	addCheck(t, m, "c", "0 10 * * *", 60)
	addCheck(t, m, "d", "0 10 * * *", 60)
	off := false
	if _, err := m.SetCheck("c", "", 0, &off); err != nil {
		t.Fatal(err)
	}
	markAt(t, m, now, time.Date(2026, 10, 19, 10, 5, 0, 0, time.UTC), "d")
	*now = time.Date(2026, 10, 19, 11, 0, 1, 0, time.UTC)
	if err := m.Tick(); err != nil {
		t.Fatal(err)
	}
	got := missedLines(t, logf)
	want := "[jobs:check-missed] a expected 2026-10-19 10:00 UTC (no mark within 60m); b expected 2026-10-19 10:30 UTC (no mark within 30m)"
	if len(got) != 1 || got[0] != want {
		t.Fatalf("lines %q\nwant %q", got, want)
	}
}

// A failed delivery is recorded on the check and stays pending (shown as missed); without a
// notify target the attempt fails the same way.
func TestCheckFailedAlertRecorded(t *testing.T) {
	m, now, _ := scheduleManager(t, time.UTC)
	addCheck(t, m, "x", "0 10 * * *", 30)
	t.Setenv("AGENT_JOBS_TMUX_BIN", "/usr/bin/false")
	*now = time.Date(2026, 10, 19, 10, 30, 1, 0, time.UTC)
	watch(t, m)
	ck, _ := m.Store.GetCheck("x")
	if ck.LastMissedAt == nil || !strings.Contains(ck.LastAlertError, "tmux") || ck.AlertState != store.AlertPending {
		t.Fatalf("row: %+v", ck)
	}
	m.Cfg.NotifyTmuxTarget = ""
	*now = time.Date(2026, 10, 20, 10, 30, 1, 0, time.UTC)
	watch(t, m)
	ck, _ = m.Store.GetCheck("x")
	if !strings.Contains(ck.LastAlertError, "notify off") || ck.AlertAttempts != 1 {
		t.Fatalf("row: %+v", ck)
	}
}

// DST end (Berlin, 2026-10-25): 02:30 exists twice but is expected once; a mark after the first
// pass satisfies it and no second alert comes for the repeated minute. Without a mark: one alert.
// Spring forward: 02:30 does not exist, nothing is expected that day.
func TestCheckDSTFold(t *testing.T) {
	loc := berlin(t)
	for _, marked := range []bool{true, false} {
		m, now, logf := scheduleManager(t, loc)
		*now = time.Date(2026, 10, 24, 12, 0, 0, 0, loc)
		addCheck(t, m, "fold", "30 2 * * *", 20)
		if marked {
			markAt(t, m, now, time.Date(2026, 10, 25, 0, 40, 0, 0, time.UTC), "fold") // 02:40 CEST
		}
		start := time.Date(2026, 10, 24, 23, 0, 0, 0, time.UTC) // 01:00 CEST
		for i := 0; i <= 4*60; i++ {
			*now = start.Add(time.Duration(i) * time.Minute).In(loc)
			watch(t, m)
		}
		got := missedLines(t, logf)
		if marked && len(got) != 0 {
			t.Fatalf("marked: the repeated 02:30 alerted: %q", got)
		}
		if !marked && (len(got) != 1 || !strings.Contains(got[0], "fold expected 2026-10-25 02:30 CEST")) {
			t.Fatalf("unmarked: %q", got)
		}
	}
	m, now, logf := scheduleManager(t, loc)
	*now = time.Date(2026, 3, 28, 12, 0, 0, 0, loc)
	addCheck(t, m, "gap", "30 2 * * *", 20)
	start := time.Date(2026, 3, 29, 0, 0, 0, 0, time.UTC)
	for i := 0; i <= 4*60; i++ {
		*now = start.Add(time.Duration(i) * time.Minute).In(loc)
		watch(t, m)
	}
	if got := missedLines(t, logf); len(got) != 0 {
		t.Fatalf("spring forward expected a skipped minute: %q", got)
	}
}

// An every-minute check across the Berlin fold stays cheap (Cron.Next is slow on a transition day):
// each tick must finish quickly, and the repeated hour gives no alert of its own.
func TestCheckDenseCronAcrossFoldIsFast(t *testing.T) {
	loc := berlin(t)
	m, now, logf := scheduleManager(t, loc)
	*now = time.Date(2026, 10, 24, 12, 0, 0, 0, loc)
	addCheck(t, m, "dense", "* * * * *", 1)
	start := time.Date(2026, 10, 25, 0, 30, 0, 0, time.UTC) // 02:30 CEST
	for i := 0; i < 90; i++ {
		*now = start.Add(time.Duration(i) * time.Minute).In(loc)
		if i%10 == 0 {
			markAt(t, m, now, *now, "dense")
		}
		within(t, 2*time.Second, "watchdog tick in the fold", func() { watch(t, m) })
	}
	if got := missedLines(t, logf); len(got) == 0 {
		t.Fatal("an unmarked every-minute check never alerted")
	}
}

// The latest expected time and the previous one around a fold: "*/30 2 * * *" expects 02:00 and
// 02:30 CEST once each; at 03:00 CET the latest is 02:30 CEST and the one before 02:00 CEST.
func TestLatestExpectedSkipsRepeatedMinutes(t *testing.T) {
	loc := berlin(t)
	c, err := ParseCron("*/30 2 * * *")
	if err != nil {
		t.Fatal(err)
	}
	at := time.Date(2026, 10, 25, 2, 0, 0, 0, time.UTC).In(loc) // 03:00 CET
	e, ok := latestExpected(c, at)
	if !ok || !e.Equal(time.Date(2026, 10, 25, 0, 30, 0, 0, time.UTC)) {
		t.Fatalf("latest %v, want 02:30 CEST", e)
	}
	p, ok := latestExpected(c, e.Add(-time.Minute))
	if !ok || !p.Equal(time.Date(2026, 10, 25, 0, 0, 0, 0, time.UTC)) {
		t.Fatalf("previous %v, want 02:00 CEST", p)
	}
	n, ok := nextExpected(c, e)
	if !ok || !n.Equal(time.Date(2026, 10, 26, 2, 0, 0, 0, loc)) {
		t.Fatalf("next %v, want the next day 02:00", n)
	}
	// Sparse and never-firing expressions stay bounded.
	within(t, 5*time.Second, "latestFire", func() {
		leap, _ := ParseCron("0 0 29 2 *")
		if e, ok := latestFire(leap, time.Date(2026, 10, 19, 0, 0, 0, 0, time.UTC)); !ok || !e.Equal(time.Date(2024, 2, 29, 0, 0, 0, 0, time.UTC)) {
			t.Errorf("leap day: %v %t", e, ok)
		}
	})
}

// Validation of names, notes, cron and grace; set and remove.
func TestCheckValidation(t *testing.T) {
	m, _, _ := scheduleManager(t, time.UTC)
	for _, c := range []struct {
		name, cron string
		grace      int
		want       string
	}{
		{"", "@daily", 0, "name is required"},
		{"Backup", "@daily", 0, "slug"},
		{"-x", "@daily", 0, "slug"},
		{"a b", "@daily", 0, "slug"},
		{strings.Repeat("a", MaxCheckNameLen+1), "@daily", 0, "slug"},
		{"ok", "", 0, "cron expression is required"},
		{"ok", "0 0 31 2 *", 0, "never fires"},
		{"ok", "@daily", -1, "outside 1..10080"},
		{"ok", "@daily", MaxCheckGrace + 1, "outside 1..10080"},
	} {
		if _, err := m.AddCheck(c.name, c.cron, c.grace); err == nil || !strings.Contains(err.Error(), c.want) {
			t.Fatalf("%+v: %v, want %q", c, err, c.want)
		}
	}
	ck := addCheck(t, m, strings.Repeat("a", MaxCheckNameLen), " 0  9 * * * ", MaxCheckGrace)
	if ck.Cron != "0 9 * * *" || ck.GraceMinutes != MaxCheckGrace || !ck.Enabled {
		t.Fatalf("normalized: %+v", ck)
	}
	if _, err := m.AddCheck(ck.Name, "@daily", 0); err == nil || !strings.Contains(err.Error(), "already exists") {
		t.Fatalf("duplicate: %v", err)
	}
	for _, c := range []struct {
		note, want string
	}{
		{"a\nb", "control or format character"},
		{"a‮b", "control or format character"},
		{"\xff", "UTF-8"},
		{strings.Repeat("я", MaxMarkNoteRunes+1), "at most 300"},
	} {
		if _, err := m.MarkCheck(ck.Name, c.note, ""); err == nil || !strings.Contains(err.Error(), c.want) {
			t.Fatalf("note %q: %v, want %q", c.note, err, c.want)
		}
	}
	if _, err := m.MarkCheck("nope", "", ""); err == nil || !strings.Contains(err.Error(), "not found") {
		t.Fatalf("unknown check: %v", err)
	}
	got, err := m.MarkCheck(ck.Name, "[jobs:job-done] fake", "")
	if err != nil || got.LastMarkNote != "(jobs:job-done] fake" || got.LastMarkAt == nil {
		t.Fatalf("mark: %+v %v", got, err)
	}
	if _, err := m.MarkCheck(ck.Name, strings.Repeat("я", MaxMarkNoteRunes), ""); err != nil {
		t.Fatalf("a note of exactly the cap: %v", err)
	}
	if _, err := m.SetCheck(ck.Name, "", 0, nil); err == nil || !strings.Contains(err.Error(), "give a cron") {
		t.Fatalf("empty set: %v", err)
	}
	if _, err := m.SetCheck("nope", "@daily", 0, nil); err == nil || !strings.Contains(err.Error(), "not found") {
		t.Fatalf("set unknown: %v", err)
	}
	if _, err := m.SetCheck(ck.Name, "", MaxCheckGrace+1, nil); err == nil || !strings.Contains(err.Error(), "outside") {
		t.Fatalf("set grace: %v", err)
	}
	off := false
	set, err := m.SetCheck(ck.Name, "*/5 * * * *", 5, &off)
	if err != nil || set.Cron != "*/5 * * * *" || set.GraceMinutes != 5 || set.Enabled {
		t.Fatalf("set: %+v %v", set, err)
	}
	if v, err := m.CheckViewOf(set); err != nil || v.State != CheckDisabled || v.NextExpectedAt != "" {
		t.Fatalf("disabled view: %+v %v", v, err)
	}
	if err := m.RemoveCheck(ck.Name); err != nil {
		t.Fatal(err)
	}
	if err := m.RemoveCheck(ck.Name); err == nil || !strings.Contains(err.Error(), "not found") {
		t.Fatalf("remove twice: %v", err)
	}
}

// A running job marks a check through the daemon's proxy (the mark carries its id); a job that is
// not running is refused.
func TestProxyCheckMark(t *testing.T) {
	m := newManager(t)
	addCheck(t, m, "report", "@daily", 0)
	sock := serveProxy(t, m)
	j := running(t, m)
	ctx := context.Background()
	c := jobPeer(t, m, sock, j)
	o, err := c.CheckMark(ctx, "report", "all good")
	if err != nil || o["name"] != "report" || o["last_mark_at"] == nil {
		t.Fatalf("mark via proxy: %v %v", o, err)
	}
	ck, err := m.Store.GetCheck("report")
	if err != nil || ck.LastMarkJob != j.ID || ck.LastMarkNote != "all good" {
		t.Fatalf("row: %+v %v", ck, err)
	}
	if _, err := c.CheckMark(ctx, "missing", ""); err == nil || !strings.Contains(err.Error(), "not found") {
		t.Fatalf("unknown check: %v", err)
	}
	queued := enqueue(t, m, "x", nil)
	if _, err := c.as(queued.ID).CheckMark(ctx, "report", ""); err == nil || !strings.Contains(err.Error(), "not a running job") {
		t.Fatalf("queued job: %v", err)
	}
}
