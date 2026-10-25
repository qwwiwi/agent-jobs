package store

import (
	"errors"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

func runningJudge(t *testing.T, s *Store, id string, attempt int) string {
	t.Helper()
	tok := "tok-" + id
	if err := s.Insert(&Job{ID: id, Agent: "a", Title: "t", Kind: "judge", Brief: "b", Status: StatusRunning, Model: "opus",
		MaxTurns: 1, MaxBudgetUSD: 1, TimeoutMinutes: 1, CreatedAt: time.Now(), LaunchToken: tok, Attempt: attempt}); err != nil {
		t.Fatal(err)
	}
	return tok
}

// A manifest-check record is written only under the running launch token of its job and bound to the
// attempt the token runs (never the caller's); it is read by job attempt and by repository and sha.
func TestManifestCheckTokenBound(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "jobs.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	sha := strings.Repeat("a", 40)
	tok := runningJudge(t, s, "j1", 2)
	rec := ManifestCheck{JobID: "j1", Attempt: 7, Repo: "/r", SHA: sha, ChecklistID: "demo", Version: 2, ChecklistSHA256: "h",
		Script: "scripts/manifest-check.sh", ExitCode: 1, DurationSec: 3, Output: "out", OutputCut: 5, CreatedAt: time.Now()}
	if _, err := s.PutManifestCheck("forged", rec); !errors.Is(err, ErrNotRunningLaunch) {
		t.Fatalf("forged token: %v", err)
	}
	if _, err := s.PutManifestCheck(tok, ManifestCheck{JobID: "j1", Repo: "/r", SHA: sha}); err == nil {
		t.Fatal("an incomplete record was stored")
	}
	if _, err := s.PutManifestCheck(tok, rec); err != nil {
		t.Fatal(err)
	}
	got, err := s.ManifestCheckByJob("j1", 2)
	if err != nil || got == nil || got.Attempt != 2 || got.ExitCode != 1 || got.Output != "out" || got.OutputCut != 5 || got.SHA != sha {
		t.Fatalf("by job: %+v %v", got, err)
	}
	if other, _ := s.ManifestCheckByJob("j1", 7); other != nil {
		t.Fatalf("the caller's attempt was taken: %+v", other)
	}
	if l, err := s.LatestManifestCheck("/r", sha); err != nil || l == nil || l.Seq != got.Seq {
		t.Fatalf("latest: %+v %v", l, err)
	}
	if l, _ := s.LatestManifestCheck("/r", strings.Repeat("b", 40)); l != nil {
		t.Fatalf("another sha: %+v", l)
	}
	// A verdict carries the record and reads it back.
	if _, err := s.PutVerdict(tok, JudgeVerdict{JobID: "j1", Repo: "/r", SHA: sha, ChecklistID: "demo", Version: 2, ChecklistSHA256: "h",
		ManifestCheck: got, Counts: map[string]int{}, Verdict: map[string]any{"schema": "x"}, CreatedAt: time.Now()}); err != nil {
		t.Fatal(err)
	}
	v, err := s.VerdictByJob("j1", 2)
	if err != nil || v == nil || v.ManifestCheck == nil || v.ManifestCheck.Seq != got.Seq || v.ManifestCheck.ExitCode != 1 {
		t.Fatalf("verdict's record: %+v %v", v, err)
	}
}

// VerdictsAt lists every verdict for one sha at one checklist version, oldest first; CoverJobs carries
// each feature job's covers.
func TestVerdictsAtAndCoverJobCovers(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "jobs.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	sha := strings.Repeat("a", 40)
	for i, id := range []string{"j1", "j2", "j3"} {
		tok := runningJudge(t, s, id, 1)
		v := JudgeVerdict{JobID: id, Repo: "/r", SHA: sha, ChecklistID: "demo", Version: 2, ChecklistSHA256: "h", Counts: map[string]int{},
			Verdict: map[string]any{"n": i}, CreatedAt: time.Now()}
		if id == "j3" {
			v.Version = 3 // another version
		}
		if _, err := s.PutVerdict(tok, v); err != nil {
			t.Fatal(err)
		}
	}
	vs, err := s.VerdictsAt("/r", "demo", 2, sha)
	if err != nil || len(vs) != 2 || vs[0].JobID != "j1" || vs[1].JobID != "j2" {
		t.Fatalf("verdicts: %+v %v", vs, err)
	}
	if vs, _ := s.VerdictsAt("/r", "demo", 2, strings.Repeat("b", 40)); len(vs) != 0 {
		t.Fatalf("another sha: %+v", vs)
	}
	if err := s.Insert(&Job{ID: "c1", Agent: "a", Title: "t", Kind: "fix", Brief: "b", Status: StatusSucceeded, Model: "opus",
		RepoPath: "/r", CreatedAt: time.Now(), Covers: []string{"R01", "R03i"}, CoverContract: &CoverContract{ChecklistID: "demo", Version: 2}}); err != nil {
		t.Fatal(err)
	}
	cj, err := s.CoverJobs("/r", "demo")
	if err != nil || len(cj) != 1 || !slices.Equal(cj[0].Covers, []string{"R01", "R03i"}) {
		t.Fatalf("cover jobs: %+v %v", cj, err)
	}
}
