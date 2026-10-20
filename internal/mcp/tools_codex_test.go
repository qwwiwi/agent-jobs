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

// A queued or running codex job reports its cost unknown before any attempt ended: never a known $0.00.
func TestCodexJobViewCostUnknownWhileActive(t *testing.T) {
	m := newManager(t)
	now := time.Now()
	ids := []string{"261018-120000-c0d1", "261018-120000-c0d2"}
	for i, status := range []string{store.StatusQueued, store.StatusRunning} {
		j := &store.Job{ID: ids[i], Agent: "test", Title: "codex review", Kind: "review", Brief: "b", Status: status,
			Model: "gpt-6.1-sol", Runner: store.RunnerCodex, TimeoutMinutes: 45, CreatedAt: now.Add(-time.Hour)}
		if status == store.StatusRunning {
			j.StartedAt = &now
		}
		if err := m.Store.Insert(j); err != nil {
			t.Fatal(err)
		}
	}
	cs := connect(t, mustServer(t, m))
	for _, id := range ids {
		var st JobView
		call(t, cs, "job_status", map[string]any{"id": id}, &st)
		if !st.CostUnknown {
			t.Fatalf("job_status %s: %+v", id, st)
		}
		var res JobResultOutput
		call(t, cs, "job_result", map[string]any{"id": id}, &res)
		if !res.CostUnknown {
			t.Fatalf("job_result %s: %+v", id, res)
		}
	}
	var list JobListOutput
	call(t, cs, "job_list", map[string]any{}, &list)
	if len(list.Jobs) != 2 || !list.Jobs[0].CostUnknown || !list.Jobs[1].CostUnknown {
		t.Fatalf("job_list: %+v", list)
	}
}

// A codex job_start shows codex_jobs_today n/limit; past the cap it is refused, claude jobs still
// start, and system_doctor shows the count.
func TestJobStartCodexDailyCap(t *testing.T) {
	m := newManager(t)
	m.SelfBin = selfBin // the doctor's guard probe runs the real binary
	two := 2
	m.Cfg.CodexJobsPerDay = &two
	cs := connect(t, mustServer(t, m))
	codex := startArgs(func(a map[string]any) { a["kind"] = "review"; a["model"] = "gpt-5.5" })
	var v JobView
	call(t, cs, "job_start", codex, &v)
	if v.CodexJobsToday != "1/2" {
		t.Fatalf("first: %q", v.CodexJobsToday)
	}
	call(t, cs, "job_start", codex, &v)
	if v.CodexJobsToday != "2/2" {
		t.Fatalf("second: %q", v.CodexJobsToday)
	}
	if msg := callErr(t, cs, "job_start", codex); !strings.Contains(msg, "codex daily cap reached: 2 codex jobs") {
		t.Fatalf("third: %q", msg)
	}
	var c JobView
	call(t, cs, "job_start", startArgs(nil), &c)
	if c.Runner != "claude" || c.CodexJobsToday != "" {
		t.Fatalf("claude job: %+v", c)
	}
	var doc SystemDoctorOutput
	call(t, cs, "system_doctor", map[string]any{}, &doc)
	if doc.CodexJobsToday != "2/2" {
		t.Fatalf("doctor: %q", doc.CodexJobsToday)
	}
}
