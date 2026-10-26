package main

import (
	"context"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/qwwiwi/agent-jobs/internal/jobs"
	"github.com/qwwiwi/agent-jobs/internal/store"
)

// G01 on the command line: `agent-jobs milestone <id>` shows each task's planned skills and, per job,
// the skills it got: one list when selected and delivered match, both labelled when they differ.
func TestMilestoneCLISkills(t *testing.T) {
	t.Run("[G01][R01] milestone <id> shows the tree with planned and delivered skills", func(t *testing.T) {
		t.Setenv("AGENT_JOBS_HOME", t.TempDir())
		m, err := jobs.Open("test")
		if err != nil {
			t.Fatal(err)
		}
		ctx := context.Background()
		v, err := m.CreateMilestone(ctx, jobs.MilestoneCreate{Title: "Stage", Tasks: []jobs.MilestoneTaskInput{
			{Key: "T1", Title: "one"}, {Key: "T2", Title: "two"}}})
		if err != nil {
			t.Fatal(err)
		}
		// No catalog in this test: the plan is written directly (milestone_create checks it against one).
		if err := m.Store.UpdateMilestone(ctx, v.ID, time.Now(), func(ms *store.Milestone, tasks []store.MilestoneTask,
			_ []*store.Job) (*store.Milestone, []store.MilestoneTask, error) {
			tasks[0].Skills = []string{"+golang", "-legacy"}
			return ms, tasks[:1], nil
		}); err != nil {
			t.Fatal(err)
		}
		// job-c: golang is repository-owned, so only extra is linked; both lists must stay visible.
		for _, j := range []struct {
			id                  string
			selected, delivered []string
		}{
			{"job-a", []string{"golang"}, []string{"golang"}},
			{"job-b", []string{"golang"}, nil},
			{"job-c", []string{"golang", "extra"}, []string{"extra"}},
		} {
			if err := m.Store.Insert(&store.Job{ID: j.id, Agent: "test", Title: "t", Kind: "fix", Brief: "b", Status: store.StatusQueued,
				Model: "opus", MaxTurns: 1, MaxBudgetUSD: 1, TimeoutMinutes: 1, CreatedAt: time.Now(), MilestoneID: v.ID, TaskKey: "T1",
				Skills: j.selected, SkillsDelivered: j.delivered}); err != nil {
				t.Fatal(err)
			}
		}
		_ = m.Close()
		out := captureStdout(t, func() error { return cmdMilestone([]string{"--agent", "test", strconv.FormatInt(v.ID, 10)}) })
		lines := strings.Split(out, "\n")
		var t1, t2 string
		for _, l := range lines {
			switch {
			case strings.HasPrefix(l, "T1 "):
				t1 = l
			case strings.HasPrefix(l, "T2 "):
				t2 = l
			}
		}
		if !strings.Contains(out, "SKILLS") || !strings.Contains(t1, "+golang,-legacy") || !strings.Contains(t1, "job-a queued [golang]") ||
			!strings.Contains(t1, "job-b queued [selected golang; delivered none]") ||
			!strings.Contains(t1, "job-c queued [selected golang,extra; delivered extra]") || strings.Contains(t2, "golang") {
			t.Fatalf("milestone output:\n%s", out)
		}
	})
}
