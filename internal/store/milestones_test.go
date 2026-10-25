package store

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"testing"
	"time"
)

func milestoneJob(id string, ms int64, key string, now time.Time) *Job {
	return &Job{ID: id, Agent: "test", Title: "t", Kind: "fix", Brief: "b", Status: StatusQueued, Model: "sonnet",
		MaxTurns: 1, MaxBudgetUSD: 1, TimeoutMinutes: 1, CreatedAt: now, MilestoneID: ms, TaskKey: key}
}

// An older database gains the milestone tables and the jobs columns on Open; milestones and their
// tasks round-trip, list in repo / order / creation order with the filters, and jobs attach by id.
func TestMilestonesRoundTripOnOldDatabase(t *testing.T) {
	path := filepath.Join(t.TempDir(), "jobs.db")
	raw, err := sql.Open("sqlite", "file:"+path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := raw.Exec(stage1Schema); err != nil {
		t.Fatal(err)
	}
	_ = raw.Close()
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	ctx := context.Background()
	if j, err := s.Get("old-1"); err != nil || j.MilestoneID != 0 || j.TaskKey != "" {
		t.Fatalf("old job: %+v %v", j, err)
	}
	t0 := time.Unix(1770000000, 0)
	two := 2
	a, err := s.CreateMilestone(&Milestone{Title: "second", Repo: "/r/a", Order: 2, ChecklistID: "c1", MaxParallel: &two},
		[]MilestoneTask{{Key: "T1", Title: "one"}, {Key: "T2", Title: "two", DependsOn: []string{"T1"}, Covers: []string{"R01", "R02"}}}, t0)
	if err != nil {
		t.Fatal(err)
	}
	b, err := s.CreateMilestone(&Milestone{Title: "first", Repo: "/r/a", Order: 1}, []MilestoneTask{{Key: "T1", Title: "x"}}, t0.Add(time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	c, err := s.CreateMilestone(&Milestone{Title: "no repo"}, []MilestoneTask{{Key: "T1", Title: "x"}}, t0.Add(2*time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	ms, err := s.GetMilestone(ctx, a)
	if err != nil || ms.Title != "second" || ms.Status != MilestoneOpen || ms.MaxParallel == nil || *ms.MaxParallel != 2 ||
		ms.ChecklistID != "c1" || !ms.CreatedAt.Equal(t0) || ms.ClosedAt != nil {
		t.Fatalf("get: %+v %v", ms, err)
	}
	tasks, err := s.MilestoneTasks(ctx, a)
	if err != nil || len(tasks) != 2 || tasks[1].Key != "T2" || len(tasks[1].DependsOn) != 1 || len(tasks[1].Covers) != 2 {
		t.Fatalf("tasks: %+v %v", tasks, err)
	}
	if _, err := s.GetMilestone(ctx, 999); !errors.Is(err, ErrMilestoneNotFound) {
		t.Fatalf("missing milestone: %v", err)
	}
	list, err := s.ListMilestones(ctx, MilestoneFilter{})
	if err != nil || len(list) != 3 || list[0].ID != c || list[1].ID != b || list[2].ID != a {
		t.Fatalf("order (repo, order, created): %+v %v", list, err)
	}
	if list, _ := s.ListMilestones(ctx, MilestoneFilter{Repo: "/r/a", ChecklistID: "c1"}); len(list) != 1 || list[0].ID != a {
		t.Fatalf("repo + checklist filter: %+v", list)
	}
	// Created at the database's clock; set back to the creation times to test the cursor's edge.
	if _, err := s.db.Exec(`UPDATE milestones SET updated_at = created_at`); err != nil {
		t.Fatal(err)
	}
	since := t0.Add(time.Minute)
	if list, _ := s.ListMilestones(ctx, MilestoneFilter{UpdatedSince: &since}); len(list) != 2 {
		t.Fatalf("updated_since includes the equal second: %+v", list)
	}
	if err := s.Insert(milestoneJob("j1", a, "T1", t0)); err != nil {
		t.Fatal(err)
	}
	jobs, err := s.MilestoneJobs(ctx, a)
	if err != nil || len(jobs) != 1 || jobs[0].ID != "j1" || jobs[0].MilestoneID != a || jobs[0].TaskKey != "T1" {
		t.Fatalf("jobs: %+v %v", jobs, err)
	}
	// UpdateMilestone writes the row and the returned tasks; an edit error changes nothing.
	t1 := t0.Add(time.Hour)
	err = s.UpdateMilestone(ctx, a, t1, func(ms *Milestone, tasks []MilestoneTask, jobs []*Job) (*Milestone, []MilestoneTask, error) {
		if len(tasks) != 2 || len(jobs) != 1 {
			t.Errorf("edit sees %d tasks, %d jobs", len(tasks), len(jobs))
		}
		next := *ms
		next.Title, next.Status, next.ClosedAt = "renamed", MilestoneDropped, &t1
		return &next, []MilestoneTask{{Key: "T3", Title: "three"}, {Key: "T1", Title: "one", Dropped: true}}, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	ms, _ = s.GetMilestone(ctx, a)
	tasks, _ = s.MilestoneTasks(ctx, a)
	if ms.Title != "renamed" || ms.Status != MilestoneDropped || ms.ClosedAt == nil || ms.UpdatedAt.Unix() < t1.Unix() ||
		len(tasks) != 3 || !tasks[0].Dropped || !tasks[0].CreatedAt.Equal(t0) || tasks[2].Key != "T3" {
		t.Fatalf("after update: %+v %+v", ms, tasks)
	}
	boom := errors.New("boom")
	if err := s.UpdateMilestone(ctx, a, t1, func(*Milestone, []MilestoneTask, []*Job) (*Milestone, []MilestoneTask, error) {
		return nil, nil, boom
	}); !errors.Is(err, boom) {
		t.Fatalf("edit error: %v", err)
	}
}

// A job attached to a milestone moves the milestone's updated_at when it is inserted and when its
// status or outcome changes, through any store method (triggers); other jobs do not.
func TestMilestoneUpdatedAtFollowsJobs(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "jobs.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	ctx := context.Background()
	old := time.Now().Add(-time.Hour)
	id, err := s.CreateMilestone(&Milestone{Title: "m"}, []MilestoneTask{{Key: "T1", Title: "x"}}, old)
	if err != nil {
		t.Fatal(err)
	}
	updated := func() int64 {
		t.Helper()
		ms, err := s.GetMilestone(ctx, id)
		if err != nil {
			t.Fatal(err)
		}
		return ms.UpdatedAt.Unix()
	}
	rewind := func() {
		t.Helper()
		if _, err := s.db.Exec(`UPDATE milestones SET updated_at=? WHERE id=?`, old.Unix(), id); err != nil {
			t.Fatal(err)
		}
	}
	rewind() // created at the database's clock
	if err := s.Insert(milestoneJob("other", 0, "", old)); err != nil {
		t.Fatal(err)
	}
	if err := s.Finish("other", "", StatusCancelled, 0, 0, 0, "", old); err != nil {
		t.Fatal(err)
	}
	if updated() != old.Unix() {
		t.Fatal("a job of no milestone moved updated_at")
	}
	if err := s.Insert(milestoneJob("j1", id, "T1", old)); err != nil {
		t.Fatal(err)
	}
	if updated() <= old.Unix() {
		t.Fatal("insert did not move updated_at")
	}
	for _, step := range []struct {
		name string
		do   func() error
	}{
		{"claim", func() error { return s.Claim("j1", "tok", old) }},
		{"finish", func() error { return s.Finish("j1", "tok", StatusSucceeded, 1, 0.1, 1, "", old) }},
		{"outcome", func() error { return s.SetOutcome("j1", "test", "accepted", "", old) }},
	} {
		rewind()
		if err := step.do(); err != nil {
			t.Fatalf("%s: %v", step.name, err)
		}
		if updated() <= old.Unix() {
			t.Fatalf("%s did not move updated_at", step.name)
		}
	}
	// A change that is neither status nor outcome leaves it.
	rewind()
	if err := s.SetMergeNote("j1", "note"); err != nil {
		t.Fatal(err)
	}
	if updated() != old.Unix() {
		t.Fatal("a merge note moved updated_at")
	}
}

// MilestoneFull: a milestone with max_parallel is full at that many running jobs; one without never is.
func TestMilestoneFull(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "jobs.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	now := time.Now()
	one := 1
	capped, _ := s.CreateMilestone(&Milestone{Title: "c", MaxParallel: &one}, []MilestoneTask{{Key: "T1", Title: "x"}}, now)
	free, _ := s.CreateMilestone(&Milestone{Title: "f"}, []MilestoneTask{{Key: "T1", Title: "x"}}, now)
	for _, j := range []*Job{milestoneJob("a", capped, "T1", now), milestoneJob("b", free, "T1", now)} {
		if err := s.Insert(j); err != nil {
			t.Fatal(err)
		}
		if full, err := s.MilestoneFull(j.MilestoneID); err != nil || full {
			t.Fatalf("%s queued: full=%v %v", j.ID, full, err)
		}
		if err := s.Claim(j.ID, "tok", now); err != nil {
			t.Fatal(err)
		}
	}
	if full, _ := s.MilestoneFull(capped); !full {
		t.Fatal("max_parallel 1 with one running job is not full")
	}
	if full, _ := s.MilestoneFull(free); full {
		t.Fatal("a milestone without max_parallel is full")
	}
}

// A judge verdict of a milestone's repository and checklist moves its updated_at (acceptance_met reads
// the latest verdict); a milestone of another repository stays.
func TestMilestoneUpdatedAtFollowsVerdicts(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "jobs.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	ctx := context.Background()
	old := time.Now().Add(-time.Hour)
	tied, err := s.CreateMilestone(&Milestone{Title: "tied", Repo: "/r/a", ChecklistID: "c1"}, []MilestoneTask{{Key: "T1", Title: "x"}}, old)
	if err != nil {
		t.Fatal(err)
	}
	other, err := s.CreateMilestone(&Milestone{Title: "other", Repo: "/r/b", ChecklistID: "c1"}, []MilestoneTask{{Key: "T1", Title: "x"}}, old)
	if err != nil {
		t.Fatal(err)
	}
	rewind := func() {
		t.Helper()
		if _, err := s.db.Exec(`UPDATE milestones SET updated_at=?`, old.Unix()); err != nil {
			t.Fatal(err)
		}
	}
	updated := func(id int64) int64 {
		t.Helper()
		ms, err := s.GetMilestone(ctx, id)
		if err != nil {
			t.Fatal(err)
		}
		return ms.UpdatedAt.Unix()
	}
	judge := milestoneJob("judge-1", 0, "", old)
	judge.Status, judge.LaunchToken, judge.Attempt = StatusRunning, "tok", 1
	if err := s.Insert(judge); err != nil {
		t.Fatal(err)
	}
	rewind() // the cursor has passed the creation
	if _, err := s.PutVerdict("tok", JudgeVerdict{JobID: judge.ID, Repo: "/r/a", SHA: "abc", ChecklistID: "c1", Version: 1,
		ChecklistSHA256: "h", Verdict: map[string]any{"requirements": []any{}}}); err != nil {
		t.Fatal(err)
	}
	if updated(tied) <= old.Unix() {
		t.Fatal("a verdict of the milestone's checklist did not move updated_at")
	}
	if updated(other) != old.Unix() {
		t.Fatal("a verdict of another repository moved updated_at")
	}
}
