package mcp

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/qwwiwi/agent-jobs/internal/store"
)

// A finished codex job: its cost is unknown (never $0.00) and its turn/budget caps are not applicable,
// in job_status, job_result and job_list alike.
func TestCodexJobViewCostUnknownAndLimits(t *testing.T) {
	m := newManager(t)
	now := time.Now()
	j := &store.Job{ID: "261018-120000-c0de", Agent: "test", Title: "codex review", Kind: "review", Brief: "b",
		Status: store.StatusSucceeded, Model: "gpt-6.1-sol", Runner: store.RunnerCodex, TimeoutMinutes: 45,
		CreatedAt: now.Add(-time.Hour), StartedAt: &now, FinishedAt: &now, Turns: 4, DurationSec: 600, CostUnknown: true}
	if err := m.Store.Insert(j); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(m.JobDir(j.ID), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(m.JobDir(j.ID), "report.md"), []byte("## Summary\nok\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	cs := connect(t, mustServer(t, m))
	var st JobView
	call(t, cs, "job_status", map[string]any{"id": j.ID}, &st)
	if !st.CostUnknown || st.MaxTurns != 0 || st.MaxBudgetUSD != 0 || st.Limits != "n/a (codex)" {
		t.Fatalf("job_status: %+v", st)
	}
	var res JobResultOutput
	call(t, cs, "job_result", map[string]any{"id": j.ID}, &res)
	if !res.CostUnknown || !strings.Contains(res.DoneLine, "cost unknown") || strings.Contains(res.DoneLine, "$0.00") {
		t.Fatalf("job_result: %+v", res)
	}
	var list JobListOutput
	call(t, cs, "job_list", map[string]any{}, &list)
	if len(list.Jobs) != 1 || !list.Jobs[0].CostUnknown || list.Jobs[0].Limits != "n/a (codex)" {
		t.Fatalf("job_list: %+v", list)
	}
}
