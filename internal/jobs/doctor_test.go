package jobs

import (
	"os"
	"os/exec"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/qwwiwi/agent-jobs/internal/store"
)

// execSelf runs the built binary with the test's environment (AGENT_JOBS_HOME of the test).
func execSelf(args ...string) (string, error) {
	out, err := exec.Command(selfBin, args...).CombinedOutput()
	return string(out), err
}

// spawnOwn starts a process in its own session and registers a kill for it: the test only ever
// signals processes it spawned itself.
func spawnOwn(t *testing.T, env []string, name string, args ...string) *exec.Cmd {
	t.Helper()
	c := exec.Command(name, args...)
	c.Env = append(os.Environ(), env...)
	c.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := c.Start(); err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	go func() { _ = c.Wait(); close(done) }()
	t.Cleanup(func() {
		_ = syscall.Kill(c.Process.Pid, syscall.SIGKILL)
		<-done
	})
	return c
}

// pidAlive reports whether pid is a live (non-zombie) process.
func pidAlive(pid int) bool {
	st, _, err := procStat(pid)
	return err == nil && st != "Z"
}

func findGhost(rep ProcsReport, pid int) *Ghost {
	for i := range rep.Ghosts {
		if rep.Ghosts[i].PID == pid {
			return &rep.Ghosts[i]
		}
	}
	return nil
}

// Reference: doctor procs finds processes that outlived their job; it only reports by default and
// kills on a flag. A process carrying the marker of a finished job is found, survives the report and
// is ended only with kill; a live job's process is never reported.
func TestDoctorProcsMarkerGhostKilledOnlyWithFlag(t *testing.T) {
	m := newManager(t)
	finished := time.Now().Add(-time.Minute)
	dead := insertJob(t, m, "doctor-dead-"+strconv.Itoa(os.Getpid()), store.StatusFailed, &finished, nil)
	ghost := spawnOwn(t, []string{JobEnvVar + "=" + dead.ID}, "sleep", "300")

	// A running job with a live wrapper: its process is the job's business, not a ghost.
	wrapper := spawnOwn(t, nil, "sleep", "300")
	live := insertJob(t, m, "doctor-live-"+strconv.Itoa(os.Getpid()), store.StatusRunning, nil, func(j *store.Job) {
		j.PID, j.PGID = wrapper.Process.Pid, wrapper.Process.Pid
	})
	liveProc := spawnOwn(t, []string{JobEnvVar + "=" + live.ID}, "sleep", "300")
	// A marker of a job this agent does not know (another agent's): never judged.
	foreign := spawnOwn(t, []string{JobEnvVar + "=not-in-this-store"}, "sleep", "300")

	waitFor(t, "spawned processes", 5*time.Second, func() bool { return procMarker(ghost.Process.Pid) == dead.ID })
	rep, err := m.DoctorProcs(false)
	if err != nil {
		t.Fatal(err)
	}
	if !rep.Supported {
		t.Skip("no /proc")
	}
	g := findGhost(rep, ghost.Process.Pid)
	if g == nil || g.Kind != GhostJob || g.JobID != dead.ID || g.Killed {
		t.Fatalf("ghost not reported as the dead job's: %+v (all: %+v)", g, rep.Ghosts)
	}
	for _, p := range []int{liveProc.Process.Pid, foreign.Process.Pid, wrapper.Process.Pid} {
		if findGhost(rep, p) != nil {
			t.Fatalf("pid %d must not be reported: %+v", p, rep.Ghosts)
		}
	}
	time.Sleep(200 * time.Millisecond)
	if !pidAlive(ghost.Process.Pid) {
		t.Fatal("report-only run killed the ghost")
	}

	// The CLI without --kill reports and kills nothing either.
	out, err := execSelf("doctor", "procs", "--agent", "test")
	if err != nil || !strings.Contains(out, strconv.Itoa(ghost.Process.Pid)) || !strings.Contains(out, "report only") {
		t.Fatalf("doctor procs CLI: %v\n%s", err, out)
	}
	if !pidAlive(ghost.Process.Pid) {
		t.Fatal("CLI report-only run killed the ghost")
	}

	rep, err = m.DoctorProcs(true)
	if err != nil {
		t.Fatal(err)
	}
	if g := findGhost(rep, ghost.Process.Pid); g == nil || !g.Killed {
		t.Fatalf("ghost not killed with the flag: %+v", g)
	}
	waitFor(t, "ghost gone", 5*time.Second, func() bool { return !pidAlive(ghost.Process.Pid) })
	for _, p := range []int{liveProc.Process.Pid, foreign.Process.Pid, wrapper.Process.Pid} {
		if !pidAlive(p) {
			t.Fatalf("pid %d was killed but is not a ghost", p)
		}
	}
}

// A markerless process left in the session of a finished job (its wrapper, the session leader, is
// gone) is found by its session id and killed with the flag (here through the CLI's --kill).
func TestDoctorProcsSessionGhost(t *testing.T) {
	m := newManager(t)
	// sh leads a new session, starts a detached sleep in it and exits: the sleep is a re-parented
	// orphan whose session id is the dead sh's pid, like a leftover of a killed wrapper.
	c := exec.Command("sh", "-c", `sleep 300 >/dev/null 2>&1 & echo $!`)
	c.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	out, err := c.Output()
	if err != nil {
		t.Fatal(err)
	}
	leader := c.Process.Pid
	pid, err := strconv.Atoi(strings.TrimSpace(string(out)))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if _, sid, err := procStat(pid); err == nil && sid == leader {
			_ = syscall.Kill(pid, syscall.SIGKILL)
		}
	})
	finished := time.Now()
	started := time.Now().Add(-time.Minute)
	j := insertJob(t, m, "doctor-session-"+strconv.Itoa(os.Getpid()), store.StatusCancelled, &finished, func(j *store.Job) {
		j.PID, j.PGID, j.StartedAt = leader, leader, &started
	})
	rep, err := m.DoctorProcs(false)
	if err != nil {
		t.Fatal(err)
	}
	if !rep.Supported {
		t.Skip("no /proc")
	}
	g := findGhost(rep, pid)
	if g == nil || g.Kind != GhostJob || g.JobID != j.ID || g.SID != leader {
		t.Fatalf("session ghost not found: %+v (all: %+v)", g, rep.Ghosts)
	}
	if !pidAlive(pid) {
		t.Fatal("report killed the ghost")
	}
	if out, err := execSelf("doctor", "procs", "--agent", "test", "--kill"); err != nil || !strings.Contains(out, "KILLED") {
		t.Fatalf("doctor procs --kill: %v\n%s", err, out)
	}
	waitFor(t, "session ghost gone", 5*time.Second, func() bool { return !pidAlive(pid) })
}

// A process that started before the job (a reused session id) is not the job's.
func TestDoctorProcsIgnoresOlderProcessInSession(t *testing.T) {
	m := newManager(t)
	c := exec.Command("sh", "-c", `sleep 300 >/dev/null 2>&1 & echo $!`)
	c.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	out, err := c.Output()
	if err != nil {
		t.Fatal(err)
	}
	pid, _ := strconv.Atoi(strings.TrimSpace(string(out)))
	t.Cleanup(func() { _ = syscall.Kill(pid, syscall.SIGKILL) })
	later := time.Now().Add(time.Hour) // the job "started" after the process
	insertJob(t, m, "doctor-later-"+strconv.Itoa(os.Getpid()), store.StatusFailed, &later, func(j *store.Job) {
		j.PID, j.PGID, j.StartedAt = c.Process.Pid, c.Process.Pid, &later
	})
	rep, err := m.DoctorProcs(true)
	if err != nil {
		t.Fatal(err)
	}
	if g := findGhost(rep, pid); g != nil && g.Kind == GhostJob {
		t.Fatalf("older process attributed to the job: %+v", g)
	}
	if !pidAlive(pid) {
		t.Fatal("older process was killed")
	}
}

// killGhost never signals a process whose start time differs from the scanned one (pid reuse).
func TestKillGhostRefusesChangedIdentity(t *testing.T) {
	p := spawnOwn(t, nil, "sleep", "300")
	info, err := procStatFull(p.Process.Pid)
	if err != nil {
		t.Skip("no /proc")
	}
	if err := killGhost(p.Process.Pid, info.sid, info.start+1); err == nil || !strings.Contains(err.Error(), "no longer") {
		t.Fatalf("want refusal, got %v", err)
	}
	time.Sleep(100 * time.Millisecond)
	if !pidAlive(p.Process.Pid) {
		t.Fatal("process with a different identity was signalled")
	}
	if err := killGhost(p.Process.Pid, info.sid, info.start); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "killed", 5*time.Second, func() bool { return !pidAlive(p.Process.Pid) })
}

// Running rows with a dead wrapper pid are reported.
func TestDoctorProcsDeadRunningRow(t *testing.T) {
	m := newManager(t)
	c := exec.Command("true")
	if err := c.Run(); err != nil {
		t.Fatal(err)
	}
	insertJob(t, m, "deadrow", store.StatusRunning, nil, func(j *store.Job) { j.PID = c.Process.Pid })
	rep, err := m.DoctorProcs(false)
	if err != nil {
		t.Fatal(err)
	}
	if len(rep.DeadRows) != 1 || rep.DeadRows[0].ID != "deadrow" {
		t.Fatalf("dead rows: %+v", rep.DeadRows)
	}
}

func TestSuspectKind(t *testing.T) {
	for _, c := range []struct {
		argv []string
		want string
	}{
		{[]string{"/opt/chrome/chrome-headless-shell", "--headless"}, "browser"},
		{[]string{"node", "/x/node_modules/playwright-core/cli.js", "run-server"}, "browser"},
		{[]string{"node", "/x/node_modules/.bin/vite", "--port", "5173"}, "preview server"},
		{[]string{"python3", "-m", "http.server", "8000"}, "preview server"},
		{[]string{"npm", "run", "dev"}, "preview server"},
		{[]string{"bash", "-c", "while pgrep -f server; do sleep 1; done"}, "wait loop"},
		{[]string{"/bin/sh", "-c", "until kill -0 1; do sleep 5; done"}, "wait loop"},
		{[]string{"bash", "-c", "make test"}, ""},
		{[]string{"sleep", "300"}, ""},
		{nil, ""},
	} {
		if got := SuspectKind(c.argv); got != c.want {
			t.Errorf("%q: got %q, want %q", c.argv, got, c.want)
		}
	}
}
