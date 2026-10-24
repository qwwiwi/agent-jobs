package jobs

import (
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
