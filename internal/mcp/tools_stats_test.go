package mcp

import (
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/qwwiwi/agent-jobs/internal/jobs"
	"github.com/qwwiwi/agent-jobs/internal/store"
)

// job_stats is orchestrator-only and returns the same report as `agent-jobs stats --json`.
func TestJobStatsTool(t *testing.T) {
	if !slices.Contains(OrchestratorOnly, "job_stats") || slices.Contains(JobAllowed, "job_stats") {
		t.Fatal("job_stats must be orchestrator-only")
	}
	m := newManager(t)
	now := time.Now()
	m.Now = func() time.Time { return now }
	recent, old := now.Add(-time.Hour), now.Add(-10*24*time.Hour)
	insertRow(t, m, "ok1", store.StatusSucceeded, &recent, 1.5)
	insertRow(t, m, "bad1", store.StatusFailed, &old, 0.5)
	if _, err := m.BackfillStats(false); err != nil {
		t.Fatal(err)
	}
	cs := connect(t, mustServer(t, m))
	var out jobs.StatsReport
	call(t, cs, "job_stats", map[string]any{}, &out)
	if out.By != "kind" || out.Totals.Jobs != 2 || out.Totals.SuccessRate != 0.5 || len(out.Groups) != 1 {
		t.Fatalf("all: %+v", out)
	}
	call(t, cs, "job_stats", map[string]any{"since": "7d", "by": "model"}, &out)
	if out.Totals.Jobs != 1 || out.Groups[0].Key != "sonnet" || out.Since == "" {
		t.Fatalf("since 7d by model: %+v", out)
	}
	if msg := callErr(t, cs, "job_stats", map[string]any{"by": "owner"}); !strings.Contains(msg, "must be one of") {
		t.Fatalf("bad by: %q", msg)
	}
	if msg := callErr(t, cs, "job_stats", map[string]any{"since": "soon"}); !strings.Contains(msg, "since") {
		t.Fatalf("bad since: %q", msg)
	}
}

// job_outcome is orchestrator-only (absent from the job role), sets and overwrites the outcome, and
// refuses unknown ids and unfinished jobs; job_stats then carries the counts.
func TestJobOutcomeTool(t *testing.T) {
	if !slices.Contains(OrchestratorOnly, "job_outcome") || slices.Contains(JobAllowed, "job_outcome") || Allowed(RoleJob, "job_outcome") {
		t.Fatal("job_outcome must be orchestrator-only")
	}
	m := newManager(t)
	done := time.Now()
	insertRow(t, m, "ok1", store.StatusSucceeded, &done, 2)
	insertRow(t, m, "run1", store.StatusRunning, nil, 0)
	cs := connect(t, mustServer(t, m))
	var out JobOutcomeOutput
	call(t, cs, "job_outcome", map[string]any{"id": "ok1", "outcome": "reworked", "note": "tests were missing"}, &out)
	if out.Outcome != "reworked" || out.OutcomeNote != "tests were missing" {
		t.Fatalf("outcome: %+v", out)
	}
	out = JobOutcomeOutput{}
	call(t, cs, "job_outcome", map[string]any{"id": "ok1", "outcome": "accepted"}, &out)
	if out.Outcome != "accepted" || out.OutcomeNote != "" {
		t.Fatalf("overwrite: %+v", out)
	}
	if msg := callErr(t, cs, "job_outcome", map[string]any{"id": "nope", "outcome": "accepted"}); !strings.Contains(msg, "not found") {
		t.Fatalf("unknown id: %q", msg)
	}
	if msg := callErr(t, cs, "job_outcome", map[string]any{"id": "run1", "outcome": "accepted"}); !strings.Contains(msg, "is running") {
		t.Fatalf("running job: %q", msg)
	}
	var v JobView
	call(t, cs, "job_status", map[string]any{"id": "ok1"}, &v)
	if v.Outcome != "accepted" {
		t.Fatalf("job_status outcome: %+v", v)
	}
	if _, err := m.BackfillStats(false); err != nil {
		t.Fatal(err)
	}
	var rep jobs.StatsReport
	call(t, cs, "job_stats", map[string]any{"by": "repo"}, &rep)
	if rep.Totals.Outcomes["accepted"] != 1 || rep.Totals.CostPerUsableResultUSD != 2 {
		t.Fatalf("stats: %+v", rep.Totals)
	}
}

// wake_stats is orchestrator-only and returns the same report as `agent-jobs stats wakes --json`.
func TestWakeStatsTool(t *testing.T) {
	if !slices.Contains(OrchestratorOnly, "wake_stats") || slices.Contains(JobAllowed, "wake_stats") || Allowed(RoleJob, "wake_stats") {
		t.Fatal("wake_stats must be orchestrator-only")
	}
	m := newManager(t)
	now := time.Now()
	m.Now = func() time.Time { return now }
	for _, w := range []store.Wake{
		{At: now.Add(-time.Hour), Kind: store.WakeDelivery, Category: jobs.WakeJobDone, Bytes: 100, BatchSize: 3},
		{At: now.Add(-10 * 24 * time.Hour), Kind: store.WakeDelivery, Category: jobs.WakeSchedule, Bytes: 50},
		{At: now.Add(-time.Hour), Kind: store.WakeInjection, Category: jobs.InjectStop, Bytes: 9},
	} {
		if _, err := m.Store.AddWake(w); err != nil {
			t.Fatal(err)
		}
	}
	cs := connect(t, mustServer(t, m))
	var out jobs.WakeStatsReport
	call(t, cs, "wake_stats", map[string]any{}, &out)
	if out.Wakes != 2 || out.WakeBytes != 150 || out.InjectionBytes != 9 || len(out.Categories) != 3 {
		t.Fatalf("all: %+v", out)
	}
	call(t, cs, "wake_stats", map[string]any{"since": "7d"}, &out)
	if out.Wakes != 1 || out.Categories[0].Category != jobs.WakeJobDone || out.Categories[0].Members != 3 {
		t.Fatalf("since 7d: %+v", out)
	}
	if msg := callErr(t, cs, "wake_stats", map[string]any{"since": "soon"}); !strings.Contains(msg, "since") {
		t.Fatalf("bad since: %q", msg)
	}
}
