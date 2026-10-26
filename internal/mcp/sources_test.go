package mcp

import (
	"encoding/json"
	"slices"
	"strings"
	"testing"

	"github.com/qwwiwi/agent-jobs/internal/jobs"
)

// G02 over MCP: job_start takes sources (https only), milestone_create plans them per task and the tree
// shows them; a job on the task gets them.
func TestSourcesOverMCP(t *testing.T) {
	m := newManager(t)
	cs := connect(t, mustServer(t, m))
	t.Run("[G02] job_start takes https sources and refuses ssh, file and local ones", func(t *testing.T) {
		var jv JobView
		call(t, cs, "job_start", startArgs(func(a map[string]any) {
			a["kind"] = "research"
			a["sources"] = []any{"https://github.com/qwwiwi/agent-jobs"}
		}), &jv)
		if !slices.Equal(jv.Sources, []string{"https://github.com/qwwiwi/agent-jobs"}) {
			t.Fatalf("job view sources: %+v", jv.Sources)
		}
		for _, bad := range []string{"git@github.com:a/b.git", "file:///tmp/x", "/home/x/repo", "https://u:p@github.com/a/b"} {
			if msg := callErr(t, cs, "job_start", startArgs(func(a map[string]any) { a["sources"] = []any{bad} })); msg == "" {
				t.Errorf("%s accepted", bad)
			}
		}
	})
	t.Run("[G02] milestone_create plans sources per task; the tree JSON shows them", func(t *testing.T) {
		var v jobs.MilestoneView
		call(t, cs, "milestone_create", map[string]any{"title": "Study", "tasks": []any{
			map[string]any{"key": "T1", "title": "read lib", "sources": []any{"https://github.com/a/lib.git"}},
			map[string]any{"key": "T2", "title": "other"},
		}}, &v)
		var list map[string]any
		call(t, cs, "milestone_list", map[string]any{"tree": true}, &list)
		b, _ := json.Marshal(list)
		if !strings.Contains(string(b), `"sources":["https://github.com/a/lib.git"]`) || !strings.Contains(string(b), `"sources":[]`) {
			t.Fatalf("tree JSON lacks the planned sources: %s", b)
		}
		var jv JobView
		call(t, cs, "job_start", startArgs(func(a map[string]any) { a["milestone"], a["task"], a["kind"] = v.ID, "T1", "research" }), &jv)
		if !slices.Equal(jv.Sources, []string{"https://github.com/a/lib.git"}) {
			t.Fatalf("job on T1: %+v", jv.Sources)
		}
	})
}
