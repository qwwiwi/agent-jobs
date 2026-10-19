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
	// The killed attempt has no result event: its cost counts at its whole budget, so a new one is given.
	if _, err := m.Resume(j.ID, "more", Limits{MaxBudgetUSD: j.MaxBudgetUSD + 1}); err != nil {
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

// strangerGroup starts an unrelated process group without a leader (its shell exited, a sleep stays),
// as a recycled pgid looks: the id of a job's group that emptied, now an unrelated group's.
func strangerGroup(t *testing.T) (pgid, member int) {
	t.Helper()
	sh := exec.Command("sh", "-c", "sleep 30 >/dev/null 2>&1 & echo $!")
	sh.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	sh.Env = slices.DeleteFunc(os.Environ(), func(kv string) bool { return strings.HasPrefix(kv, JobEnvVar+"=") })
	out, err := sh.Output()
	if err != nil {
		t.Fatal(err)
	}
	g := sh.Process.Pid
	if _, err := fmt.Sscan(strings.TrimSpace(string(out)), &member); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = syscall.Kill(member, syscall.SIGKILL) })
	waitFor(t, "leaderless stranger group", 5*time.Second, func() bool { return !alive(g) && groupAlive(g) })
	return g, member
}

// #42 review: a pgid recorded at the spawn proves past ownership only. When the job's group emptied
// and the id now belongs to an unrelated group (no marker of the job), neither the dead-wrapper
// collect nor a timeout signals it; the row says so.
func TestDeadWrapperNeverSignalsRecycledGroup(t *testing.T) {
	m := newManager(t)
	m.KillGrace = 200 * time.Millisecond
	for _, timeout := range []bool{false, true} {
		g, member := strangerGroup(t)
		j := enqueue(t, m, "x", nil)
		tok := fmt.Sprintf("tok-%v", timeout)
		if err := m.Store.Claim(j.ID, tok, m.Now()); err != nil {
			t.Fatal(err)
		}
		if err := m.Store.SetSpawnedPID(j.ID, tok, g); err != nil {
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
		if got.Status == store.StatusRunning {
			t.Fatalf("timeout=%v: still running", timeout)
		}
		if !timeout && !strings.Contains(got.Error, fmt.Sprintf("leftover process group %d not signalled", g)) {
			t.Fatalf("error %q lacks the refusal", got.Error)
		}
		time.Sleep(300 * time.Millisecond)
		if !alive(member) {
			t.Fatalf("timeout=%v: member %d of the unrelated group %d was killed", timeout, member, g)
		}
	}
}

// writeFakeProc writes /proc/<pid>/{stat,environ,cgroup} under the fake procRoot root.
func writeFakeProc(t *testing.T, root string, pid, pgid int, marker, cgroup string) {
	t.Helper()
	dir := filepath.Join(root, fmt.Sprint(pid))
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	// pid (comm) state ppid pgrp session tty tpgid flags … starttime (field 22)
	stat := fmt.Sprintf("%d (claude) S 1 %d %d 0 -1 0 0 0 0 0 0 0 0 0 20 0 1 0 12345 0 0\n", pid, pgid, pgid)
	env := "PATH=/bin\x00"
	if marker != "" {
		env += JobEnvVar + "=" + marker + "\x00"
	}
	for name, body := range map[string]string{"stat": stat, "environ": env, "cgroup": cgroup} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

// #42 review: a member of a scoped job's group must carry the job's marker and live in its scope.
func TestJobGroupMemberNeedsMarkerAndScope(t *testing.T) {
	root := t.TempDir()
	old := procRoot
	procRoot = root
	t.Cleanup(func() { procRoot = old })
	j := &store.Job{ID: "261019-000000-abcd", ScopeUnit: ScopeUnit("thrall", "261019-000000-abcd")}
	in := "0::/user.slice/user-1000.slice/user@1000.service/app.slice/" + j.ScopeUnit + "\n"
	other := "0::/user.slice/user-1000.slice/user@1000.service/app.slice/" + ScopeUnit("thrall", "x") + "\n"
	writeFakeProc(t, root, 100, 50, j.ID, in)
	writeFakeProc(t, root, 101, 50, j.ID, other)
	writeFakeProc(t, root, 102, 50, "", in)
	writeFakeProc(t, root, 103, 51, j.ID, in)
	isMember := jobGroupMember(j, 50)
	for pid, want := range map[int]bool{100: true, 101: false, 102: false, 103: false, 104: false} {
		if got := isMember(pid); got != want {
			t.Errorf("pid %d: member %v, want %v", pid, got, want)
		}
	}
	if !jobGroupMember(&store.Job{ID: j.ID}, 50)(101) {
		t.Error("a session-only job's marked member refused")
	}
}

// #52 review: a recorded wrapper pid that is alive proves nothing either: the wrapper may have died and
// its pid and group id been reused by an unrelated group whose leader is alive. Neither a cancel nor a
// timeout signals that group, whether it was recorded at the spawn or adopted from wrapper.json.
func TestLiveRecycledWrapperGroupNeverSignalled(t *testing.T) {
	var kills []int
	realKill := killFn
	killFn = func(pid int, _ syscall.Signal) error { kills = append(kills, pid); return nil }
	t.Cleanup(func() { killFn = realKill })

	other := exec.Command("sleep", "30")
	other.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	other.Env = slices.DeleteFunc(os.Environ(), func(kv string) bool { return strings.HasPrefix(kv, JobEnvVar+"=") })
	if err := other.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = other.Process.Kill(); _ = other.Wait() })
	g := other.Process.Pid

	m := newManager(t)
	m.KillGrace = 200 * time.Millisecond
	for _, tc := range []struct {
		spawned, timeout bool
	}{{true, false}, {true, true}, {false, false}, {false, true}} {
		j := enqueue(t, m, "x", nil)
		tok := fmt.Sprintf("tok-%v-%v", tc.spawned, tc.timeout)
		if err := m.Store.Claim(j.ID, tok, m.Now()); err != nil {
			t.Fatal(err)
		}
		set := func() error { return m.Store.SetPID(j.ID, tok, g, g) }
		if tc.spawned {
			set = func() error { return m.Store.SetSpawnedPID(j.ID, tok, g) }
		}
		if err := set(); err != nil {
			t.Fatal(err)
		}
		if tc.timeout {
			real := time.Now
			m.Now = func() time.Time { return real().Add(time.Duration(DefaultTimeoutMinutes+1) * time.Minute) }
			if err := m.Tick(); err != nil {
				t.Fatal(err)
			}
			m.Now = time.Now
		} else if err := m.Cancel(j.ID); err != nil {
			t.Fatal(err)
		}
		if got := get(t, m, j.ID); got.Status == store.StatusRunning {
			t.Fatalf("%+v: still running", tc)
		}
		if slices.Contains(kills, -g) || slices.Contains(kills, g) {
			t.Fatalf("%+v: unrelated group %d with a live leader signalled: %v", tc, g, kills)
		}
	}
	if !alive(g) {
		t.Fatal("unrelated leader gone")
	}
}

// #52 review: the identity check keeps ending a genuine job: its live wrapper is shown to be the job's
// and its whole group is signalled on cancel.
func TestCancelEndsGenuineWrapperGroup(t *testing.T) {
	m := newManager(t)
	j := enqueue(t, m, "FAKE_SLEEP=60", nil)
	if err := m.Tick(); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "fake claude", 5*time.Second, func() bool {
		_, err := os.Stat(filepath.Join(m.JobDir(j.ID), "argv.txt"))
		return err == nil
	})
	j = get(t, m, j.ID)
	if !jobWrapper(j, m.JobDir(j.ID))(j.PID) {
		t.Fatalf("the job's own wrapper pid %d is not recognised", j.PID)
	}
	if err := m.Cancel(j.ID); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "group gone", 5*time.Second, func() bool { return !groupAlive(j.PGID) })
	if got := get(t, m, j.ID); got.Status != store.StatusCancelled {
		t.Fatalf("status %s, want cancelled", got.Status)
	}
}
