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
	if msg := callErr(t, cs, "job_stats", map[string]any{"by": "repo"}); !strings.Contains(msg, "must be one of") {
		t.Fatalf("bad by: %q", msg)
	}
	if msg := callErr(t, cs, "job_stats", map[string]any{"since": "soon"}); !strings.Contains(msg, "since") {
		t.Fatalf("bad since: %q", msg)
	}
}
