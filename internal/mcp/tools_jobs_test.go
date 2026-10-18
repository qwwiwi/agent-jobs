package mcp

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/qwwiwi/agent-jobs/internal/jobs"
	"github.com/qwwiwi/agent-jobs/internal/store"
)

// job_result: secret-looking values redacted, "Not done / uncertain" and "Progress" given as fields
// (Progress after it); a report without Progress is not an error.
func TestJobResultRedactsAndShowsProgress(t *testing.T) {
	m := newManager(t)
	cs := connect(t, mustServer(t, m))
	now := time.Now()
	tok := "ghp_" + strings.Repeat("Q", 36)
	for i, c := range []struct {
		report, progress string
	}{
		{"## Summary\nused " + tok + "\n\n## Not done / uncertain\nnothing\n\n## Progress\n- 10:00 UTC read\n- 10:05 UTC fixed\n",
			"- 10:00 UTC read\n- 10:05 UTC fixed"},
		{"## Summary\nok\n\n## Not done / uncertain\nnothing\n", ""},
	} {
		j := &store.Job{ID: fmt.Sprintf("261018-12000%d-abcd", i), Agent: "test", Title: "t", Kind: "general", Brief: "b",
			Status: store.StatusSucceeded, Model: "opus", MaxTurns: 1, MaxBudgetUSD: 1, TimeoutMinutes: 1,
			CreatedAt: now, StartedAt: &now, FinishedAt: &now, Error: "auth Bearer " + strings.Repeat("z", 24)}
		if err := m.Store.Insert(j); err != nil {
			t.Fatal(err)
		}
		dir := m.JobDir(j.ID)
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "report.md"), []byte(c.report), 0o600); err != nil {
			t.Fatal(err)
		}
		var res JobResultOutput
		call(t, cs, "job_result", map[string]any{"id": j.ID}, &res)
		if strings.Contains(res.Report, tok) || strings.Contains(res.Error, strings.Repeat("z", 24)) ||
			res.NotDone != "nothing" || res.Progress != c.progress || res.ReportMissing {
			t.Fatalf("case %d: %+v", i, res)
		}
		if i == 0 && !strings.Contains(res.Report, "[redacted:github]") {
			t.Fatalf("case %d: report %q", i, res.Report)
		}
	}
}

// job_start: a brief above max_brief_bytes is refused; one naming many files is queued with a size_warning.
func TestJobStartBriefSize(t *testing.T) {
	m := newManager(t)
	cs := connect(t, mustServer(t, m))
	msg := callErr(t, cs, "job_start", startArgs(func(a map[string]any) { a["brief"] = strings.Repeat("x", jobs.DefaultMaxBriefBytes+1) }))
	if !strings.Contains(msg, "above the cap") || !strings.Contains(msg, "max_brief_bytes") {
		t.Fatalf("oversized brief: %q", msg)
	}
	var many strings.Builder
	for i := 0; i <= jobs.DefaultBriefWarnFiles; i++ {
		fmt.Fprintf(&many, "- pkg/file%d.go\n", i)
	}
	var v JobView
	call(t, cs, "job_start", startArgs(func(a map[string]any) { a["brief"] = many.String() }), &v)
	if v.ID == "" || !strings.Contains(v.SizeWarning, "files") {
		t.Fatalf("size warning: %+v", v)
	}
	var small JobView
	call(t, cs, "job_start", startArgs(nil), &small)
	if small.ID == "" || small.SizeWarning != "" {
		t.Fatalf("small brief: %+v", small)
	}
}
