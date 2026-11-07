package store

import (
	"errors"
	"path/filepath"
	"reflect"
	"testing"
	"time"
)

func TestJobDependenciesRoundTripAndMigration(t *testing.T) {
	dbpath := filepath.Join(t.TempDir(), "jobs.db")
	now := time.Now()
	j := &Job{ID: "stack", Agent: "test", Title: "stack", Kind: "fix", Brief: "edit", Model: "sonnet", Status: StatusQueued, CreatedAt: now,
		Paths: []string{"file,with,commas.go", "path with spaces/"}, OnOverlap: "serialize", Parent: "parent", ParentBase: "result",
		WaitFor: []string{"older"}, Overlap: []JobOverlap{{ID: "older", Paths: []string{"file,with,commas.go"}, OtherPaths: []string{"*.go"}}}}
	for attempt := range 2 {
		s, err := Open(dbpath)
		if err != nil {
			t.Fatal(err)
		}
		if attempt == 0 {
			if err := s.Insert(j); err != nil {
				t.Fatal(err)
			}
		}
		got, err := s.Get(j.ID)
		if err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(got.Paths, j.Paths) || !reflect.DeepEqual(got.Overlap, j.Overlap) || !reflect.DeepEqual(got.WaitFor, j.WaitFor) || got.Parent != j.Parent || got.ParentBase != j.ParentBase || got.OnOverlap != j.OnOverlap {
			t.Fatalf("dependency roundtrip: %+v", got)
		}
		if err := s.Close(); err != nil {
			t.Fatal(err)
		}
	}
}

func TestJobAdmissionIncludesCapsAndRollsBack(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "jobs.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	now := time.Now()
	for _, mode := range []string{"plain", "codex", "budget"} {
		t.Run(mode, func(t *testing.T) {
			j := &Job{ID: mode, Agent: "test", RepoPath: "/repo", Title: "test", Kind: "general", Brief: "edit", Model: "sonnet", Status: StatusQueued, CreatedAt: now}
			if mode == "codex" {
				j.Runner = RunnerCodex
			}
			if mode == "budget" {
				j.Provider = ProviderOpenRouter
				j.MaxBudgetUSD = 1
			}
			rejected := errors.New("overlap refused")
			check := func(others []*Job, parent *Job, _ map[int64]AdmissionMilestone) error { return rejected }
			insert := func(c JobAdmission) (bool, error) {
				switch mode {
				case "codex":
					return s.InsertCapped(j, now.Add(-time.Hour), 10, c)
				case "budget":
					return s.InsertBudgetCapped(j, now.Add(-time.Hour), 10, c)
				default:
					err := s.Insert(j, c)
					return err == nil, err
				}
			}
			if ok, err := insert(check); ok || !errors.Is(err, rejected) {
				t.Fatalf("admission bypassed: %v %v", ok, err)
			}
			if _, err := s.Get(j.ID); !errors.Is(err, ErrNotFound) {
				t.Fatalf("rejected insert persisted: %v", err)
			}
			if ok, err := insert(func(others []*Job, parent *Job, _ map[int64]AdmissionMilestone) error {
				j.Paths = []string{"a,b.go"}
				j.WaitFor = []string{"older"}
				return nil
			}); !ok || err != nil {
				t.Fatalf("admission: %v %v", ok, err)
			}
			got, err := s.Get(j.ID)
			if err != nil || len(got.Paths) != 1 || got.Paths[0] != "a,b.go" || len(got.WaitFor) != 1 {
				t.Fatalf("admission writes not inserted: %+v %v", got, err)
			}
		})
	}
}

func TestClaimChecksJobDependenciesOnMilestone(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "jobs.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	now := time.Now()
	parent := &Job{ID: "parent", Agent: "test", Title: "parent", Kind: "fix", Brief: "edit", Model: "sonnet", Status: StatusQueued, CreatedAt: now}
	child := &Job{ID: "child", Agent: "test", Title: "child", Kind: "fix", Brief: "edit", Model: "sonnet", Status: StatusQueued, CreatedAt: now, WaitFor: []string{parent.ID}}
	for _, j := range []*Job{parent, child} {
		if err := s.Insert(j); err != nil {
			t.Fatal(err)
		}
	}
	check := func(*Milestone, []MilestoneTask, []*Job) error { return nil }
	if err := s.ClaimOnMilestone(child.ID, "early", now, 0, check); !errors.Is(err, ErrImmutable) {
		t.Fatalf("milestone claim bypassed blocker: %v", err)
	}
	if err := s.Finish(parent.ID, "", StatusCancelled, 0, 0, 0, "", now); err != nil {
		t.Fatal(err)
	}
	if err := s.ClaimOnMilestone(child.ID, "ready", now, 0, check); err != nil {
		t.Fatal(err)
	}
}

// A blocker resumed after its serialized successor started must not claim while the successor runs.
func TestClaimWaitsForRunningSuccessorOfResumedBlocker(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "jobs.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	now := time.Now()
	blocker := &Job{ID: "blocker", Agent: "test", Title: "blocker", Kind: "fix", Brief: "edit", Model: "sonnet", Status: StatusQueued, CreatedAt: now}
	next := &Job{ID: "next", Agent: "test", Title: "next", Kind: "fix", Brief: "edit", Model: "sonnet", Status: StatusQueued, CreatedAt: now, WaitFor: []string{blocker.ID}}
	for _, j := range []*Job{blocker, next} {
		if err := s.Insert(j); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.Claim(blocker.ID, "b1", now); err != nil {
		t.Fatal(err)
	}
	if err := s.Finish(blocker.ID, "b1", StatusFailed, 0, 0, 0, "boom", now); err != nil {
		t.Fatal(err)
	}
	if err := s.Claim(next.ID, "n1", now); err != nil {
		t.Fatal(err)
	}
	if err := s.Requeue(blocker.ID, ResumeLimits{MaxTurns: 1, MaxBudgetUSD: 1, TimeoutMinutes: 1}, "", ""); err != nil {
		t.Fatal(err)
	}
	if err := s.Claim(blocker.ID, "b2", now); !errors.Is(err, ErrImmutable) {
		t.Fatalf("resumed blocker claimed next to the running successor: %v", err)
	}
	if got, err := s.ActiveSuccessors(blocker.ID); err != nil || len(got) != 1 || got[0].ID != next.ID {
		t.Fatalf("successors: %+v %v", got, err)
	}
	if err := s.Finish(next.ID, "n1", StatusSucceeded, 0, 0, 0, "", now); err != nil {
		t.Fatal(err)
	}
	if err := s.Claim(blocker.ID, "b3", now); err != nil {
		t.Fatalf("blocker still held after the successor ended: %v", err)
	}
}
