package jobs

import (
	"strings"
	"testing"
	"time"

	"github.com/qwwiwi/agent-jobs/internal/store"
)

// Two jobs edit the same line from the same base: the first one, landed while the second runs, makes
// the second's job-done line warn about the conflict with the file and the branch's new commit; the
// first one, whose branch had not moved, merges cleanly.
func TestMergeCheckAtNotify(t *testing.T) {
	m := newManager(t)
	one := 1
	m.Cfg.MaxRunning = &one
	repo := newRepo(t)
	a := enqueue(t, m, "FAKE_EDIT_TRACKED", func(r *StartRequest) { r.RepoPath = repo; r.Kind = "fix" })
	b := enqueue(t, m, "FAKE_EDIT_TRACKED", func(r *StartRequest) { r.RepoPath = repo; r.Kind = "fix" })
	if a.BaseBranch != "refs/heads/main" || b.BaseBranch != "refs/heads/main" {
		t.Fatalf("base branch: %q %q", a.BaseBranch, b.BaseBranch)
	}
	if n := m.SameRepoNote(b); !strings.Contains(n, a.ID+" queued") {
		t.Fatalf("same repo note: %q", n)
	}
	a = runToEnd(t, m, a.ID) // A ends and is notified; B is launched in the same tick
	if a.Status != store.StatusSucceeded || !strings.HasPrefix(a.MergeNote, "merges cleanly onto main at ") {
		t.Fatalf("A: %s %q", a.Status, a.MergeNote)
	}
	if line := m.DoneLineFor(a); !strings.Contains(line, a.MergeNote) {
		t.Fatalf("A done line: %s", line)
	}
	// A's commit lands on main while B runs.
	aHead := gitT(t, m.JobDir(a.ID)+"/work", "rev-parse", "HEAD")
	gitT(t, repo, "merge", "-q", "--ff-only", aHead)
	b = runToEnd(t, m, b.ID)
	if b.Status != store.StatusSucceeded {
		t.Fatalf("B: %s %s", b.Status, b.Error)
	}
	if !strings.HasPrefix(b.MergeNote, "WARNING: conflicts with main at "+aHead[:12]+" in tracked.txt") ||
		!strings.Contains(b.MergeNote, "main gained since base: "+aHead[:7]) ||
		!strings.Contains(b.MergeNote, "edit tracked by job") {
		t.Fatalf("B merge note: %q", b.MergeNote)
	}
	// merge-tree moved no ref.
	if got := gitT(t, repo, "rev-parse", "main"); got != aHead {
		t.Fatalf("main moved to %s", got)
	}
}

// A job started from a tag or a detached HEAD has no branch: its line says the merge was not checked.
func TestMergeCheckWithoutBranch(t *testing.T) {
	m := newManager(t)
	repo := newRepo(t)
	gitT(t, repo, "tag", "v1")
	tagged := enqueue(t, m, "FAKE_EDIT_TRACKED", func(r *StartRequest) { r.RepoPath = repo; r.Base = "v1" })
	gitT(t, repo, "checkout", "-q", "--detach")
	detached := enqueue(t, m, "FAKE_EDIT_TRACKED", func(r *StartRequest) { r.RepoPath = repo })
	if tagged.BaseBranch != "" || detached.BaseBranch != "" {
		t.Fatalf("base branch: %q %q", tagged.BaseBranch, detached.BaseBranch)
	}
	for _, id := range []string{tagged.ID, detached.ID} {
		j := runToEnd(t, m, id)
		if !strings.HasPrefix(j.MergeNote, "merge onto a branch not checked: the job's base is not a branch") {
			t.Fatalf("%s merge note: %q", id, j.MergeNote)
		}
	}
	// A job without commits of its own and a job without a repository get no merge line.
	plain := runToEnd(t, m, enqueue(t, m, "plain", func(r *StartRequest) { r.RepoPath = repo }).ID)
	norepo := runToEnd(t, m, enqueue(t, m, "plain", nil).ID)
	if plain.MergeNote != "" || norepo.MergeNote != "" {
		t.Fatalf("notes: %q %q", plain.MergeNote, norepo.MergeNote)
	}
}

// A named branch ref is the base branch; the merge check gives up after its time cap.
func TestBaseBranchFromRef(t *testing.T) {
	repo := newRepo(t)
	gitT(t, repo, "branch", "feature")
	for ref, want := range map[string]string{"HEAD": "refs/heads/main", "feature": "refs/heads/feature",
		"main": "refs/heads/main", "HEAD~0": ""} {
		if got := baseBranch(repo, ref); got != want {
			t.Errorf("%s: %q, want %q", ref, got, want)
		}
	}
	m := newManager(t)
	j := enqueue(t, m, "FAKE_EDIT_TRACKED", func(r *StartRequest) { r.RepoPath = repo })
	if err := m.Tick(); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "exit_code", 15*time.Second, func() bool { _, ok := readExitCode(m.JobDir(j.ID)); return ok })
	old := MergeCheckTimeout
	MergeCheckTimeout = time.Nanosecond
	defer func() { MergeCheckTimeout = old }()
	if n := m.MergeNote(get(t, m, j.ID)); !strings.Contains(n, "not checked: timed out") {
		t.Fatalf("note: %q", n)
	}
}
