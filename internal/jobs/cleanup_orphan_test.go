package jobs

import (
	"bytes"
	"database/sql"
	"encoding/json"
	"log"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/qwwiwi/agent-jobs/internal/store"
)

// Issue #57: a job started with repo = another job's worktree. That worktree is cleaned up first, so
// the child's repo path is gone; the child's cleanup must still remove its own worktree.

// setRepoPath rewrites a job's repo_path the way a daemon before #57 stored it.
func setRepoPath(t *testing.T, m *Manager, id, repo string) {
	t.Helper()
	db, err := sql.Open("sqlite", filepath.Join(m.StateDir, "jobs.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Exec(`UPDATE jobs SET repo_path = ? WHERE id = ?`, repo, id); err != nil {
		t.Fatal(err)
	}
}

// parentChild runs a parent job on repo and a child job whose repo is the parent's worktree.
func parentChild(t *testing.T, m *Manager, repo string) (parent, child *store.Job) {
	t.Helper()
	parent = runToEnd(t, m, enqueue(t, m, "FAKE_COMMIT", func(r *StartRequest) { r.RepoPath = repo; r.Kind = "fix" }).ID)
	if parent.Status != store.StatusSucceeded {
		t.Fatalf("parent %s %q", parent.Status, parent.Error)
	}
	parentWork := filepath.Join(m.JobDir(parent.ID), "work")
	child = runToEnd(t, m, enqueue(t, m, "FAKE_COMMIT", func(r *StartRequest) { r.RepoPath = parentWork; r.Kind = "fix" }).ID)
	if child.Status != store.StatusSucceeded {
		t.Fatalf("child %s %q", child.Status, child.Error)
	}
	return parent, child
}

func TestJobStartStoresMainRepoForLinkedWorktree(t *testing.T) {
	m := newManager(t)
	notifyTo(t, m)
	repo := newRepo(t)
	parent, child := parentChild(t, m, repo)
	if !samePath(child.RepoPath, repo) {
		t.Fatalf("child repo_path %s, want the main repository %s", child.RepoPath, repo)
	}
	// The base is still resolved in the worktree that was given: the parent's HEAD (its commit),
	// not the main worktree's HEAD.
	parentHead := gitT(t, filepath.Join(m.JobDir(parent.ID), "work"), "rev-parse", "HEAD")
	if child.BaseCommit != parentHead || child.BaseCommit == parent.BaseCommit {
		t.Fatalf("child base %s, want the parent worktree's HEAD %s", child.BaseCommit, parentHead)
	}
	// A linked worktree on a branch: the base branch is the worktree's branch.
	wt := filepath.Join(t.TempDir(), "feature-wt")
	gitT(t, repo, "worktree", "add", "-q", "-b", "feature", wt, "HEAD")
	j := enqueue(t, m, "x", func(r *StartRequest) { r.RepoPath = filepath.Join(wt, ".") })
	if !samePath(j.RepoPath, repo) || j.BaseBranch != "refs/heads/feature" {
		t.Fatalf("repo %s branch %q, want %s refs/heads/feature", j.RepoPath, j.BaseBranch, repo)
	}
	if top, err := RepoTopLevel(wt); err != nil || !samePath(top, repo) {
		t.Fatalf("RepoTopLevel(%s) = %s %v, want %s", wt, top, err, repo)
	}
	// A plain repository keeps its own path.
	if top, err := RepoTopLevel(repo); err != nil || !samePath(top, repo) {
		t.Fatalf("RepoTopLevel(%s) = %s %v", repo, top, err)
	}
	// A bare repository has no main worktree: its linked worktree is kept.
	bare := filepath.Join(t.TempDir(), "bare.git")
	gitT(t, repo, "clone", "-q", "--bare", repo, bare)
	bwt := filepath.Join(t.TempDir(), "bare-wt")
	gitT(t, bare, "worktree", "add", "-q", "--detach", bwt, "HEAD")
	if got := mainWorktree(bwt); !samePath(got, bwt) {
		t.Fatalf("mainWorktree of a bare repository's worktree = %s, want %s", got, bwt)
	}
}

// The exact chain of #57: parent worktree -> child with repo = parent worktree -> parent cleaned ->
// child cleanup succeeds.
func TestCleanupChildAfterParentCleaned(t *testing.T) {
	m := newManager(t)
	notifyTo(t, m)
	repo := newRepo(t)
	parent, child := parentChild(t, m, repo)
	if err := m.Cleanup(parent); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(m.JobDir(parent.ID), "work")); !os.IsNotExist(err) {
		t.Fatal("parent worktree not removed")
	}
	childWork := filepath.Join(m.JobDir(child.ID), "work")
	childHead := gitT(t, childWork, "rev-parse", "HEAD")
	var logBuf bytes.Buffer
	m.Log = log.New(&logBuf, "", 0)
	real := time.Now
	m.Now = func() time.Time { return real().Add(31 * time.Minute) }
	if err := m.Tick(); err != nil {
		t.Fatal(err)
	}
	if get(t, m, child.ID).CleanedAt == nil {
		t.Fatalf("child not cleaned: %s", logBuf.String())
	}
	if _, err := os.Stat(childWork); !os.IsNotExist(err) {
		t.Fatal("child worktree still exists")
	}
	if ref := gitT(t, repo, "rev-parse", "refs/agent-jobs/jobs/"+child.ID); ref != childHead {
		t.Fatalf("child ref %s, want %s", ref, childHead)
	}
}

// A row stored before #57 still names the parent's (now removed) worktree: cleanup resolves the
// main repository from the job's own worktree admin dir and verifies ownership there.
func TestCleanupFallbackWhenRepoPathGone(t *testing.T) {
	m := newManager(t)
	notifyTo(t, m)
	repo := newRepo(t)
	parent, child := parentChild(t, m, repo)
	parentWork := filepath.Join(m.JobDir(parent.ID), "work")
	setRepoPath(t, m, child.ID, parentWork)
	if err := m.Cleanup(parent); err != nil {
		t.Fatal(err)
	}
	child = get(t, m, child.ID)
	if child.RepoPath != parentWork {
		t.Fatalf("precondition: repo_path %s", child.RepoPath)
	}
	childWork := filepath.Join(m.JobDir(child.ID), "work")
	childHead := gitT(t, childWork, "rev-parse", "HEAD")
	if err := m.Cleanup(child); err != nil {
		t.Fatalf("cleanup with a missing repo path: %v", err)
	}
	if _, err := os.Stat(childWork); !os.IsNotExist(err) {
		t.Fatal("child worktree still exists")
	}
	if ref := gitT(t, repo, "rev-parse", "refs/agent-jobs/jobs/"+child.ID); ref != childHead {
		t.Fatalf("child ref %s, want %s", ref, childHead)
	}
	if out := gitT(t, repo, "worktree", "list", "--porcelain"); strings.Contains(out, childWork) {
		t.Fatalf("child worktree still registered: %s", out)
	}
}

// The fallback does not weaken the ownership check: a missing repo path with a foreign worktree at
// the job's work path, or a worktree.json whose gitdir is not <common>/worktrees/<name>, is refused.
func TestCleanupFallbackRefusesForeign(t *testing.T) {
	t.Run("foreign worktree", func(t *testing.T) {
		m := newManager(t)
		repo := newRepo(t)
		j := runToEnd(t, m, enqueue(t, m, "x", func(r *StartRequest) { r.RepoPath = repo }).ID)
		work := filepath.Join(m.JobDir(j.ID), "work")
		setRepoPath(t, m, j.ID, filepath.Join(t.TempDir(), "gone"))
		gitT(t, repo, "worktree", "remove", "--force", work)
		gitT(t, repo, "worktree", "add", "-q", "--detach", work, "HEAD")
		if err := os.WriteFile(filepath.Join(work, "precious.txt"), []byte("keep"), 0o644); err != nil {
			t.Fatal(err)
		}
		err := m.Cleanup(get(t, m, j.ID))
		if err == nil || !strings.Contains(err.Error(), "refusing to touch") {
			t.Fatalf("foreign worktree not refused: %v", err)
		}
		if _, err := os.Stat(filepath.Join(work, "precious.txt")); err != nil {
			t.Fatal("foreign worktree content removed")
		}
	})
	t.Run("gitdir outside worktrees", func(t *testing.T) {
		m := newManager(t)
		repo := newRepo(t)
		j := runToEnd(t, m, enqueue(t, m, "x", func(r *StartRequest) { r.RepoPath = repo }).ID)
		dir := m.JobDir(j.ID)
		work := filepath.Join(dir, "work")
		setRepoPath(t, m, j.ID, filepath.Join(t.TempDir(), "gone"))
		b, _ := json.Marshal(WorktreeInfo{Path: work, GitDir: filepath.Join(t.TempDir(), "x", "y"), Head: j.BaseCommit})
		if err := os.WriteFile(filepath.Join(dir, "worktree.json"), b, 0o600); err != nil {
			t.Fatal(err)
		}
		err := m.Cleanup(get(t, m, j.ID))
		if err == nil || !strings.Contains(err.Error(), "refusing to touch") {
			t.Fatalf("bad gitdir not refused: %v", err)
		}
		if _, err := os.Stat(filepath.Join(work, "tracked.txt")); err != nil {
			t.Fatal("work removed")
		}
	})
}

// After cleanupBackoffAfter consecutive failures cleanup logs the back-off once and retries less
// often; a success resets it.
func TestCleanupFailureBackoff(t *testing.T) {
	m := newManager(t)
	repo := newRepo(t)
	j := runToEnd(t, m, enqueue(t, m, "x", func(r *StartRequest) { r.RepoPath = repo }).ID)
	work := filepath.Join(m.JobDir(j.ID), "work")
	// A foreign worktree at the work path: every cleanup is refused.
	gitT(t, repo, "worktree", "remove", "--force", work)
	gitT(t, repo, "worktree", "add", "-q", "--detach", work, "HEAD")
	var logBuf bytes.Buffer
	m.Log = log.New(&logBuf, "", 0)
	now := time.Now().Add(31 * time.Minute)
	m.Now = func() time.Time { return now }
	failures := func() int { return strings.Count(logBuf.String(), "cleanup failed") }
	for i := 0; i < 30; i++ {
		if err := m.Tick(); err != nil {
			t.Fatal(err)
		}
		now = now.Add(time.Minute)
	}
	if n := failures(); n > cleanupBackoffAfter+1 {
		t.Fatalf("%d failure lines in 30 ticks, want at most %d:\n%s", n, cleanupBackoffAfter+1, logBuf.String())
	}
	if n := strings.Count(logBuf.String(), "backing off"); n != 1 {
		t.Fatalf("back-off logged %d times, want once:\n%s", n, logBuf.String())
	}
	// Retries keep happening (with back-off): a fixed worktree is cleaned within the cap.
	gitT(t, repo, "worktree", "remove", "--force", work)
	if err := os.MkdirAll(filepath.Dir(work), 0o700); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < int(cleanupBackoffMax/time.Minute)+1 && get(t, m, j.ID).CleanedAt == nil; i++ {
		if err := m.Tick(); err != nil {
			t.Fatal(err)
		}
		now = now.Add(time.Minute)
	}
	if get(t, m, j.ID).CleanedAt == nil {
		t.Fatalf("not cleaned after the back-off cap:\n%s", logBuf.String())
	}
	if _, ok := m.cleanupFails[j.ID]; ok {
		t.Fatal("failure state kept after success")
	}
}
