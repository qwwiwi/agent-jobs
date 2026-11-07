package mcp

import (
	"strings"
	"testing"

	"github.com/qwwiwi/agent-jobs/internal/store"
)

func TestResultBranchToolViews(t *testing.T) {
	m := newManager(t)
	cs := connect(t, mustServer(t, m))
	const note = "requested branch feature exists; not moved - published job/x"
	id := insertReportJob(t, m, 90, "## Summary\nok\n\n## Not done / uncertain\nnothing\n", func(j *store.Job) {
		j.RequestedBranch, j.Branch, j.BranchNote = "feature", "job/261106-150090-abcd", note
	})
	var status JobView
	call(t, cs, "job_status", map[string]any{"id": id}, &status)
	if status.RequestedBranch != "feature" || status.Branch == "" || status.BranchNote != note {
		t.Fatalf("status: %+v", status)
	}
	var list JobListOutput
	call(t, cs, "job_list", map[string]any{}, &list)
	if len(list.Jobs) != 1 || list.Jobs[0].Branch != status.Branch || list.Jobs[0].BranchNote != note {
		t.Fatalf("list: %+v", list)
	}
	for _, detail := range []string{"summary", "full"} {
		var result JobResultOutput
		call(t, cs, "job_result", map[string]any{"id": id, "detail": detail}, &result)
		if result.RequestedBranch != "feature" || result.Branch != status.Branch || result.BranchNote != note ||
			!strings.Contains(result.DoneLine, "branch "+status.Branch+" ("+note+")") {
			t.Fatalf("result %s: %+v", detail, result)
		}
	}
}

func TestJobStartResultBranchParameter(t *testing.T) {
	m := newManager(t)
	repo, _ := gitRepo(t)
	cs := connect(t, mustServer(t, m))
	var started JobView
	call(t, cs, "job_start", startArgs(func(a map[string]any) { a["repo"], a["branch"] = repo, "feature" }), &started)
	if started.RequestedBranch != "feature" || started.Branch != "" {
		t.Fatalf("start: %+v", started)
	}
	for _, name := range []string{"main", "release-next", "bad name"} {
		msg := callErr(t, cs, "job_start", startArgs(func(a map[string]any) { a["repo"], a["branch"] = repo, name }))
		if !strings.Contains(msg, "branch") {
			t.Fatalf("%s: %q", name, msg)
		}
	}
}
