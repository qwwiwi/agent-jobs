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
