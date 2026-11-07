package store

import (
	"database/sql"
	"path/filepath"
	"testing"
	"time"
)

func TestResultBranchMigrationAndRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "jobs.db")
	raw, err := sql.Open("sqlite", "file:"+path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := raw.Exec(stage1Schema); err != nil {
		t.Fatal(err)
	}
	_ = raw.Close()
	for i := range 2 {
		s, err := Open(path)
		if err != nil {
			t.Fatal(err)
		}
		j, err := s.Get("old-1")
		if err != nil {
			t.Fatal(err)
		}
		if i == 0 {
			if j.Branch != "" || j.RequestedBranch != "" || j.BranchNote != "" {
				t.Fatalf("migration defaults: %+v", j)
			}
			j.ID, j.RequestedBranch = "branch-job", "feature"
			if err := s.Insert(j); err != nil {
				t.Fatal(err)
			}
			if err := s.SetResultBranch(j.ID, "job/branch-job", "requested branch feature not moved: not a fast-forward"); err != nil {
				t.Fatal(err)
			}
		}
		j, err = s.Get("branch-job")
		if err != nil || j.RequestedBranch != "feature" || j.Branch != "job/branch-job" || j.BranchNote == "" {
			t.Fatalf("round trip: %+v %v", j, err)
		}
		_ = s.Close()
	}
}

func TestPendingResultBranch(t *testing.T) {
	st, err := Open(filepath.Join(t.TempDir(), "jobs.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	now := time.Date(2026, 11, 7, 7, 0, 0, 0, time.UTC)
	for _, c := range []struct {
		id, status, repo, base, branch, note string
		cleaned                              bool
		want                                 bool
	}{
		{"stopped", StatusStopped, "/r", "abc", "", "", false, true}, // PendingNotify skips this one
		{"succeeded", StatusSucceeded, "/r", "abc", "", "", false, true},
		{"cancelled", StatusCancelled, "/r", "abc", "", "", false, true},
		{"recorded", StatusSucceeded, "/r", "abc", "job/recorded", "", false, false},
		{"noted", StatusSucceeded, "/r", "abc", "", "result branch not published: x", false, false},
		{"cleaned", StatusSucceeded, "/r", "abc", "", "", true, false},
		{"running", StatusRunning, "/r", "abc", "", "", false, false},
		{"queued", StatusQueued, "/r", "abc", "", "", false, false},
		{"no-repo", StatusSucceeded, "", "abc", "", "", false, false},
		{"no-base", StatusSucceeded, "/r", "", "", "", false, false},
	} {
		j := &Job{ID: c.id, Agent: "test", Title: c.id, Kind: "fix", Brief: "b", Status: c.status, Model: "sonnet",
			CreatedAt: now, FinishedAt: &now, RepoPath: c.repo, BaseCommit: c.base, Branch: c.branch, BranchNote: c.note}
		if c.cleaned {
			j.CleanedAt = &now
		}
		if err := st.Insert(j); err != nil {
			t.Fatal(err)
		}
	}
	got, err := st.PendingResultBranch()
	if err != nil {
		t.Fatal(err)
	}
	ids := map[string]bool{}
	for _, j := range got {
		ids[j.ID] = true
	}
	if len(ids) != 3 || !ids["stopped"] || !ids["succeeded"] || !ids["cancelled"] {
		t.Fatalf("pending result branches: %v", ids)
	}
}
