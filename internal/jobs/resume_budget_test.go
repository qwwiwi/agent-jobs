package jobs

import (
	"strings"
	"testing"

	"github.com/qwwiwi/agent-jobs/internal/store"
)

// Issue #85: a job stopped before its cost was reported counts at its whole budget; at the $50
// ceiling job_stop's note says a resume is impossible and what to do instead, and job_resume refuses.
func TestStopAtBudgetCeilingResumeImpossible(t *testing.T) {
	m := newManager(t)
	sessionsHome(t, true)
	st := startAndStop(t, m, func(r *StartRequest) { r.MaxBudgetUSD = CeilingMaxBudgetUSD })
	if !st.CostUnknown {
		t.Fatalf("stopped row: %+v", st)
	}
	note, err := m.ResumeBudget(st)
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range []string{"resume impossible", "$50.00 of the $50 budget ceiling", "whole $50.00 budget", "no budget",
		"WIP commit", "start a new job"} {
		if !strings.Contains(note, s) {
			t.Fatalf("note lacks %q: %s", s, note)
		}
	}
	if _, err := m.Resume(st.ID, "go on", Limits{MaxBudgetUSD: 1}); err == nil || !strings.Contains(err.Error(), "max_budget_usd") {
		t.Fatalf("resume at the ceiling: %v", err)
	}
}

// A smaller stopped attempt: the note shows what it counts and what is left, and that much resumes.
func TestStopBelowBudgetCeilingShowsCapacity(t *testing.T) {
	m := newManager(t)
	sessionsHome(t, true)
	st := startAndStop(t, m, func(r *StartRequest) { r.MaxBudgetUSD = 10 })
	note, err := m.ResumeBudget(st)
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range []string{"$10.00 of the $50 budget ceiling", "whole $10.00 budget", "$40.00 left for job_resume",
		"job_resume needs max_budget_usd"} {
		if !strings.Contains(note, s) {
			t.Fatalf("note lacks %q: %s", s, note)
		}
	}
	if strings.Contains(note, "impossible") {
		t.Fatalf("resumable job called impossible: %s", note)
	}
	if r, err := m.Resume(st.ID, "go on", Limits{MaxBudgetUSD: 40}); err != nil || r.MaxBudgetUSD != 40 {
		t.Fatalf("resume with what is left: %+v %v", r, err)
	}
}

// The note follows resumeLimits' accounting: a known cost leaves a default budget; earlier attempts
// count too, on every ceiling.
func TestResumeBudgetNote(t *testing.T) {
	j := &store.Job{MaxTurns: 40, MaxBudgetUSD: 10, TimeoutMinutes: 60, Turns: 5, CostUSD: 1, DurationSec: 60}
	if n := resumeBudgetNote(j, nil); !strings.Contains(n, "$1.00 of the $50") || !strings.Contains(n, "$49.00 left") ||
		!strings.Contains(n, "default max_budget_usd is $9.00") || strings.Contains(n, "whole") {
		t.Fatalf("known cost: %s", n)
	}
	earlier := []store.Attempt{{MaxBudgetUSD: 45, CostUSD: 45, Turns: 10, DurationSec: 60}}
	if n := resumeBudgetNote(j, earlier); !strings.Contains(n, "$46.00 of the $50") || !strings.Contains(n, "default max_budget_usd is $4.00") {
		t.Fatalf("earlier attempt: %s", n)
	}
	turns := []store.Attempt{{MaxBudgetUSD: 5, CostUSD: 1, Turns: CeilingMaxTurns - 5, DurationSec: 60}}
	if n := resumeBudgetNote(j, turns); !strings.HasPrefix(n, "resume impossible") || !strings.Contains(n, "no turns") ||
		strings.Contains(n, "no budget") {
		t.Fatalf("turn ceiling: %s", n)
	}
}
