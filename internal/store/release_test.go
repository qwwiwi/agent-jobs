package store

import (
	"path/filepath"
	"slices"
	"testing"
	"time"
)

// A store created before release_passes gets the table on Open; a pass is read back only at its exact
// repo, sha, checklist id, version and hash, and dropping the repo's passes for the sha removes it.
func TestReleasePasses(t *testing.T) {
	path := filepath.Join(t.TempDir(), "jobs.db")
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec(`DROP TABLE release_passes`); err != nil {
		t.Fatal(err)
	}
	if _, err := s.ReleasePassed("/r", "a", "demo", 1, "h"); err == nil {
		t.Fatal("read without the table")
	}
	_ = s.Close()
	if s, err = Open(path); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	defer s.Close()
	if err := s.PutReleasePass(ReleasePass{Repo: "/r", SHA: "a"}); err == nil {
		t.Fatal("an incomplete pass stored")
	}
	p := ReleasePass{Repo: "/r", SHA: "a", ChecklistID: "demo", Version: 2, ChecklistSHA256: "h", VerdictJob: "j1", CreatedAt: time.Unix(100, 0)}
	if err := s.PutReleasePass(p); err != nil {
		t.Fatal(err)
	}
	if err := s.PutReleasePass(ReleasePass{Repo: "/r", SHA: "b", ChecklistID: "demo", Version: 2, ChecklistSHA256: "h"}); err != nil {
		t.Fatal(err)
	}
	got, err := s.ReleasePassed("/r", "a", "demo", 2, "h")
	if err != nil || got == nil || got.VerdictJob != "j1" || !got.CreatedAt.Equal(p.CreatedAt) {
		t.Fatalf("pass: %+v %v", got, err)
	}
	for _, k := range [][]any{{"/x", "a", "demo", 2, "h"}, {"/r", "c", "demo", 2, "h"}, {"/r", "a", "other", 2, "h"},
		{"/r", "a", "demo", 3, "h"}, {"/r", "a", "demo", 2, "h2"}} {
		if got, err := s.ReleasePassed(k[0].(string), k[1].(string), k[2].(string), k[3].(int), k[4].(string)); err != nil || got != nil {
			t.Errorf("%v: %+v %v", k, got, err)
		}
	}
	if err := s.DropReleasePasses("/r", "a"); err != nil {
		t.Fatal(err)
	}
	if got, _ := s.ReleasePassed("/r", "a", "demo", 2, "h"); got != nil {
		t.Fatalf("kept after drop: %+v", got)
	}
	if got, _ := s.ReleasePassed("/r", "b", "demo", 2, "h"); got == nil {
		t.Fatal("another sha's pass dropped")
	}
	if err := s.PutApproval(ChecklistApproval{Repo: "/r", ChecklistID: "demo", Version: 2, ChecklistSHA256: "h", BriefSHA256: "b", Quote: "yes",
		ApprovedAt: time.Now()}); err != nil {
		t.Fatal(err)
	}
	if err := s.PutApproval(ChecklistApproval{Repo: "/a", ChecklistID: "x", Version: 1, ChecklistSHA256: "h", BriefSHA256: "b", Quote: "yes",
		ApprovedAt: time.Now()}); err != nil {
		t.Fatal(err)
	}
	if repos, err := s.ApprovalRepos(); err != nil || !slices.Equal(repos, []string{"/a", "/r"}) {
		t.Fatalf("approval repos: %v %v", repos, err)
	}
}

// A pass keeps the seq of the approval release_check checked (#99). A release_passes table created
// before approval_seq is read as 0 by a read-only store (the merge hook's) and gets the column on Open,
// its old rows keeping 0.
func TestReleasePassApprovalSeq(t *testing.T) {
	path := filepath.Join(t.TempDir(), "jobs.db")
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	for _, q := range []string{`DROP TABLE release_passes`, `CREATE TABLE release_passes (repo TEXT NOT NULL, sha TEXT NOT NULL,
		checklist_id TEXT NOT NULL, version INTEGER NOT NULL, checklist_sha256 TEXT NOT NULL, verdict_job TEXT NOT NULL,
		created_at INTEGER NOT NULL, PRIMARY KEY (repo, sha, checklist_id, version, checklist_sha256))`,
		`INSERT INTO release_passes VALUES ('/r', 'old', 'demo', 2, 'h', 'j0', 100)`} {
		if _, err := s.db.Exec(q); err != nil {
			t.Fatal(err)
		}
	}
	_ = s.Close()
	ro, err := OpenReadOnly(path)
	if err != nil {
		t.Fatal(err)
	}
	if p, err := ro.ReleasePassed("/r", "old", "demo", 2, "h"); err != nil || p == nil || p.ApprovalSeq != 0 || p.VerdictJob != "j0" {
		t.Fatalf("old table, read-only: %+v %v", p, err)
	}
	if ps, err := ro.ReleasePassesAt("/r", "demo", 2, "h"); err != nil || len(ps) != 1 || ps[0].ApprovalSeq != 0 {
		t.Fatalf("old table, read-only list: %+v %v", ps, err)
	}
	_ = ro.Close()
	if s, err = Open(path); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	defer s.Close()
	if err := s.PutReleasePass(ReleasePass{Repo: "/r", SHA: "new", ChecklistID: "demo", Version: 2, ChecklistSHA256: "h",
		CreatedAt: time.Unix(200, 0), ApprovalSeq: 7}); err != nil {
		t.Fatal(err)
	}
	if p, err := s.ReleasePassed("/r", "new", "demo", 2, "h"); err != nil || p == nil || p.ApprovalSeq != 7 {
		t.Fatalf("pass: %+v %v", p, err)
	}
	ps, err := s.ReleasePassesAt("/r", "demo", 2, "h")
	if err != nil || len(ps) != 2 || ps[0].SHA != "new" || ps[0].ApprovalSeq != 7 || ps[1].SHA != "old" || ps[1].ApprovalSeq != 0 {
		t.Fatalf("passes: %+v %v", ps, err)
	}
}

// Two release_check passes of the same repo, sha and checklist race to store (#103): the one checked at
// the higher approval seq is kept, whichever order they land in; an equal seq replaces (the later run).
func TestReleasePassKeepsHigherApprovalSeq(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "jobs.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	pass := func(sha, job string, seq int64) ReleasePass {
		return ReleasePass{Repo: "/r", SHA: sha, ChecklistID: "demo", Version: 2, ChecklistSHA256: "h", VerdictJob: job,
			CreatedAt: time.Unix(100+seq, 0), ApprovalSeq: seq}
	}
	for _, c := range []struct {
		name     string
		sha      string
		puts     []ReleasePass
		wantJob  string
		wantSeq  int64
		wantTime int64
	}{
		{"newer then older", "a", []ReleasePass{pass("a", "new", 5), pass("a", "old", 3)}, "new", 5, 105},
		{"older then newer", "b", []ReleasePass{pass("b", "old", 3), pass("b", "new", 5)}, "new", 5, 105},
		{"equal", "c", []ReleasePass{pass("c", "first", 4), pass("c", "second", 4)}, "second", 4, 104},
	} {
		for _, p := range c.puts {
			if err := s.PutReleasePass(p); err != nil {
				t.Fatalf("%s: %v", c.name, err)
			}
		}
		got, err := s.ReleasePassed("/r", c.sha, "demo", 2, "h")
		if err != nil || got == nil || got.VerdictJob != c.wantJob || got.ApprovalSeq != c.wantSeq || got.CreatedAt.Unix() != c.wantTime {
			t.Errorf("%s: %+v %v", c.name, got, err)
		}
	}
}
