package jobs

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/qwwiwi/agent-jobs/internal/store"
)

// A claude attempt that ends its turn without a report, such as one that ends on «waiting for the
// subagent notifications» (#126), is not succeeded: it fails with NoteNoSummary, under report_contract
// warn too, and is continued once like an attempt stopped at its turn limit.
func TestMidTaskEndAutoContinues(t *testing.T) {
	m := newManager(t)
	sessionsHome(t, true)
	j := enqueue(t, m, "FAKE_WAIT_SUBAGENTS_ONCE", nil)
	tick(t, m)
	waitExit(t, m, j.ID)
	tick(t, m) // collects attempt 1, queues and launches attempt 2
	got := get(t, m, j.ID)
	if got.Status != store.StatusRunning || got.Attempt != 2 || got.RunMode != store.RunResume {
		t.Fatalf("after the mid-task end: %s attempt %d mode %s %q", got.Status, got.Attempt, got.RunMode, got.Error)
	}
	for _, s := range []string{autoContinueTag + "attempt 1 " + stopMidTask + " after 4 turns (cost $0.20)",
		"Waiting for the subagent notifications.", "Subagents the attempt started ended with it",
		"Continue from here; do not redo what is listed as done."} {
		if !strings.Contains(got.PendingContext, s) {
			t.Fatalf("update lacks %q:\n%s", s, got.PendingContext)
		}
	}
	atts, err := m.Store.Attempts(j.ID)
	if err != nil || len(atts) != 1 || atts[0].Status != store.StatusFailed || !strings.Contains(atts[0].Error, NoteNoSummary) {
		t.Fatalf("archived attempt 1: %+v %v", atts, err)
	}
	waitExit(t, m, j.ID)
	tick(t, m)
	got = get(t, m, j.ID)
	if got.Status != store.StatusSucceeded || got.Attempt != 2 || got.Error != "" {
		t.Fatalf("continuation: %s attempt %d %q", got.Status, got.Attempt, got.Error)
	}
	if line := DoneLine(got, 0); !strings.Contains(line, "auto-continued (attempt 2; attempt 1 "+stopMidTask) {
		t.Fatalf("done line: %s", line)
	}
}

// With auto_continue off, a mid-task end stays failed and job_resume accepts it (#126: a succeeded
// job cannot be resumed).
func TestMidTaskEndResumable(t *testing.T) {
	m := newManager(t)
	sessionsHome(t, true)
	noAutoContinue(m)
	j := runToEnd(t, m, enqueue(t, m, "FAKE_WAIT_SUBAGENTS_ONCE", nil).ID)
	if j.Status != store.StatusFailed || j.Attempt != 1 || j.Error != NoteNoSummary {
		t.Fatalf("mid-task end: %s attempt %d %q", j.Status, j.Attempt, j.Error)
	}
	r, err := m.Resume(j.ID, "the subagents are gone: finish the work yourself", Limits{})
	if err != nil || r.Status != store.StatusQueued || r.Attempt != 1 {
		t.Fatalf("resume of a mid-task end: %+v %v", r, err)
	}
	if got := runToEnd(t, m, j.ID); got.Status != store.StatusSucceeded || got.Attempt != 2 {
		t.Fatalf("resumed: %s attempt %d %q", got.Status, got.Attempt, got.Error)
	}
}

// A clean claude result with an empty final message and no report file ("exit 0 without a report",
// which the report contract never sees) is a mid-task end too: it is continued once, and an attempt 2
// that ends the same way is not continued again.
func TestNoReportEndAutoContinuesOnce(t *testing.T) {
	m := newManager(t)
	sessionsHome(t, true)
	j := enqueue(t, m, "FAKE_NOREPORT", nil)
	tick(t, m)
	waitExit(t, m, j.ID)
	// The update quotes the progress file: attempt 2 runs in the same no-report mode.
	if err := os.WriteFile(filepath.Join(m.JobDir(j.ID), ProgressFile), []byte("- next: FAKE_NOREPORT again\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	tick(t, m) // collects attempt 1, queues and launches attempt 2
	got := get(t, m, j.ID)
	if got.Status != store.StatusRunning || got.Attempt != 2 || got.RunMode != store.RunResume ||
		!strings.Contains(got.PendingContext, autoContinueTag+"attempt 1 "+stopMidTask) {
		t.Fatalf("after the empty end: %s attempt %d mode %s %q\n%s", got.Status, got.Attempt, got.RunMode, got.Error, got.PendingContext)
	}
	waitExit(t, m, j.ID)
	tick(t, m)
	got = get(t, m, j.ID)
	if got.Status != store.StatusFailed || got.Attempt != 2 || !strings.Contains(got.Error, "without a report") ||
		!strings.Contains(got.Error, "not auto-continued: it was auto-continued once already") {
		t.Fatalf("attempt 2: %s attempt %d %q", got.Status, got.Attempt, got.Error)
	}
}

// A runner failure that also left no report (a non-success result not flagged as an error, here a
// spent budget) is not a mid-task end: it is not continued.
func TestNoReportRunnerFailureNotContinued(t *testing.T) {
	m := newManager(t)
	sessionsHome(t, true)
	j := runToEnd(t, m, enqueue(t, m, "FAKE_NONSUCCESS_REPORT", nil).ID)
	if j.Status != store.StatusFailed || j.Attempt != 1 || strings.Contains(j.Error, "auto-continue") {
		t.Fatalf("runner failure: %s attempt %d %q", j.Status, j.Attempt, j.Error)
	}
}

// A gate job's stored result is what it delivers: one whose report has a valid block but no Summary
// section is not continued as a mid-task end, under report_contract warn (it succeeds with the note)
// and fail (it fails with it).
func TestGateNoSummaryNotContinued(t *testing.T) {
	for _, contract := range []string{ReportContractWarn, ReportContractFail} {
		t.Run(contract, func(t *testing.T) {
			m := newManager(t)
			sessionsHome(t, true)
			m.Cfg.ReportContract = contract
			j, err := m.StartGate(GateRequest{Kind: KindSlicing, Repo: briefRepo(t), Model: "sonnet"})
			if err != nil {
				t.Fatal(err)
			}
			b, err := json.Marshal(slicingBlock(j.Gate))
			if err != nil {
				t.Fatal(err)
			}
			p := filepath.Join(t.TempDir(), "report.md")
			if err := os.WriteFile(p, []byte("```json\n"+string(b)+"\n```\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			t.Setenv("FAKE_GATE_REPORT", p)
			got := runToEnd(t, m, j.ID)
			want := store.StatusSucceeded
			if contract == ReportContractFail {
				want = store.StatusFailed
			}
			if got.Status != want || got.Attempt != 1 || got.Error != NoteNoSummary {
				t.Fatalf("gate without Summary: %s attempt %d %q", got.Status, got.Attempt, got.Error)
			}
		})
	}
}
