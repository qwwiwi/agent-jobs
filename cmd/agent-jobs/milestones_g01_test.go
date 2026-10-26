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
// the skills it got (linked, else selected).
func TestMilestoneCLISkills(t *testing.T) {
	t.Run("[G01] milestone shows planned and delivered skills", func(t *testing.T) {
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
		for id, delivered := range map[string][]string{"job-a": {"golang"}, "job-b": nil} {
			if err := m.Store.Insert(&store.Job{ID: id, Agent: "test", Title: "t", Kind: "fix", Brief: "b", Status: store.StatusQueued,
				Model: "opus", MaxTurns: 1, MaxBudgetUSD: 1, TimeoutMinutes: 1, CreatedAt: time.Now(), MilestoneID: v.ID, TaskKey: "T1",
				Skills: []string{"golang"}, SkillsDelivered: delivered}); err != nil {
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
			!strings.Contains(t1, "job-b queued [selected golang]") || strings.Contains(t2, "golang") {
			t.Fatalf("milestone output:\n%s", out)
		}
	})
}
