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
			if err := s.SetResultBranch(j.ID, "job/branch-job", "requested branch feature exists; not moved - published job/branch-job"); err != nil {
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
		cleaned, notified                    bool
		want                                 bool
	}{
		{"stopped", StatusStopped, "/r", "abc", "", "", false, false, true}, // PendingNotify skips this one
		{"succeeded", StatusSucceeded, "/r", "abc", "", "", false, false, true},
		{"cancelled", StatusCancelled, "/r", "abc", "", "", false, false, true},
		{"recorded", StatusSucceeded, "/r", "abc", "job/recorded", "", false, false, false},
		{"noted", StatusSucceeded, "/r", "abc", "", "result branch not published: x", false, false, false},
		{"cleaned", StatusSucceeded, "/r", "abc", "", "", true, false, false},
		{"running", StatusRunning, "/r", "abc", "", "", false, false, false},
		{"queued", StatusQueued, "/r", "abc", "", "", false, false, false},
		{"no-repo", StatusSucceeded, "", "abc", "", "", false, false, false},
		{"no-base", StatusSucceeded, "/r", "", "", "", false, false, false},
		// A row delivered before this feature existed must not be published retrospectively.
		{"legacy-notified", StatusSucceeded, "/r", "abc", "", "", false, true, false},
	} {
		j := &Job{ID: c.id, Agent: "test", Title: c.id, Kind: "fix", Brief: "b", Status: c.status, Model: "sonnet",
			CreatedAt: now, FinishedAt: &now, RepoPath: c.repo, BaseCommit: c.base, Branch: c.branch, BranchNote: c.note}
		if c.cleaned {
			j.CleanedAt = &now
		}
		if c.notified {
			j.NotifiedAt = &now
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

// Resume archives the published branch with the attempt that produced it and clears the job row for
// the next attempt, so every attempt keeps its own result branch (#205).
func TestRequeueArchivesResultBranchPerAttempt(t *testing.T) {
	st, err := Open(filepath.Join(t.TempDir(), "jobs.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	now := time.Unix(1760000000, 0)
	if err := st.Insert(&Job{ID: "j", Agent: "a", Title: "t", Kind: "fix", Brief: "b", Status: StatusQueued, Model: "m",
		MaxTurns: 80, MaxBudgetUSD: 10, TimeoutMinutes: 45, CreatedAt: now}); err != nil {
		t.Fatal(err)
	}
	limits := ResumeLimits{MaxTurns: 10, MaxBudgetUSD: 1, TimeoutMinutes: 5}
	for attempt, branch := range []string{"job/j", "job/j-2"} {
		token := "tok" + branch
		if err := st.Claim("j", token, now); err != nil {
			t.Fatal(err)
		}
		if err := st.SetSession("j", token, "sid", RunNew); err != nil {
			t.Fatal(err)
		}
		if err := st.Finish("j", token, StatusFailed, 1, 0, 1, "failed", now); err != nil {
			t.Fatal(err)
		}
		if err := st.SetResultBranch("j", branch, "note "+branch); err != nil {
			t.Fatal(err)
		}
		if err := st.Requeue("j", limits, "more", ""); err != nil {
			t.Fatalf("requeue %d: %v", attempt+1, err)
		}
		if j, err := st.Get("j"); err != nil || j.Branch != "" || j.BranchNote != "" {
			t.Fatalf("job row after requeue %d: %+v %v", attempt+1, j, err)
		}
	}
	attempts, err := st.Attempts("j")
	if err != nil || len(attempts) != 2 {
		t.Fatalf("attempts: %+v %v", attempts, err)
	}
	for i, want := range []string{"job/j", "job/j-2"} {
		if attempts[i].Branch != want || attempts[i].BranchNote != "note "+want {
			t.Fatalf("attempt %d: branch=%q note=%q", i+1, attempts[i].Branch, attempts[i].BranchNote)
		}
	}
}
