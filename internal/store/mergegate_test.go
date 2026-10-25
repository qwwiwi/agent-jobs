package store

import (
	"errors"
	"path/filepath"
	"slices"
	"testing"
	"time"
)

// A store created before merge_gate_repos gets the table on Open; names are added (sorted, no
// duplicates, none with a comma), and retiring a repository records its latest approval's seq and forgets
// its names; a repository without an approval cannot be retired.
func TestGateRepos(t *testing.T) {
	path := filepath.Join(t.TempDir(), "jobs.db")
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec(`DROP TABLE merge_gate_repos`); err != nil {
		t.Fatal(err)
	}
	if g, err := s.GateRepoOf("/r"); err != nil || g.Repo != "" {
		t.Fatalf("no table: %+v %v", g, err)
	}
	_ = s.Close()
	if s, err = Open(path); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	defer s.Close()
	if g, err := s.GateRepoOf("/r"); err != nil || g.Repo != "" || g.Names != nil {
		t.Fatalf("no record: %+v %v", g, err)
	}
	if err := s.AddGateRepoNames("/r", []string{"b/x", "a/y"}); err != nil {
		t.Fatal(err)
	}
	if err := s.AddGateRepoNames("/r", []string{"a/y", "c/z"}); err != nil {
		t.Fatal(err)
	}
	if err := s.AddGateRepoNames("/r", []string{"a,b/c"}); err == nil {
		t.Fatal("a name with a comma stored")
	}
	if g, err := s.GateRepoOf("/r"); err != nil || g.Repo != "/r" || !slices.Equal(g.Names, []string{"a/y", "b/x", "c/z"}) || g.RetiredSeq != 0 {
		t.Fatalf("names: %+v %v", g, err)
	}
	if _, err := s.RetireGateRepo("/r"); !errors.Is(err, ErrNotGated) {
		t.Fatalf("retired without an approval: %v", err)
	}
	for range 2 {
		if err := s.PutApproval(ChecklistApproval{Repo: "/r", ChecklistID: "demo", Version: 1, ChecklistSHA256: "h", BriefSHA256: "b",
			Quote: "yes", ApprovedAt: time.Now()}); err != nil {
			t.Fatal(err)
		}
	}
	last, err := s.LatestApproval("/r")
	if err != nil {
		t.Fatal(err)
	}
	seq, err := s.RetireGateRepo("/r")
	if err != nil || seq != last.Seq {
		t.Fatalf("retire: %d %v (latest %d)", seq, err, last.Seq)
	}
	if g, err := s.GateRepoOf("/r"); err != nil || g.Names != nil || g.RetiredSeq != last.Seq {
		t.Fatalf("after retire: %+v %v", g, err)
	}
}
