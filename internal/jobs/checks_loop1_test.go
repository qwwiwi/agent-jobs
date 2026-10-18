package jobs

import (
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/qwwiwi/agent-jobs/internal/store"
)

// Codex review (fix loop 1, item 1): the window of E is (P, E+grace], P the previous expected time,
// so a mark early in the hour (before E, after P) counts even for the first E after creation.
func TestCheckEarlyMarkCountsForFirstExpected(t *testing.T) {
	m, now, logf := scheduleManager(t, time.UTC)
	*now = time.Date(2026, 10, 19, 9, 1, 0, 0, time.UTC)
	addCheck(t, m, "hourly", "0 * * * *", 60)
	markAt(t, m, now, time.Date(2026, 10, 19, 9, 59, 0, 0, time.UTC), "hourly")
	*now = time.Date(2026, 10, 19, 11, 0, 1, 0, time.UTC) // E=10:00 + 60m passed
	watch(t, m)
	if got := missedLines(t, logf); len(got) != 0 {
		t.Fatalf("an early mark at 09:59 did not satisfy 10:00: %q", got)
	}
	if v, _ := m.CheckViews(); v[0].State != CheckOK {
		t.Fatalf("state %q, want ok", v[0].State)
	}
}

// A single mark late in E1's grace that also lies in E2's window satisfies E1 only: each mark
// satisfies at most one expected time, so E2 alerts when nothing else comes.
func TestCheckMarkSatisfiesOneExpectedOnly(t *testing.T) {
	m, now, logf := scheduleManager(t, time.UTC) // 09:00
	addCheck(t, m, "hourly", "0 * * * *", 60)
	markAt(t, m, now, time.Date(2026, 10, 19, 10, 59, 0, 0, time.UTC), "hourly") // E1=10:00, +grace-1m
	*now = time.Date(2026, 10, 19, 11, 0, 1, 0, time.UTC)
	watch(t, m)
	if got := missedLines(t, logf); len(got) != 0 {
		t.Fatalf("E1 alerted: %q", got)
	}
	*now = time.Date(2026, 10, 19, 12, 0, 1, 0, time.UTC)
	watch(t, m)
	got := missedLines(t, logf)
	if len(got) != 1 || !strings.Contains(got[0], "hourly expected 2026-10-19 11:00 UTC") {
		t.Fatalf("E2 must alert (the mark belongs to E1): %q", got)
	}
}

// Codex review (fix loop 1, item 2): an evaluated expected time is never re-judged from marks, so
// many marks after a satisfied daily E (more than a window stores) never turn it into missed.
func TestCheckManyMarksKeepEvaluatedOK(t *testing.T) {
	m, now, logf := scheduleManager(t, time.UTC) // 09:00
	addCheck(t, m, "daily", "0 10 * * *", 30)
	markAt(t, m, now, time.Date(2026, 10, 19, 9, 30, 0, 0, time.UTC), "daily")
	*now = time.Date(2026, 10, 19, 10, 30, 1, 0, time.UTC)
	watch(t, m)
	for i := 0; i < 250; i++ {
		markAt(t, m, now, time.Date(2026, 10, 19, 10, 30, 2+i, 0, time.UTC), "daily")
	}
	*now = time.Date(2026, 10, 19, 10, 35, 0, 0, time.UTC)
	watch(t, m)
	if got := missedLines(t, logf); len(got) != 0 {
		t.Fatalf("a satisfied E alerted after many marks: %q", got)
	}
	if v, _ := m.CheckViews(); v[0].State != CheckOK {
		t.Fatalf("state %q, want ok", v[0].State)
	}
}

// Codex review (fix loop 1, item 5): a late mark keeps the missed expected time visible in
// MissedChecks (status, system_doctor) as missed with the late mark's time.
func TestCheckLateMarkKeepsMissedVisible(t *testing.T) {
	m, now, _ := scheduleManager(t, time.UTC)
	addCheck(t, m, "nightly", "0 10 * * *", 15)
	*now = time.Date(2026, 10, 19, 10, 15, 1, 0, time.UTC)
	watch(t, m)
	*now = time.Date(2026, 10, 19, 10, 40, 0, 0, time.UTC)
	if _, err := m.MarkCheck("nightly", "done late", ""); err != nil {
		t.Fatal(err)
	}
	missed, err := m.MissedChecks()
	if err != nil || len(missed) != 1 || missed[0].State != CheckMissed {
		t.Fatalf("late mark hid the missed E: %+v %v", missed, err)
	}
	if txt := CheckViewText(missed[0]); !strings.Contains(txt, "missed (late mark at 2026-10-19 10:40 UTC)") {
		t.Fatalf("text %q", txt)
	}
}

// Codex review (fix loop 1, item 3): a crash after the missed time was claimed and before its
// alert was typed leaves a pending alert; the next tick after the restart delivers it, once.
func TestCheckAlertDeliveredAfterCrashBeforeDelivery(t *testing.T) {
	m, now, logf := scheduleManager(t, time.UTC)
	addCheck(t, m, "nightly", "0 10 * * *", 15)
	*now = time.Date(2026, 10, 19, 10, 15, 1, 0, time.UTC)
	if err := m.watchChecks(); err != nil { // judged and claimed; the daemon dies here
		t.Fatal(err)
	}
	if ck, _ := m.Store.GetCheck("nightly"); ck.AlertState != store.AlertPending || ck.AlertAttempts != 0 {
		t.Fatalf("claimed: %+v", ck)
	}
	if missed, _ := m.MissedChecks(); len(missed) != 1 || !strings.Contains(CheckViewText(missed[0]), "alert pending (0 of 5") {
		t.Fatalf("pending not shown: %+v", missed)
	}
	log := m.Log
	m.Close()
	m2, err := Open("test")
	if err != nil {
		t.Fatal(err)
	}
	defer m2.Close()
	m2.Cfg.NotifyTmuxTarget, m2.Loc, m2.Log = "fake-session", time.UTC, log
	m2.Now = func() time.Time { return *now }
	for i := 0; i < 3; i++ {
		if err := m2.Tick(); err != nil {
			t.Fatal(err)
		}
	}
	if got := missedLines(t, logf); len(got) != 1 || !strings.Contains(got[0], "nightly expected 2026-10-19 10:00 UTC") {
		t.Fatalf("lines %q", got)
	}
	if ck, _ := m2.Store.GetCheck("nightly"); ck.AlertState != store.AlertDelivered || ck.AlertAttempts != 1 {
		t.Fatalf("row: %+v", ck)
	}
}

// A failing tmux is retried with backoff (1, 2, 4, 8 minutes), then the alert is failed after
// MaxCheckAlertAttempts; a tmux that recovers in between delivers it.
func TestCheckAlertRetriedThenFailed(t *testing.T) {
	for _, recover := range []bool{false, true} {
		m, now, logf := scheduleManager(t, time.UTC)
		addCheck(t, m, "x", "0 10 * * *", 30)
		good := os.Getenv("AGENT_JOBS_TMUX_BIN")
		t.Setenv("AGENT_JOBS_TMUX_BIN", "/usr/bin/false")
		start := time.Date(2026, 10, 19, 10, 30, 1, 0, time.UTC)
		attempts := func() *store.Check {
			ck, err := m.Store.GetCheck("x")
			if err != nil {
				t.Fatal(err)
			}
			return ck
		}
		*now = start
		watch(t, m)
		watch(t, m) // before the backoff: no new attempt
		if ck := attempts(); ck.AlertAttempts != 1 || ck.AlertState != store.AlertPending || !strings.Contains(ck.LastAlertError, "tmux") {
			t.Fatalf("after one attempt: %+v", ck)
		}
		at := start
		for n := 1; n < MaxCheckAlertAttempts; n++ {
			at = at.Add(checkAlertBackoff(n))
			*now = at
			if recover && n == 2 {
				t.Setenv("AGENT_JOBS_TMUX_BIN", good)
			}
			watch(t, m)
			ck := attempts()
			if recover && n >= 2 {
				if ck.AlertState != store.AlertDelivered || ck.AlertAttempts != 3 {
					t.Fatalf("recovered: %+v", ck)
				}
				continue
			}
			if ck.AlertAttempts != n+1 {
				t.Fatalf("attempt %d: %+v", n+1, ck)
			}
		}
		ck := attempts()
		if !recover {
			if ck.AlertState != store.AlertFailed || ck.AlertAttempts != MaxCheckAlertAttempts {
				t.Fatalf("after the last attempt: %+v", ck)
			}
			*now = at.Add(time.Hour)
			watch(t, m)
			if ck := attempts(); ck.AlertAttempts != MaxCheckAlertAttempts {
				t.Fatalf("a failed alert was retried: %+v", ck)
			}
			missed, _ := m.MissedChecks()
			if len(missed) != 1 || !strings.Contains(CheckViewText(missed[0]), "alert not delivered after 5 attempts") {
				t.Fatalf("failed alert not shown: %+v", missed)
			}
		}
		if got := missedLines(t, logf); recover != (len(got) == 1) {
			t.Fatalf("recover=%t lines %q", recover, got)
		}
	}
}

// Codex review (fix loop 1, item 4): a check_set between the watchdog's read and its claim (a
// grace change, or a pause and resume) voids the claim: no obsolete expected time is judged.
func TestCheckClaimVoidedByConcurrentEdit(t *testing.T) {
	for _, edit := range []string{"grace", "pause-resume"} {
		m, now, logf := scheduleManager(t, time.UTC)
		addCheck(t, m, "x", "0 10 * * *", 30)
		*now = time.Date(2026, 10, 19, 10, 30, 1, 0, time.UTC)
		list, err := m.Store.ListChecks(true) // the watchdog reads the check
		if err != nil || len(list) != 1 {
			t.Fatal(list, err)
		}
		switch edit { // the owner edits it in between
		case "grace":
			if _, err := m.SetCheck("x", "", 45, nil); err != nil {
				t.Fatal(err)
			}
		default:
			off, on := false, true
			if _, err := m.SetCheck("x", "", 0, &off); err != nil {
				t.Fatal(err)
			}
			if _, err := m.SetCheck("x", "", 0, &on); err != nil {
				t.Fatal(err)
			}
		}
		claimed, err := m.judgeCheck(list[0], *now)
		if err != nil || claimed {
			t.Fatalf("%s: stale read claimed: %t %v", edit, claimed, err)
		}
		if err := m.deliverCheckAlerts(); err != nil {
			t.Fatal(err)
		}
		if ck, _ := m.Store.GetCheck("x"); ck.EvaluatedAt != nil || ck.AlertState != "" {
			t.Fatalf("%s: row %+v", edit, ck)
		}
		if got := missedLines(t, logf); len(got) != 0 {
			t.Fatalf("%s: alerted %q", edit, got)
		}
	}
}

// Codex review (fix loop 1, item 6): Tick types the alerts after releasing the lifecycle lock, so a
// stalled tmux leaves tick.lock free while it hangs.
func TestCheckDeliveryOutsideLifecycleLock(t *testing.T) {
	m, now, _ := scheduleManager(t, time.UTC)
	_, callsFile := stalledTmux(t)
	TmuxDeliveryTimeout = 3 * time.Second
	addCheck(t, m, "x", "0 10 * * *", 30)
	*now = time.Date(2026, 10, 19, 10, 30, 1, 0, time.UTC)
	done := make(chan error, 1)
	go func() { done <- m.Tick() }()
	waitFor(t, "tmux called", 5*time.Second, func() bool {
		_, err := os.Stat(callsFile)
		return err == nil
	})
	f, err := os.OpenFile(filepath.Join(m.StateDir, "tick.lock"), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		t.Fatalf("lifecycle lock held during the delivery: %v", err)
	}
	_ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
	select {
	case <-done:
		t.Fatal("tick finished before the check; the stall was not observed")
	default:
	}
	if err := <-done; err != nil {
		t.Fatalf("tick: %v", err)
	}
	if ck, _ := m.Store.GetCheck("x"); ck.AlertState != store.AlertPending || !strings.Contains(ck.LastAlertError, "timed out") {
		t.Fatalf("row: %+v", ck)
	}
}
