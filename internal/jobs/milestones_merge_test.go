package jobs

import (
	"context"
	"slices"
	"testing"
)

// G01 + G02: a milestone task plans skills and sources side by side. Each is kept by an edit that does
// not name it, and a job on the task inherits each one independently: a nil job_start field takes the
// task's plan, a given list (also []) replaces it.
func TestMilestoneTaskSkillsAndSources(t *testing.T) {
	ctx := context.Background()
	t.Run("[G01][G02] skills and sources are planned and inherited independently", func(t *testing.T) {
		m, _ := skillsManager(t)
		const lib = "https://github.com/a/lib"
		v, err := m.CreateMilestone(ctx, MilestoneCreate{Title: "m", Tasks: []MilestoneTaskInput{
			{Key: "T1", Title: "both", Skills: deps("+extra"), Sources: deps(lib)},
		}})
		if err != nil {
			t.Fatal(err)
		}
		task := taskOf(t, v, "T1")
		if !slices.Equal(task.Skills, []string{"+extra"}) || !slices.Equal(task.Sources, []string{lib}) {
			t.Fatalf("tree: %+v", task)
		}
		// An edit of one plan keeps the other.
		if v, err = m.SetMilestone(ctx, MilestoneSet{ID: v.ID, Tasks: []MilestoneTaskInput{{Key: "T1", Skills: deps("+drafty")}}}); err != nil {
			t.Fatal(err)
		}
		if task = taskOf(t, v, "T1"); !slices.Equal(task.Skills, []string{"+drafty"}) || !slices.Equal(task.Sources, []string{lib}) {
			t.Fatalf("skills edit: %+v", task)
		}
		if v, err = m.SetMilestone(ctx, MilestoneSet{ID: v.ID, Tasks: []MilestoneTaskInput{{Key: "T1", Sources: deps(lib + ".git")}}}); err != nil {
			t.Fatal(err)
		}
		if task = taskOf(t, v, "T1"); !slices.Equal(task.Skills, []string{"+drafty"}) || !slices.Equal(task.Sources, []string{lib + ".git"}) {
			t.Fatalf("sources edit: %+v", task)
		}
		onTask := func(skills, sources []string) ([]string, []string) {
			j := enqueue(t, m, "b", func(r *StartRequest) {
				r.Kind, r.Milestone, r.Task, r.Skills, r.Sources = "fix", v.ID, "T1", skills, sources
			})
			return j.Skills, j.Sources
		}
		planned := []string{"drafty", "git-workflows", "golang"}
		if sk, src := onTask(nil, nil); !slices.Equal(sk, planned) || !slices.Equal(src, []string{lib + ".git"}) {
			t.Fatalf("both inherited: %v %v", sk, src)
		}
		if sk, src := onTask([]string{}, nil); !slices.Equal(sk, []string{"git-workflows", "golang"}) || !slices.Equal(src, []string{lib + ".git"}) {
			t.Fatalf("own skills, planned sources: %v %v", sk, src)
		}
		if sk, src := onTask(nil, []string{}); !slices.Equal(sk, planned) || len(src) != 0 {
			t.Fatalf("planned skills, own sources: %v %v", sk, src)
		}
	})
}
