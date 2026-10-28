package mcp

import (
	"slices"
	"testing"

	"github.com/qwwiwi/agent-jobs/internal/jobs"
)

// G01 over MCP: milestone_create takes each task's skills (job_start's edit syntax), milestone_get shows
// them, and job_start on the task inherits them when it gives no skills; an explicit [] keeps the defaults.
func TestMilestoneToolsTaskSkills(t *testing.T) {
	t.Run("[G01] task skills planned at milestone_create reach job_start", func(t *testing.T) {
		m := newManager(t)
		m.Cfg.SkillRoots = []string{catalogRoot(t, map[string][3]string{
			"golang": {"core", "code", "Go code conventions."},
			"extra":  {"active", "code", "Extra helpers."},
		})}
		cs := connect(t, mustServer(t, m))
		var v jobs.MilestoneView
		call(t, cs, "milestone_create", map[string]any{"title": "Stage", "tasks": []any{
			map[string]any{"key": "T1", "title": "one", "skills": []any{"+extra"}},
			map[string]any{"key": "T2", "title": "two"},
		}}, &v)
		if !slices.Equal(v.Tasks[0].Skills, []string{"+extra"}) || len(v.Tasks[1].Skills) != 0 {
			t.Fatalf("milestone_create tasks: %+v", v.Tasks)
		}
		var jv JobView
		call(t, cs, "job_start", startArgs(func(a map[string]any) { a["kind"], a["milestone"], a["task"] = "fix", v.ID, "T1" }), &jv)
		if !slices.Equal(jv.Skills, []string{"extra", "golang"}) {
			t.Fatalf("inherited skills: %v", jv.Skills)
		}
		call(t, cs, "job_start", startArgs(func(a map[string]any) {
			a["kind"], a["milestone"], a["task"], a["skills"] = "fix", v.ID, "T1", []any{}
		}), &jv)
		if !slices.Equal(jv.Skills, []string{"golang"}) {
			t.Fatalf("skills [] overrides the plan with the defaults: %v", jv.Skills)
		}
		var got jobs.MilestoneView
		call(t, cs, "milestone_get", map[string]any{"id": v.ID}, &got)
		if js := got.Tasks[0].Jobs; len(js) != 2 || !slices.Equal(js[0].Skills, []string{"extra", "golang"}) {
			t.Fatalf("milestone_get jobs: %+v", js)
		}
	})
}
