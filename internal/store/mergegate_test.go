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

// The shipped record (#99): a merge_gate_repos table from before it gets its columns on Open (a
// read-only store opened before that reads no shipped record); MarkGateShipped keeps the names and the
// retirement; ReleasePassesAt lists the passes at one checklist id, version and hash, newest first.
func TestGateRepoShipped(t *testing.T) {
	path := filepath.Join(t.TempDir(), "jobs.db")
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	for _, q := range []string{`DROP TABLE merge_gate_repos`, `CREATE TABLE merge_gate_repos (repo TEXT PRIMARY KEY,
		names TEXT NOT NULL DEFAULT '', retired_seq INTEGER NOT NULL DEFAULT 0, updated_at INTEGER NOT NULL)`,
		`INSERT INTO merge_gate_repos (repo, names, retired_seq, updated_at) VALUES ('/r', 'a/y', 3, 1)`} {
		if _, err := s.db.Exec(q); err != nil {
			t.Fatal(err)
		}
	}
	_ = s.Close()
	ro, err := OpenReadOnly(path)
	if err != nil {
		t.Fatal(err)
	}
	if g, err := ro.GateRepoOf("/r"); err != nil || g.RetiredSeq != 3 || g.ShippedSeq != 0 || !slices.Equal(g.Names, []string{"a/y"}) {
		t.Fatalf("old table, read-only: %+v %v", g, err)
	}
	_ = ro.Close()
	if s, err = Open(path); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	defer s.Close()
	if err := s.MarkGateShipped("/r", 0, "abc", time.Now()); err == nil {
		t.Fatal("shipped without a seq")
	}
	at := time.Unix(1_800_000_000, 0).UTC()
	if err := s.MarkGateShipped("/r", 5, "abc", at); err != nil {
		t.Fatal(err)
	}
	if g, err := s.GateRepoOf("/r"); err != nil || g.RetiredSeq != 3 || g.ShippedSeq != 5 || g.ShippedSHA != "abc" ||
		!g.ShippedAt.Equal(at) || !slices.Equal(g.Names, []string{"a/y"}) {
		t.Fatalf("shipped: %+v %v", g, err)
	}
	if err := s.MarkGateShipped("/new", 7, "def", at); err != nil {
		t.Fatal(err)
	}
	if g, err := s.GateRepoOf("/new"); err != nil || g.ShippedSeq != 7 || g.Names != nil || g.RetiredSeq != 0 {
		t.Fatalf("shipped, new record: %+v %v", g, err)
	}

	for i, p := range []ReleasePass{
		{Repo: "/r", SHA: "s1", ChecklistID: "demo", Version: 2, ChecklistSHA256: "h", CreatedAt: at},
		{Repo: "/r", SHA: "s2", ChecklistID: "demo", Version: 2, ChecklistSHA256: "h", CreatedAt: at.Add(time.Hour)},
		{Repo: "/r", SHA: "s3", ChecklistID: "demo", Version: 2, ChecklistSHA256: "other", CreatedAt: at},
		{Repo: "/r", SHA: "s4", ChecklistID: "demo", Version: 1, ChecklistSHA256: "h", CreatedAt: at},
		{Repo: "/x", SHA: "s5", ChecklistID: "demo", Version: 2, ChecklistSHA256: "h", CreatedAt: at},
	} {
		if err := s.PutReleasePass(p); err != nil {
			t.Fatalf("%d: %v", i, err)
		}
	}
	ps, err := s.ReleasePassesAt("/r", "demo", 2, "h")
	if err != nil || len(ps) != 2 || ps[0].SHA != "s2" || ps[1].SHA != "s1" {
		t.Fatalf("passes: %+v %v", ps, err)
	}
}
