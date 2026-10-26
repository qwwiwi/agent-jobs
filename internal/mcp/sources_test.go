package mcp

import (
	"context"
	"encoding/json"
	"slices"
	"strings"
	"testing"

	"github.com/qwwiwi/agent-jobs/internal/jobs"
	"github.com/qwwiwi/agent-jobs/internal/store"
)

// G02 over MCP: job_start takes sources (https only), milestone_create plans them per task and the tree
// shows them; a job on the task gets them.
func TestSourcesOverMCP(t *testing.T) {
	t.Skip("G02 disabled until OS-level isolation (owner decision 2026-10-26): job_start and milestone_create refuse sources")
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

// G02 is deferred (owner decision 2026-10-26): job_start, milestone_create and milestone_set refuse a
// non-empty sources list before anything is stored, and their schemas say so.
func TestSourcesDisabledOverMCP(t *testing.T) {
	m := newManager(t)
	cs := connect(t, mustServer(t, m))
	const off = "sources is disabled until OS-level isolation lands"
	t.Run("[G02 off] the job_start and milestone schemas say sources is disabled", func(t *testing.T) {
		res, err := cs.ListTools(context.Background(), nil)
		if err != nil {
			t.Fatal(err)
		}
		seen := 0
		for _, tl := range res.Tools {
			if tl.Name != "job_start" && tl.Name != "milestone_create" && tl.Name != "milestone_set" {
				continue
			}
			seen++
			b, _ := json.Marshal(tl.InputSchema)
			if !strings.Contains(string(b), "currently DISABLED (G02 deferred, owner decision 2026-10-26") {
				t.Errorf("%s schema does not say sources is disabled", tl.Name)
			}
		}
		if seen != 3 {
			t.Fatalf("found %d of the 3 tools", seen)
		}
	})
	t.Run("[G02 off] job_start refuses sources and queues nothing", func(t *testing.T) {
		msg := callErr(t, cs, "job_start", startArgs(func(a map[string]any) {
			a["kind"] = "research"
			a["sources"] = []any{"https://github.com/qwwiwi/agent-jobs"}
		}))
		if !strings.Contains(msg, off) {
			t.Fatalf("job_start: %s", msg)
		}
		if all, err := m.Store.List(100); err != nil || len(all) != 0 {
			t.Fatalf("jobs after the refusal: %d %v", len(all), err)
		}
		var jv JobView
		call(t, cs, "job_start", startArgs(func(a map[string]any) { a["sources"] = []any{} }), &jv)
		if len(jv.Sources) != 0 {
			t.Fatalf("empty sources: %+v", jv.Sources)
		}
	})
	t.Run("[G02 off] milestone_create and milestone_set refuse planned sources and store nothing", func(t *testing.T) {
		msg := callErr(t, cs, "milestone_create", map[string]any{"title": "Study", "tasks": []any{
			map[string]any{"key": "T1", "title": "read lib", "sources": []any{"https://github.com/a/lib.git"}},
		}})
		if !strings.Contains(msg, off) {
			t.Fatalf("milestone_create: %s", msg)
		}
		if all, err := m.Store.ListMilestones(context.Background(), store.MilestoneFilter{}); err != nil || len(all) != 0 {
			t.Fatalf("milestones after the refusal: %d %v", len(all), err)
		}
		var v jobs.MilestoneView
		call(t, cs, "milestone_create", map[string]any{"title": "Study", "tasks": []any{
			map[string]any{"key": "T1", "title": "read lib", "sources": []any{}},
		}}, &v)
		msg = callErr(t, cs, "milestone_set", map[string]any{"id": v.ID, "tasks": []any{
			map[string]any{"key": "T1", "sources": []any{"https://github.com/a/lib.git"}},
		}})
		if !strings.Contains(msg, off) {
			t.Fatalf("milestone_set: %s", msg)
		}
		tasks, err := m.Store.MilestoneTasks(context.Background(), v.ID)
		if err != nil || len(tasks) != 1 || len(tasks[0].Sources) != 0 {
			t.Fatalf("task after the refusal: %+v %v", tasks, err)
		}
	})
}
