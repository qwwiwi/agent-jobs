package httpapi

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/qwwiwi/agent-jobs/internal/jobs"
	"github.com/qwwiwi/agent-jobs/internal/store"
)

// G01: the read-only API shows each task's planned skills and, per job, the skills it got
// (skills, skills_delivered), names capped like every text field.
func TestMilestoneSkillsOnAPI(t *testing.T) {
	t.Run("[G01] /v1/milestones/{id} shows planned and delivered skills", func(t *testing.T) {
		f := newFixture(t)
		long := strings.Repeat("s", MaxFieldBytes+50)
		v, err := f.m.CreateMilestone(context.Background(), jobs.MilestoneCreate{Title: "Stage", Tasks: []jobs.MilestoneTaskInput{
			{Key: "T1", Title: "one"}}})
		if err != nil {
			t.Fatal(err)
		}
		// Planned skills are checked against the catalog at milestone_create; written directly here to test the cap.
		if err := f.m.Store.UpdateMilestone(context.Background(), v.ID, time.Now(), func(ms *store.Milestone, tasks []store.MilestoneTask,
			_ []*store.Job) (*store.Milestone, []store.MilestoneTask, error) {
			tasks[0].Skills = []string{"+golang", long}
			return ms, tasks, nil
		}); err != nil {
			t.Fatal(err)
		}
		if err := f.m.Store.Insert(&store.Job{ID: "job-1", Agent: "test", Title: "t", Kind: "fix", Brief: "b", Status: store.StatusQueued,
			Model: "opus", MaxTurns: 1, MaxBudgetUSD: 1, TimeoutMinutes: 1, CreatedAt: time.Now(), MilestoneID: v.ID, TaskKey: "T1",
			Skills: []string{"golang", long}, SkillsDelivered: []string{"golang"}}); err != nil {
			t.Fatal(err)
		}
		got := f.get(t, fmt.Sprintf("/v1/milestones/%d", v.ID), http.StatusOK)
		t1 := got["tasks"].([]any)[0].(map[string]any)
		planned := t1["skills"].([]any)
		if len(planned) != 2 || planned[0] != "+golang" || len(planned[1].(string)) > MaxFieldBytes {
			t.Fatalf("planned skills: %v", planned)
		}
		j := t1["jobs"].([]any)[0].(map[string]any)
		if fmt.Sprint(j["skills_delivered"]) != "[golang]" || len(j["skills"].([]any)[1].(string)) > MaxFieldBytes {
			t.Fatalf("job skills: %v", j)
		}
	})
}
