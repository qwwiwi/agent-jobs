package jobs

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/qwwiwi/agent-jobs/internal/store"
)

// Cancel with a daemon grace shorter than the wrapper's acceptance grace: the acceptance shell dies on
// SIGTERM, a child that ignores SIGTERM keeps its stdout open, so the wrapper is still waiting for the
// pipes when the daemon kills it. The child (whose group leader is gone by then) must still be ended,
// well before it would exit on its own.
func TestCancelAcceptanceGroupAfterLeaderExit(t *testing.T) {
	m := acceptanceManager(t, "")
	m.KillGrace = time.Second
	j := enqueue(t, m, "do it", withAcceptance(`(trap '' TERM; exec sleep 60) & echo $! > ../child.pid; wait`))
	if err := m.Tick(); err != nil {
		t.Fatal(err)
	}
	pidFile := filepath.Join(m.JobDir(j.ID), "child.pid")
	waitFor(t, "acceptance child", 15*time.Second, func() bool {
		b, err := os.ReadFile(pidFile)
		return err == nil && len(strings.TrimSpace(string(b))) > 0
	})
	var child int
	if _, err := fmt.Sscan(readFile(t, pidFile), &child); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = syscall.Kill(child, syscall.SIGKILL) })
	waitFor(t, "acceptance pgid recorded", 5*time.Second, func() bool { return get(t, m, j.ID).AcceptancePGID > 1 })
	pgid := get(t, m, j.ID).AcceptancePGID
	time.Sleep(200 * time.Millisecond) // let the shell reach its wait
	// The daemon's kill alone (Cancel also sweeps marked leftovers afterwards, which would hide a gap).
	start := time.Now()
	m.killRunning(get(t, m, j.ID))
	if !exited(child) {
		t.Fatalf("acceptance child %d (group %d) still running after the kill", child, pgid)
	}
	if took := time.Since(start); took >= acceptanceKillGrace {
		t.Fatalf("child ended only after %s (the wrapper's own grace)", took)
	}
	if err := m.Cancel(j.ID); err != nil {
		t.Fatal(err)
	}
	if got := get(t, m, j.ID); got.Status != store.StatusCancelled {
		t.Fatalf("status %s", got.Status)
	}
}

// startOrphanGroup starts `sh` in a process group of its own (env added) that backgrounds a sleep and
// exits at once, and returns the sleep's pid once the shell has been reaped: a group without a leader.
func startOrphanGroup(t *testing.T, env ...string) (pgid, member int) {
	t.Helper()
	return startOrphanGroupScript(t, `sleep 30 >/dev/null 2>&1 & echo $!`, env...)
}

// startOrphanGroupScript is startOrphanGroup with the shell script given; the script prints the pid
// that the caller watches.
func startOrphanGroupScript(t *testing.T, script string, env ...string) (pgid, member int) {
	t.Helper()
	c := exec.Command("sh", "-c", script)
	c.Env = append(os.Environ(), env...)
	c.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	out, err := c.Output()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := fmt.Sscan(string(out), &member); err != nil {
		t.Fatal(err)
	}
	pgid = c.Process.Pid
	t.Cleanup(func() { _ = syscall.Kill(-pgid, syscall.SIGKILL) })
	if runtime.GOOS == "darwin" {
		// $! can be printed before exec finishes; the transient shell has no readable
		// environment under SIP. Wait for the child whose identity the test needs.
		waitFor(t, "orphan sleep identity", 3*time.Second, func() bool {
			info, err := sysctlProc(member)
			return err == nil && !info.zombie && info.pgid == pgid && len(info.argv) == 2 && filepath.Base(info.argv[0]) == "sleep"
		})
	}
	return pgid, member
}

// A leaderless group with no marker of this job is never signalled. Linux requires every member's
// marker; Darwin proves group ownership through a live marked member, then signals the whole group.
func TestKillAcceptanceLeaderlessGroup(t *testing.T) {
	m := acceptanceManager(t, "")
	m.KillGrace = 500 * time.Millisecond
	j := runningRow(t, m, "261018-110000-orph", time.Now())

	g, member := startOrphanGroup(t, JobEnvVar+"="+j.ID)
	if err := m.Store.SetAcceptancePGID(j.ID, "tok", g); err != nil {
		t.Fatal(err)
	}
	m.killAcceptance(j.ID)
	waitFor(t, "marked leaderless group gone", 3*time.Second, func() bool { return exited(member) })

	g, member = startOrphanGroup(t) // no marker
	if err := m.Store.SetAcceptancePGID(j.ID, "tok", g); err != nil {
		t.Fatal(err)
	}
	m.killAcceptance(j.ID)
	time.Sleep(300 * time.Millisecond)
	if exited(member) {
		t.Fatal("killed a leaderless group whose member carries no marker of the job")
	}

	g, member = startOrphanGroup(t, JobEnvVar+"=261018-110000-othr") // another job's marker
	if err := m.Store.SetAcceptancePGID(j.ID, "tok", g); err != nil {
		t.Fatal(err)
	}
	m.killAcceptance(j.ID)
	time.Sleep(300 * time.Millisecond)
	if exited(member) {
		t.Fatal("killed a leaderless group of another job")
	}

	// Mixed: Linux refuses per-member kills; Darwin's group proof covers the unmarked sibling too.
	g, member = startOrphanGroupScript(t, `env -u `+JobEnvVar+` sleep 30 >/dev/null 2>&1 & sleep 30 >/dev/null 2>&1 & echo $!`,
		JobEnvVar+"="+j.ID)
	if err := m.Store.SetAcceptancePGID(j.ID, "tok", g); err != nil {
		t.Fatal(err)
	}
	m.killAcceptance(j.ID)
	if runtime.GOOS == "darwin" {
		waitFor(t, "mixed group gone", 3*time.Second, func() bool { return !groupAlive(g) })
		return
	}
	time.Sleep(300 * time.Millisecond)
	if exited(member) {
		t.Fatal("killed a leaderless group that also holds a member without the job's marker")
	}
}
