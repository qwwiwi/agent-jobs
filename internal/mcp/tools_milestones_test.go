package mcp

import (
	"slices"
	"strings"
	"testing"

	"github.com/qwwiwi/agent-jobs/internal/jobs"
)

// The milestone tools over a real store: create with tasks in one call, get, set (upsert, drop), list
// with tree; job_start takes milestone + task and refuses a blocked task; a job does not see the tools.
func TestMilestoneTools(t *testing.T) {
	m := newManager(t)
	cs := connect(t, mustServer(t, m))
	var v jobs.MilestoneView
	call(t, cs, "milestone_create", map[string]any{"title": "Stage", "tasks": []any{
		map[string]any{"key": "T1", "title": "one"},
		map[string]any{"key": "T2", "title": "two", "depends_on": []any{"T1"}},
	}}, &v)
	if v.ID == 0 || len(v.Tasks) != 2 || v.Tasks[1].Status != jobs.TaskBlocked || !slices.Equal(v.Ready, []string{"T1"}) {
		t.Fatalf("milestone_create: %+v", v)
	}
	if msg := callErr(t, cs, "milestone_create", map[string]any{"title": "x", "tasks": []any{
		map[string]any{"key": "T1", "title": "a", "depends_on": []any{"T1"}}}}); !strings.Contains(msg, "itself") {
		t.Fatalf("self dependency: %s", msg)
	}
	var got jobs.MilestoneView
	call(t, cs, "milestone_get", map[string]any{"id": v.ID}, &got)
	if got.ID != v.ID || got.Progress == nil || got.Progress.TasksTotal != 2 {
		t.Fatalf("milestone_get: %+v", got)
	}
	call(t, cs, "milestone_set", map[string]any{"id": v.ID, "tasks": []any{map[string]any{"key": "T3", "title": "three"}},
		"drop": []any{"T2"}}, &got)
	if len(got.Tasks) != 3 || got.Tasks[1].Status != jobs.TaskDropped || !slices.Equal(got.Ready, []string{"T1", "T3"}) {
		t.Fatalf("milestone_set: %+v", got)
	}
	var list jobs.MilestonesView
	call(t, cs, "milestone_list", map[string]any{"tree": true}, &list)
	if len(list.Milestones) != 1 || len(list.Milestones[0].Tasks) != 3 {
		t.Fatalf("milestone_list tree: %+v", list)
	}
	call(t, cs, "milestone_set", map[string]any{"id": v.ID, "tasks": []any{map[string]any{"key": "T2", "depends_on": []any{"T1"}}}}, &got)
	if msg := callErr(t, cs, "job_start", startArgs(func(a map[string]any) { a["milestone"], a["task"] = v.ID, "T2" })); !strings.Contains(msg, "blocked") {
		t.Fatalf("job_start on a blocked task: %s", msg)
	}
	var jv JobView
	call(t, cs, "job_start", startArgs(func(a map[string]any) { a["milestone"], a["task"] = v.ID, "T1" }), &jv)
	if jv.Milestone != v.ID || jv.Task != "T1" {
		t.Fatalf("job_start view: %+v", jv)
	}
	call(t, cs, "milestone_get", map[string]any{"id": v.ID}, &got)
	if got.Tasks[0].Status != jobs.TaskRunning || len(got.Tasks[0].Jobs) != 1 || got.Tasks[0].Jobs[0].ID != jv.ID {
		t.Fatalf("task after job_start: %+v", got.Tasks[0])
	}
	for _, name := range []string{"milestone_create", "milestone_set", "milestone_get", "milestone_list"} {
		if Allowed(RoleJob, name) || !Allowed(RoleOrchestrator, name) {
			t.Errorf("%s: wrong audience", name)
		}
	}
}
