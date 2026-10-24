package jobs

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/qwwiwi/agent-jobs/internal/guard"
	"github.com/qwwiwi/agent-jobs/internal/store"
)

// The checklist lock is shared by every agent of the user on a repository: two managers with distinct
// state dirs under one home (two agents) are serialized on the same repo, from two of its work trees.
func TestChecklistLockSharedAcrossAgents(t *testing.T) {
	repo := newRepo(t)
	m1 := newManager(t)
	m2, err := Open("other") // the same AGENT_JOBS_HOME as m1
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = m2.Close() })
	if m1.StateDir == m2.StateDir || m1.ChecklistLockDir() != m2.ChecklistLockDir() {
		t.Fatalf("state dirs %s / %s, lock dirs %s / %s", m1.StateDir, m2.StateDir, m1.ChecklistLockDir(), m2.ChecklistLockDir())
	}
	linked := filepath.Join(t.TempDir(), "linked")
	gitT(t, repo, "worktree", "add", "-q", "--detach", linked)
	unlock, err := m1.LockChecklist(repo)
	if err != nil {
		t.Fatal(err)
	}
	got := make(chan func(), 1)
	go func() {
		u, err := m2.LockChecklist(linked)
		if err != nil {
			t.Error(err)
			u = func() {}
		}
		got <- u
	}()
	select {
	case u := <-got:
		u()
		unlock()
		t.Fatal("a second agent took the checklist lock while the first held it")
	case <-time.After(300 * time.Millisecond):
	}
	unlock()
	select {
	case u := <-got:
		u()
	case <-time.After(5 * time.Second):
		t.Fatal("the second agent did not get the lock after the first released it")
	}
}

// The wait is bounded: a holder that never lets go (a job can open the lock file) makes the tool fail
// with a clear error instead of hanging.
func TestChecklistLockWaitBounded(t *testing.T) {
	repo := newRepo(t)
	m := newManager(t)
	unlock, err := m.LockChecklist(repo)
	if err != nil {
		t.Fatal(err)
	}
	defer unlock()
	old := checklistLockWait
	checklistLockWait = 200 * time.Millisecond
	defer func() { checklistLockWait = old }()
	start := time.Now()
	if u, err := m.LockChecklist(repo); err == nil || !strings.Contains(err.Error(), "still held") {
		if u != nil {
			u()
		}
		t.Fatalf("second lock: %v", err)
	}
	if d := time.Since(start); d > 5*time.Second {
		t.Fatalf("waited %s", d)
	}
}

// The lock lies in the agent-jobs home, out of a job's reach: not in the repository (a job's worktree
// shares its git common dir), and the job's guard refuses every read and write of the lock dir, so a job
// cannot take a lock and block the checklist tools.
func TestChecklistLockOutOfJobReach(t *testing.T) {
	repo := newRepo(t)
	m := newManager(t)
	unlock, err := m.LockChecklist(repo)
	if err != nil {
		t.Fatal(err)
	}
	unlock()
	locks, err := filepath.Glob(filepath.Join(m.ChecklistLockDir(), "*.lock"))
	if err != nil || len(locks) != 1 || filepath.Dir(m.ChecklistLockDir()) != filepath.Dir(m.StateDir) {
		t.Fatalf("lock files %v %v", locks, err)
	}
	if _, err := os.Stat(filepath.Join(repo, ".git", "agent-jobs-checklist.lock")); err == nil {
		t.Fatal("a lock file in the repository's git dir")
	}
	j := &store.Job{ID: "261024-100000-lk01", Kind: "fix"}
	pre := m.JobSettings(j)["hooks"].(map[string]any)["PreToolUse"].([]any)[0].(map[string]any)
	hook := pre["hooks"].([]any)[0].(map[string]any)["command"].(string)
	if !strings.Contains(hook, "--no-read "+shellQuote(m.ChecklistLockDir())) {
		t.Fatalf("the job's guard does not keep it from the lock dir: %s", hook)
	}
	cfg := guard.Config{Dir: filepath.Join(m.JobDir(j.ID), "work"), StateDir: m.StateDir, OwnDir: m.JobDir(j.ID),
		NoRead: []string{m.ChecklistLockDir()}}
	if r := guard.Check("exec 9<"+shellQuote(locks[0])+"; flock 9; sleep 600", cfg); r == "" {
		t.Fatal("a job may open and hold the checklist lock")
	}
	if r := guard.CheckFileRead("Read", locks[0], "", cfg); r == "" {
		t.Fatal("a job may read the checklist lock")
	}
}
