package jobs

import (
	"context"
	"slices"
	"strings"
	"testing"

	"github.com/qwwiwi/agent-jobs/internal/store"
)

// G01: skills are planned once per milestone. Each task carries an optional list of skills edits (the
// syntax of job_start skills), set at milestone_create or milestone_set; a job on the task inherits it
// unless job_start gives its own skills; the tree shows the plan and what each job got.
func TestMilestoneTaskSkills(t *testing.T) {
	ctx := context.Background()
	t.Run("[G01] tasks carry planned skills from create and set", func(t *testing.T) {
		m, _ := skillsManager(t)
		v, err := m.CreateMilestone(ctx, MilestoneCreate{Title: "m", Tasks: []MilestoneTaskInput{
			{Key: "T1", Title: "code", Skills: deps("+extra", " -golang ", "", "+extra")}, {Key: "T2", Title: "docs"},
		}})
		if err != nil {
			t.Fatal(err)
		}
		// Trimmed and blanks dropped, order and repeats kept: edits apply in order, as in job_start.
		if got := taskOf(t, v, "T1").Skills; !slices.Equal(got, []string{"+extra", "-golang", "+extra"}) {
			t.Fatalf("T1 planned skills %v", got)
		}
		if got := taskOf(t, v, "T2").Skills; got == nil || len(got) != 0 {
			t.Fatalf("T2 without a plan: %#v, want []", got)
		}
		v, err = m.SetMilestone(ctx, MilestoneSet{ID: v.ID, Tasks: []MilestoneTaskInput{
			{Key: "T1", Title: "code it"}, {Key: "T2", Skills: deps("drafty")},
		}})
		if err != nil {
			t.Fatal(err)
		}
		if got := taskOf(t, v, "T1").Skills; !slices.Equal(got, []string{"+extra", "-golang", "+extra"}) {
			t.Fatalf("a nil skills list must keep the plan: %v", got)
		}
		if got := taskOf(t, v, "T2").Skills; !slices.Equal(got, []string{"drafty"}) {
			t.Fatalf("T2 after set: %v", got)
		}
		v, err = m.SetMilestone(ctx, MilestoneSet{ID: v.ID, Tasks: []MilestoneTaskInput{{Key: "T1", Skills: deps()}}})
		if err != nil {
			t.Fatal(err)
		}
		if got := taskOf(t, v, "T1").Skills; len(got) != 0 {
			t.Fatalf("[] must clear the plan: %v", got)
		}
	})
	t.Run("[G01] an unknown or deprecated planned skill is refused", func(t *testing.T) {
		m, _ := skillsManager(t)
		for edit, want := range map[string]string{"golnag": "close: golang", "+old": "deprecated", "-nope": "unknown skill"} {
			_, err := m.CreateMilestone(ctx, MilestoneCreate{Title: "m", Tasks: []MilestoneTaskInput{
				{Key: "T1", Title: "x", Skills: deps(edit)}}})
			if err == nil || !strings.Contains(err.Error(), want) || !strings.Contains(err.Error(), "task T1") {
				t.Errorf("%s: %v", edit, err)
			}
		}
		if list, err := m.ListMilestones(ctx, MilestoneListRequest{}); err != nil || len(list.Milestones) != 0 {
			t.Fatalf("a refused create stored a milestone: %+v %v", list, err)
		}
		v, err := m.CreateMilestone(ctx, MilestoneCreate{Title: "m", Tasks: []MilestoneTaskInput{{Key: "T1", Title: "x"}}})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := m.SetMilestone(ctx, MilestoneSet{ID: v.ID, Tasks: []MilestoneTaskInput{{Key: "T1", Skills: deps("a,b")}}}); err == nil {
			t.Fatal("a skill name with a comma accepted")
		}
	})
	t.Run("[G01] job_start inherits the task's skills unless it gives its own", func(t *testing.T) {
		m, _ := skillsManager(t)
		v, err := m.CreateMilestone(ctx, MilestoneCreate{Title: "m", Tasks: []MilestoneTaskInput{
			{Key: "T1", Title: "code", Skills: deps("+extra", "-golang")}, {Key: "T2", Title: "plain"},
			{Key: "T3", Title: "ordered", Skills: deps("+extra", "-extra", "+extra")},
		}})
		if err != nil {
			t.Fatal(err)
		}
		onTask := func(key string, edits []string, brief string) *store.Job {
			return enqueue(t, m, brief, func(r *StartRequest) { r.Kind, r.Milestone, r.Task, r.Skills = "fix", v.ID, key, edits })
		}
		if got := onTask("T1", nil, "b").Skills; !slices.Equal(got, []string{"extra", "git-workflows"}) {
			t.Fatalf("inherited: %v", got)
		}
		if got := onTask("T1", []string{"+drafty"}, "b").Skills; !slices.Equal(got, []string{"drafty", "git-workflows", "golang"}) {
			t.Fatalf("job_start skills override the plan: %v", got)
		}
		if got := onTask("T1", []string{}, "b").Skills; !slices.Equal(got, []string{"git-workflows", "golang"}) {
			t.Fatalf("job_start skills [] means the defaults, not the plan: %v", got)
		}
		if got := onTask("T1", nil, "b\nskills: none").Skills; len(got) != 0 {
			t.Fatalf("a skills: none brief overrides the plan: %v", got)
		}
		if got := onTask("T2", nil, "b").Skills; !slices.Equal(got, []string{"git-workflows", "golang"}) {
			t.Fatalf("a task without a plan gets the defaults: %v", got)
		}
		if got := onTask("T3", nil, "b").Skills; !slices.Equal(got, []string{"extra", "git-workflows", "golang"}) {
			t.Fatalf("planned edits apply in order: %v", got)
		}
		// A refusal of a task without a plan does not blame a plan.
		m.Cfg.SkillsMaxPerJob = 1
		if _, err := m.Enqueue(StartRequest{Title: "t", Kind: "fix", Brief: "b", Model: "sonnet", Milestone: v.ID, Task: "T2"}); err == nil ||
			!strings.Contains(err.Error(), "skills_max_per_job") || strings.Contains(err.Error(), "planned") {
			t.Fatalf("over budget without a plan: %v", err)
		}
		m.Cfg.SkillsMaxPerJob = 0
		// A plan the catalog no longer accepts refuses the start and says where the names came from.
		if err := m.Store.UpdateMilestone(ctx, v.ID, m.Now(), func(ms *store.Milestone, tasks []store.MilestoneTask,
			_ []*store.Job) (*store.Milestone, []store.MilestoneTask, error) {
			tasks[1].Skills = []string{"+gone"}
			return ms, tasks[1:], nil
		}); err != nil {
			t.Fatal(err)
		}
		_, err = m.Enqueue(StartRequest{Title: "t", Kind: "fix", Brief: "b", Model: "sonnet", Milestone: v.ID, Task: "T2"})
		if err == nil || !strings.Contains(err.Error(), `unknown skill "gone"`) || !strings.Contains(err.Error(), "planned for task T2") {
			t.Fatalf("a stale plan: %v", err)
		}
		if got := onTask("T2", []string{}, "b").Skills; !slices.Equal(got, []string{"git-workflows", "golang"}) {
			t.Fatalf("job_start skills of its own pass a stale plan by: %v", got)
		}
	})
	t.Run("[G01] the tree shows each job's skills and skills_delivered", func(t *testing.T) {
		m, _ := skillsManager(t)
		v, err := m.CreateMilestone(ctx, MilestoneCreate{Title: "m", Tasks: []MilestoneTaskInput{
			{Key: "T1", Title: "code", Skills: deps("+extra")}}})
		if err != nil {
			t.Fatal(err)
		}
		j := enqueue(t, m, "b", func(r *StartRequest) { r.Kind, r.Milestone, r.Task = "general", v.ID, "T1" })
		js := taskOf(t, tree(t, m, v.ID), "T1").Jobs
		if len(js) != 1 || !slices.Equal(js[0].Skills, []string{"extra", "git-workflows", "golang"}) || len(js[0].SkillsDelivered) != 0 {
			t.Fatalf("queued job view: %+v", js)
		}
		if j = runToEnd(t, m, j.ID); j.Status != store.StatusSucceeded {
			t.Fatalf("job %s: %s", j.Status, j.Error)
		}
		js = taskOf(t, tree(t, m, v.ID), "T1").Jobs
		if !slices.Equal(js[0].SkillsDelivered, []string{"extra", "git-workflows", "golang"}) {
			t.Fatalf("finished job view: %+v (stored delivered %v)", js[0], j.SkillsDelivered)
		}
	})
}
