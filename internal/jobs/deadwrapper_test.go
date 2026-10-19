package jobs

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/qwwiwi/agent-jobs/internal/store"
)

// startAndKillWrapper launches job id, waits for the fake claude and kills only its wrapper; it
// returns the job's process group (the wrapper's pid, recorded at the spawn).
func startAndKillWrapper(t *testing.T, m *Manager, id string) int {
	t.Helper()
	if err := m.Tick(); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "fake claude", 5*time.Second, func() bool {
		_, err := os.Stat(filepath.Join(m.JobDir(id), "argv.txt"))
		return err == nil
	})
	j := get(t, m, id)
	if !j.PGIDSpawned || j.PGID != j.PID {
		t.Fatalf("pgid %d (spawned %v) not recorded at the spawn of pid %d", j.PGID, j.PGIDSpawned, j.PID)
	}
	g := j.PGID
	t.Cleanup(func() { _ = syscall.Kill(-g, syscall.SIGKILL) })
	if err := syscall.Kill(j.PID, syscall.SIGKILL); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "wrapper gone", 5*time.Second, func() bool { return !alive(j.PID) })
	if !groupAlive(g) {
		t.Fatal("the job's group died with its wrapper; nothing to test")
	}
	return g
}

// #42: a wrapper that dies alone (OOM, a stray kill) leaves claude running in the job's group; the
// next tick ends that group before the row turns terminal and says so.
func TestDeadWrapperKillsSpawnedGroup(t *testing.T) {
	m := newManager(t)
	j := enqueue(t, m, "FAKE_SLEEP=60", nil)
	g := startAndKillWrapper(t, m, j.ID)
	if err := m.Tick(); err != nil {
		t.Fatal(err)
	}
	j = get(t, m, j.ID)
	if j.Status != store.StatusFailed {
		t.Fatalf("status %s, want failed", j.Status)
	}
	if want := fmt.Sprintf("leftover process group %d killed", g); !strings.Contains(j.Error, want) {
		t.Fatalf("error %q lacks %q", j.Error, want)
	}
	if groupAlive(g) {
		t.Fatal("the job's process group outlived the tick")
	}
	if j.PGIDSpawned {
		t.Fatal("the mark of the killed group was kept")
	}
}

// #42: while the group of a job whose wrapper died is still alive (here: every signal is swallowed),
// the job is not resumed and its worktree is not cleaned up; once the group is gone, both are allowed.
func TestDeadWrapperLiveGroupBlocksResumeAndCleanup(t *testing.T) {
	m := newManager(t)
	realKill, realSend := killFn, pidfdSend
	killFn = func(int, syscall.Signal) error { return nil }
	pidfdSend = func(int, syscall.Signal) error { return nil }
	t.Cleanup(func() { killFn, pidfdSend = realKill, realSend })
	m.KillGrace = 200 * time.Millisecond

	repo := newRepo(t)
	j := enqueue(t, m, "FAKE_SLEEP=60", func(r *StartRequest) { r.RepoPath = repo })
	g := startAndKillWrapper(t, m, j.ID)
	if err := m.Tick(); err != nil {
		t.Fatal(err)
	}
	j = get(t, m, j.ID)
	if j.Status != store.StatusFailed || !strings.Contains(j.Error, "survived SIGKILL") {
		t.Fatalf("status %s error %q", j.Status, j.Error)
	}
	if !groupAlive(g) {
		t.Fatal("group gone although every signal was swallowed")
	}
	if _, err := m.Resume(j.ID, "more", Limits{}); err == nil || !strings.Contains(err.Error(), "still alive") {
		t.Fatalf("resume with a live group: %v", err)
	}
	if err := m.Cleanup(j); err == nil || !strings.Contains(err.Error(), "still alive") {
		t.Fatalf("cleanup with a live group: %v", err)
	}
	if _, err := os.Stat(filepath.Join(m.JobDir(j.ID), "work", ".git")); err != nil {
		t.Fatalf("worktree removed: %v", err)
	}
	pj := m.pruneJob(j, true)
	if !strings.Contains(pj.Skipped, "still alive") {
		t.Fatalf("prune with a live group: %+v", pj)
	}

	if err := syscall.Kill(-g, syscall.SIGKILL); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "group gone", 5*time.Second, func() bool { return !groupAlive(g) })
	if err := m.Cleanup(j); err != nil {
		t.Fatalf("cleanup once the group is gone: %v", err)
	}
	if get(t, m, j.ID).PGIDSpawned {
		t.Fatal("the mark of a group that is gone was kept: a later group reusing the id would block the job")
	}
	if _, err := m.Resume(j.ID, "more", Limits{}); err != nil && strings.Contains(err.Error(), "still alive") {
		t.Fatalf("resume refused once the group is gone: %v", err)
	}
}

// #42: a group adopted from wrapper.json (which the job can write) is never signalled once its
// wrapper is gone, neither when the dead wrapper is collected nor at a timeout.
func TestDeadWrapperNeverSignalsAdoptedGroup(t *testing.T) {
	var kills []int
	realKill := killFn
	killFn = func(pid int, _ syscall.Signal) error { kills = append(kills, pid); return nil }
	t.Cleanup(func() { killFn = realKill })

	other := exec.Command("sleep", "30")
	other.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := other.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = other.Process.Kill(); _ = other.Wait() })
	g := other.Process.Pid
	dead := 1 << 22

	m := newManager(t)
	for _, timeout := range []bool{false, true} {
		j := enqueue(t, m, "x", nil)
		tok := fmt.Sprintf("tok-%v", timeout)
		if err := m.Store.Claim(j.ID, tok, m.Now()); err != nil {
			t.Fatal(err)
		}
		if err := m.Store.SetPID(j.ID, tok, dead, g); err != nil {
			t.Fatal(err)
		}
		if timeout {
			real := time.Now
			m.Now = func() time.Time { return real().Add(time.Duration(DefaultTimeoutMinutes+1) * time.Minute) }
		}
		if err := m.Tick(); err != nil {
			t.Fatal(err)
		}
		got := get(t, m, j.ID)
		if got.PGIDSpawned || got.Status == store.StatusRunning || strings.Contains(got.Error, "leftover") {
			t.Fatalf("timeout=%v: spawned=%v status %s error %q", timeout, got.PGIDSpawned, got.Status, got.Error)
		}
		if slices.Contains(kills, -g) {
			t.Fatalf("timeout=%v: adopted group %d signalled: %v", timeout, g, kills)
		}
	}
	if !groupAlive(g) {
		t.Fatal("adopted group gone")
	}
}
