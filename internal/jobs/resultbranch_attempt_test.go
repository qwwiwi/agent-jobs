package jobs

import (
	"strings"
	"testing"
	"time"

	"github.com/qwwiwi/agent-jobs/internal/store"
)

// Reserving the namespace at start keeps the default publication of a later job working (#205).
func TestResultBranchReservedNamespaceThenDefaultPublishes(t *testing.T) {
	m := newManager(t)
	repo := newRepo(t)
	for _, name := range []string{"job", "job/mine"} {
		_, err := m.Enqueue(StartRequest{Title: "test", Kind: "fix", Brief: "test", Model: "sonnet", RepoPath: repo, Branch: name})
		if err == nil || !strings.Contains(err.Error(), "reserved") {
			t.Fatalf("%q: expected a reserved-name refusal, got %v", name, err)
		}
	}
	j := runToEnd(t, m, enqueue(t, m, "FAKE_EDIT_TRACKED", func(r *StartRequest) { r.RepoPath = repo }).ID)
	if j.Branch != "job/"+j.ID {
		t.Fatalf("default publication: branch=%q note=%q", j.Branch, j.BranchNote)
	}
}

// A failed attempt publishes H1 on job/<id>; the resumed attempt commits H2 and publishes it on
// job/<id>-2. Both are reachable, the first ref is untouched, the job row shows the newest, the
// archived attempt records its own branch, and re-publishing after a crash is idempotent (#205).
func TestResultBranchResumedAttemptPublishesOwnBranch(t *testing.T) {
	m := newManager(t)
	sessionsHome(t, true)
	repo := newRepo(t)
	j := enqueue(t, m, "FAKE_EDIT_TRACKED", func(r *StartRequest) { r.RepoPath = repo })
	if err := m.Tick(); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "exit_code", 15*time.Second, func() bool { _, ok := readExitCode(m.JobDir(j.ID)); return ok })
	j = get(t, m, j.ID)
	if err := m.Store.Finish(j.ID, j.LaunchToken, store.StatusFailed, 1, 0, 1, "attempt 1 failed", m.Now()); err != nil {
		t.Fatal(err)
	}
	if err := m.notifyPending(); err != nil {
		t.Fatal(err)
	}
	j = get(t, m, j.ID)
	h1 := gitT(t, m.JobDir(j.ID)+"/work", "rev-parse", "HEAD")
	if j.Branch != "job/"+j.ID || gitT(t, repo, "rev-parse", "refs/heads/job/"+j.ID) != h1 {
		t.Fatalf("attempt 1: branch=%q note=%q", j.Branch, j.BranchNote)
	}
	if _, err := m.Resume(j.ID, "FAKE_ONLY_COMMIT", Limits{}); err != nil {
		t.Fatal(err)
	}
	j = runToEnd(t, m, j.ID)
	h2 := gitT(t, m.JobDir(j.ID)+"/work", "rev-parse", "HEAD")
	if j.Status != store.StatusSucceeded || j.Attempt != 2 || h2 == h1 {
		t.Fatalf("attempt 2 setup: %s attempt=%d error=%q", j.Status, j.Attempt, j.Error)
	}
	second := "job/" + j.ID + "-2"
	if j.Branch != second || j.BranchNote != "" {
		t.Fatalf("attempt 2: branch=%q note=%q", j.Branch, j.BranchNote)
	}
	if got := gitT(t, repo, "rev-parse", "refs/heads/job/"+j.ID); got != h1 {
		t.Fatalf("attempt 1 ref moved to %s, want %s", got, h1)
	}
	if got := gitT(t, repo, "rev-parse", "refs/heads/"+second); got != h2 {
		t.Fatalf("attempt 2 ref at %s, want %s", got, h2)
	}
	earlier, err := m.Store.Attempts(j.ID)
	if err != nil || len(earlier) != 1 || earlier[0].Branch != "job/"+j.ID {
		t.Fatalf("archived attempt branch: %+v %v", earlier, err)
	}
	// Crash between the ref write and the row update: the retry lands on the same name, writes nothing.
	if err := m.Store.SetResultBranch(j.ID, "", ""); err != nil {
		t.Fatal(err)
	}
	if err := m.publishResultBranch(get(t, m, j.ID)); err != nil {
		t.Fatal(err)
	}
	if j = get(t, m, j.ID); j.Branch != second || j.BranchNote != "" {
		t.Fatalf("crash retry: branch=%q note=%q", j.Branch, j.BranchNote)
	}
	if got := gitT(t, repo, "rev-parse", "refs/heads/"+second); got != h2 {
		t.Fatalf("attempt 2 ref at %s after retry", got)
	}
}
