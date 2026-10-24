package jobs

import (
	"strings"
	"testing"
	"time"
)

// The checklist lock is shared by every agent on a repository: two managers with distinct state dirs
// (two agents) are serialized on the same repo.
func TestChecklistLockSharedAcrossAgents(t *testing.T) {
	repo := newRepo(t)
	m1 := newManager(t)
	m2 := newManager(t)
	if m1.StateDir == m2.StateDir {
		t.Fatal("the managers share a state dir")
	}
	unlock, err := m1.LockChecklist(repo)
	if err != nil {
		t.Fatal(err)
	}
	got := make(chan func(), 1)
	go func() {
		u, err := m2.LockChecklist(repo)
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
