package store

import (
	"database/sql"
	"path/filepath"
	"testing"
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
