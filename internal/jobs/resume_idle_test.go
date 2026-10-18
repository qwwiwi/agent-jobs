package jobs

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/qwwiwi/agent-jobs/internal/store"
)

// The idle-after-result rule closes a resumed attempt too (claude --resume, same watcher). The idle
// close belongs to that attempt only: a report.md of an earlier attempt is not kept as its report,
// its cost is known (it had a result event), and the next resume moves its marker aside so the next
// attempt's verdict does not report the old idle close.
func TestIdleCloseOnResumedAttempt(t *testing.T) {
	m := idleManager(t)
	sessionsHome(t, true)
	st := startAndStop(t, m, nil)
	dir := m.JobDir(st.ID)
	// A report.md left by attempt 1 (the job dir is writable by the job).
	stale := "## Summary\nSTALE_ATTEMPT_1_REPORT\n"
	if err := os.WriteFile(filepath.Join(dir, "report.md"), []byte(stale), 0o600); err != nil {
		t.Fatal(err)
	}

	// Attempt 2: resumed in the same session, ends with a non-report result and goes idle.
	if _, err := m.Resume(st.ID, "FAKE_IDLE_NOREPORT update", Limits{MaxBudgetUSD: 4}); err != nil {
		t.Fatal(err)
	}
	j := runToEnd(t, m, st.ID)
	if j.Status != store.StatusFailed || j.Attempt != 2 || j.RunMode != store.RunResume ||
		!strings.Contains(j.Error, ReasonIdleClosed) || !strings.Contains(j.Error, "no report in the stream") {
		t.Fatalf("attempt 2: %+v (stderr %s)", j, readFile(t, filepath.Join(dir, "stderr.log")))
	}
	if j.CostUnknown || j.Turns != 2 || j.CostUSD != 0.1 {
		t.Fatalf("an idle close has a result event: cost must be known, got unknown=%v turns=%d cost=%v", j.CostUnknown, j.Turns, j.CostUSD)
	}
	if !strings.Contains(readFile(t, filepath.Join(dir, "argv.txt")), "--resume\n"+st.SessionID+"\n") {
		t.Fatal("attempt 2 must resume the session")
	}
	if r := readFile(t, filepath.Join(dir, "report.md")); strings.Contains(r, "STALE_ATTEMPT_1_REPORT") || !strings.Contains(r, "ended without a report") {
		t.Fatalf("attempt 2 report.md: %q", r)
	}
	if r := readFile(t, filepath.Join(dir, AttemptReportName(1))); r != stale {
		t.Fatalf("attempt 1 report: %q", r)
	}

	// Attempt 3: a normal exit. The idle marker of attempt 2 is archived, not reported again.
	if _, err := m.Resume(st.ID, "plain update", Limits{MaxBudgetUSD: 4}); err != nil {
		t.Fatal(err)
	}
	j = runToEnd(t, m, st.ID)
	if j.Status != store.StatusSucceeded || j.Attempt != 3 || strings.Contains(j.Error, ReasonIdleClosed) || j.CostUnknown {
		t.Fatalf("attempt 3: %+v", j)
	}
	if _, err := os.Stat(filepath.Join(dir, IdleClosedMarker)); err == nil {
		t.Fatal("attempt 2's idle marker left at the top level")
	}
	if !strings.Contains(readFile(t, filepath.Join(dir, attemptsDir, "2", IdleClosedMarker)), ReasonIdleClosed) {
		t.Fatal("attempt 2's idle marker not archived with its run files")
	}
	atts, err := m.Store.Attempts(j.ID)
	if err != nil || len(atts) != 2 || atts[1].N != 2 || atts[1].CostUnknown || !strings.Contains(atts[1].Error, ReasonIdleClosed) {
		t.Fatalf("attempts: %+v %v", atts, err)
	}
}

// A resume that falls back to a fresh session inside the wrapper keeps the idle rule: the fallback
// run is closed after its result and the wrapper settles it as an idle close.
func TestIdleCloseOnFallbackRun(t *testing.T) {
	m := idleManager(t)
	sessionsHome(t, true)
	st := startAndStop(t, m, nil)
	if _, err := m.Resume(st.ID, "FAKE_RESUME_FAIL FAKE_IDLE_OK", Limits{MaxBudgetUSD: 4}); err != nil {
		t.Fatal(err)
	}
	j := runToEnd(t, m, st.ID)
	dir := m.JobDir(j.ID)
	if j.Status != store.StatusSucceeded || j.RunMode != store.RunFallback || !strings.Contains(j.Error, ReasonIdleClosed) {
		t.Fatalf("row: %+v (stderr %s)", j, readFile(t, filepath.Join(dir, "stderr.log")))
	}
	if j.Turns != 6 || j.CostUSD != 0.75 || j.CostUnknown {
		t.Fatalf("turns/cost %d/%v unknown=%v, want the last result's 6/0.75", j.Turns, j.CostUSD, j.CostUnknown)
	}
	if r := readFile(t, filepath.Join(dir, "report.md")); !strings.HasPrefix(r, "## Summary\nidle ok") {
		t.Fatalf("report.md: %q", r)
	}
	if !strings.Contains(readFile(t, filepath.Join(dir, "stderr.log")), "falling back to a fresh session") {
		t.Fatal("the run must have fallen back")
	}
}
