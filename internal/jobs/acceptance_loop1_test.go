package jobs

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/qwwiwi/agent-jobs/internal/store"
)

// envHas reports whether the `env` output holds the line key=value.
func envHas(env, key, value string) bool {
	for _, line := range strings.Split(env, "\n") {
		if line == key+"="+value {
			return true
		}
	}
	return false
}

// liveChild starts a process that sleeps (in a group of its own, with env added) and returns it;
// the test ends it.
func liveChild(t *testing.T, env ...string) *exec.Cmd {
	t.Helper()
	c := exec.Command("sleep", "30")
	c.Env = append(os.Environ(), env...)
	c.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := c.Start(); err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	go func() { _ = c.Wait(); close(done) }()
	t.Cleanup(func() {
		_ = syscall.Kill(-c.Process.Pid, syscall.SIGKILL)
		<-done
	})
	return c
}

// exited reports whether the child started by liveChild is gone (reaped or a zombie).
func exited(pid int) bool {
	st, _, err := procStat(pid)
	return err != nil || st == "Z"
}

// runningRow inserts a running job of m (launch token "tok") started at start with a 10-minute
// timeout and an acceptance command; its wrapper pid is a live child that is never signalled (pgid 0).
func runningRow(t *testing.T, m *Manager, id string, start time.Time) *store.Job {
	t.Helper()
	w := liveChild(t)
	j := &store.Job{ID: id, Agent: m.Agent, Title: "t", Kind: "fix", Brief: "b", Status: store.StatusRunning,
		Model: "sonnet", MaxTurns: 10, MaxBudgetUSD: 1, TimeoutMinutes: 10, CreatedAt: start, StartedAt: &start,
		LaunchToken: "tok", PID: w.Process.Pid, AcceptanceCmd: "make test", Attempt: 1}
	if err := m.Store.Insert(j); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(m.JobDir(id), 0o700); err != nil {
		t.Fatal(err)
	}
	return j
}

// The timeout decision and the acceptance start are serialized: collect read the row before the
// wrapper recorded the acceptance start, then the wrapper records it before the decision. The daemon
// must not time the job out on its stale row; once the recorded deadline passes, it does.
func TestAcceptanceTimeoutRaceStartWins(t *testing.T) {
	m := acceptanceManager(t, "")
	m.KillGrace = 200 * time.Millisecond
	start := time.Now().Add(-20 * time.Minute) // past the job's 10-minute timeout
	j := runningRow(t, m, "261018-100000-race", start)
	accStart := time.Now()
	deadline := acceptanceDeadline(accStart, 60*time.Minute)
	hooked := 0
	beforeTimeoutClaim = func(id string) {
		if id != j.ID {
			return
		}
		hooked++
		// The wrapper wins the race: it records the start after collect listed the running jobs.
		if err := m.Store.SetAcceptanceStarted(id, "tok", accStart, deadline); err != nil {
			t.Errorf("acceptance start: %v", err)
		}
	}
	t.Cleanup(func() { beforeTimeoutClaim = func(string) {} })
	if err := m.collect(); err != nil {
		t.Fatal(err)
	}
	if hooked != 1 {
		t.Fatalf("hook ran %d times", hooked)
	}
	got := get(t, m, j.ID)
	if got.Status != store.StatusRunning || got.TimedOutAt != nil {
		t.Fatalf("the daemon timed out a job whose acceptance command had just started: %s %v %q", got.Status, got.TimedOutAt, got.Error)
	}
	beforeTimeoutClaim = func(string) {}
	// The next tick sees the recorded deadline: still running at +30 min.
	if err := m.collect(); err != nil {
		t.Fatal(err)
	}
	if got := get(t, m, j.ID); got.Status != store.StatusRunning {
		t.Fatalf("timed out before the acceptance deadline: %s %q", got.Status, got.Error)
	}
	// Past the recorded deadline the job times out.
	m.Now = func() time.Time { return deadline.Add(time.Minute) }
	if err := m.collect(); err != nil {
		t.Fatal(err)
	}
	if got := get(t, m, j.ID); got.Status != store.StatusTimeout || got.TimedOutAt == nil {
		t.Fatalf("not timed out after the acceptance deadline: %s %v", got.Status, got.TimedOutAt)
	}
}

// The other order: once the daemon claimed the timeout, the acceptance command can no longer start.
func TestAcceptanceTimeoutRaceClaimWins(t *testing.T) {
	m := acceptanceManager(t, "")
	m.KillGrace = 200 * time.Millisecond
	start := time.Now().Add(-20 * time.Minute)
	j := runningRow(t, m, "261018-100000-clai", start)
	var startErr error
	beforeTimeoutClaim = func(string) {}
	t.Cleanup(func() { beforeTimeoutClaim = func(string) {} })
	claimed, err := m.Store.ClaimTimeout(j.ID, "tok", time.Now())
	if err != nil || !claimed {
		t.Fatalf("claim: %v %v", claimed, err)
	}
	startErr = m.Store.SetAcceptanceStarted(j.ID, "tok", time.Now(), acceptanceDeadline(time.Now(), time.Hour))
	if !errors.Is(startErr, store.ErrImmutable) {
		t.Fatalf("acceptance started after the timeout claim: %v", startErr)
	}
	if err := m.collect(); err != nil {
		t.Fatal(err)
	}
	if got := get(t, m, j.ID); got.Status != store.StatusTimeout || got.AcceptanceStartedAt != nil {
		t.Fatalf("got %s, acceptance started %v", got.Status, got.AcceptanceStartedAt)
	}
}

// The daemon uses the deadline the wrapper recorded, not its own (possibly older) config: a wrapper
// that applied a 60-minute acceptance timeout is not cut off at the daemon's 15.
func TestAcceptanceDeadlineFromRowNotDaemonConfig(t *testing.T) {
	m := acceptanceManager(t, "")
	m.Cfg.AcceptanceTimeoutMinutes = 15
	start := time.Now().Add(-11 * time.Minute)
	j := runningRow(t, m, "261018-100000-dead", start)
	accStart := start.Add(9 * time.Minute)
	if err := m.Store.SetAcceptanceStarted(j.ID, "tok", accStart, acceptanceDeadline(accStart, 60*time.Minute)); err != nil {
		t.Fatal(err)
	}
	// 30 minutes into a 60-minute acceptance run: past the daemon's own 15 (plus grace).
	m.Now = func() time.Time { return accStart.Add(30 * time.Minute) }
	if err := m.collect(); err != nil {
		t.Fatal(err)
	}
	if got := get(t, m, j.ID); got.Status != store.StatusRunning {
		t.Fatalf("the daemon cut the acceptance run short: %s %q", got.Status, got.Error)
	}
}

// acceptance.log is read through the state dir like report.md: a hard link to another file, a
// symlinked job dir or jobs dir, a FIFO and a symlinked log are refused.
func TestAcceptanceLogTailContained(t *testing.T) {
	state := t.TempDir()
	dir := filepath.Join(state, "jobs", "j1")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	log := filepath.Join(dir, AcceptanceLog)
	if err := os.WriteFile(log, []byte("ok\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if tail, err := ReadAcceptanceLogTail(state, "j1", 100); err != nil || tail != "ok\n" {
		t.Fatalf("plain log: %q %v", tail, err)
	}
	outside := filepath.Join(t.TempDir(), "secret.txt")
	if err := os.WriteFile(outside, []byte("SECRET\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	refused := func(what string) {
		t.Helper()
		tail, err := ReadAcceptanceLogTail(state, "j1", 100)
		if err == nil || strings.Contains(tail, "SECRET") {
			t.Fatalf("%s: read %q (err %v)", what, tail, err)
		}
	}
	// Hard link: a regular file, but with two links.
	if err := os.Remove(log); err != nil {
		t.Fatal(err)
	}
	if err := os.Link(outside, log); err != nil {
		t.Fatal(err)
	}
	refused("hard-linked log")
	if _, err := ReadAcceptanceLogTail(state, "j1", 100); !errors.Is(err, ErrNotRegular) {
		t.Fatalf("hard link: %v, want ErrNotRegular", err)
	}
	if err := os.Remove(log); err != nil {
		t.Fatal(err)
	}
	if err := syscall.Mkfifo(log, 0o600); err != nil {
		t.Fatal(err)
	}
	refused("FIFO log")
	// A symlinked job dir (to a dir holding a plain acceptance.log).
	other := t.TempDir()
	if err := os.WriteFile(filepath.Join(other, AcceptanceLog), []byte("SECRET\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.RemoveAll(dir); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(other, dir); err != nil {
		t.Fatal(err)
	}
	refused("symlinked job dir")
	// A symlinked jobs dir.
	jobsReal := t.TempDir()
	if err := os.Mkdir(filepath.Join(jobsReal, "j1"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(jobsReal, "j1", AcceptanceLog), []byte("SECRET\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.RemoveAll(filepath.Join(state, "jobs")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(jobsReal, filepath.Join(state, "jobs")); err != nil {
		t.Fatal(err)
	}
	refused("symlinked jobs dir")
	for _, bad := range []string{"", ".", "..", "a/b"} {
		if _, err := ReadAcceptanceLogTail(state, bad, 10); err == nil {
			t.Fatalf("id %q accepted", bad)
		}
	}
}

// Without config acceptance_allowed the command is refused at enqueue; when it is turned off after the
// job was queued, the wrapper (which reloads config.json) does not run it and the job fails closed.
func TestAcceptanceOptIn(t *testing.T) {
	m := newManager(t)
	_, err := m.Enqueue(StartRequest{Title: "t", Kind: "fix", Brief: "b", Model: "sonnet", AcceptanceCmd: "make test"})
	if err == nil || !strings.Contains(err.Error(), "acceptance_allowed") {
		t.Fatalf("enqueue without opt-in: %v", err)
	}
	if _, err := m.AddSchedule(ScheduleRequest{Cron: "@daily", Prompt: "p", Kind: "fix", Model: "sonnet",
		AcceptanceCmd: "make test"}); err == nil || !strings.Contains(err.Error(), "acceptance_allowed") {
		t.Fatalf("schedule without opt-in: %v", err)
	}
	// No command needs no opt-in.
	if _, err := m.Enqueue(StartRequest{Title: "t", Kind: "fix", Brief: "b", Model: "sonnet", AcceptanceCmd: " "}); err != nil {
		t.Fatal(err)
	}
	m = acceptanceManager(t, "")
	j := enqueue(t, m, "do it", withAcceptance(`touch ../acc-ran`))
	if err := os.WriteFile(filepath.Join(m.StateDir, "config.json"), []byte(`{"acceptance_allowed": false}`), 0o600); err != nil {
		t.Fatal(err)
	}
	j = runToEnd(t, m, j.ID)
	if j.Status != store.StatusFailed || !strings.HasPrefix(j.Error, "acceptance did not run") {
		t.Fatalf("got %s %q", j.Status, j.Error)
	}
	if _, err := os.Stat(filepath.Join(m.JobDir(j.ID), "acc-ran")); err == nil {
		t.Fatal("acceptance ran with acceptance_allowed off")
	}
	if line := DoneLine(j); !strings.Contains(line, "· acceptance did not run") {
		t.Fatalf("done line: %s", line)
	}
}

// A job failed for a missing or unreadable acceptance result says so in its done line.
func TestAcceptanceNoteWithoutResult(t *testing.T) {
	now := time.Now()
	for errText, want := range map[string]string{
		"acceptance did not run (see stderr.log)":                                                       "acceptance did not run",
		"acceptance result unreadable: database locked":                                                 "acceptance result unreadable",
		"acceptance did not run (see stderr.log); 1 process(es) outside the job's process group killed": "acceptance did not run",
	} {
		j := &store.Job{ID: "261018-100000-note", Title: "t", Kind: "fix", Status: store.StatusFailed, Model: "sonnet",
			CreatedAt: now, StartedAt: &now, FinishedAt: &now, AcceptanceCmd: "make test", Error: errText}
		if n := acceptanceNote(j); n != want {
			t.Fatalf("%q: note %q, want %q", errText, n, want)
		}
		if line := DoneLine(j); !strings.Contains(line, "· "+want) {
			t.Fatalf("done line: %s", line)
		}
	}
	// A job failed for another reason, without an acceptance result, has no acceptance note.
	j := &store.Job{AcceptanceCmd: "make test", Error: "claude exited with code 3"}
	if n := acceptanceNote(j); n != "" {
		t.Fatalf("note %q", n)
	}
}

// The daemon's kill of a job (timeout, cancel, stop) also ends the acceptance command's process group
// recorded in the row, which is not the wrapper's group; a group whose leader carries no marker of
// the job is never signalled.
func TestKillJobEndsAcceptanceGroup(t *testing.T) {
	m := acceptanceManager(t, "")
	m.KillGrace = 500 * time.Millisecond
	start := time.Now()
	j := runningRow(t, m, "261018-100000-kill", start)
	acc := liveChild(t, JobEnvVar+"="+j.ID)
	if err := m.Store.SetAcceptancePGID(j.ID, "tok", acc.Process.Pid); err != nil {
		t.Fatal(err)
	}
	m.killJob(j, 0, 0) // no scope, no wrapper group recorded
	waitFor(t, "acceptance group gone", 3*time.Second, func() bool { return exited(acc.Process.Pid) })

	stranger := liveChild(t) // no job marker: e.g. a recycled pid
	if err := m.Store.SetAcceptancePGID(j.ID, "tok", stranger.Process.Pid); err != nil {
		t.Fatal(err)
	}
	m.killJob(j, 0, 0)
	time.Sleep(300 * time.Millisecond)
	if exited(stranger.Process.Pid) {
		t.Fatal("killed a process group without the job's marker")
	}
}

// A stop signal to the wrapper while the acceptance command runs ends the command's whole group (a
// background child too) and records nothing.
func TestAcceptanceSignalEndsGroup(t *testing.T) {
	dir := t.TempDir()
	if err := os.Mkdir(filepath.Join(dir, "work"), 0o700); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var pgid int
	go func() {
		waitFor(t, "child pid", 5*time.Second, func() bool {
			b, err := os.ReadFile(filepath.Join(dir, "child.pid"))
			return err == nil && len(strings.TrimSpace(string(b))) > 0
		})
		cancel()
	}()
	start := time.Now()
	exit, _ := runAcceptance(ctx, dir, `sleep 30 & echo $! > ../child.pid; sleep 30`, time.Minute, io.Discard,
		func(g int) { pgid = g })
	if exit != acceptanceSignalled || pgid <= 1 {
		t.Fatalf("exit %d pgid %d", exit, pgid)
	}
	if took := time.Since(start); took > acceptanceKillGrace+5*time.Second {
		t.Fatalf("took %s", took)
	}
	var child int
	if _, err := fmt.Sscan(readFile(t, filepath.Join(dir, "child.pid")), &child); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "background child gone", 3*time.Second, func() bool { return exited(child) })
}

// Cancelling a job while its acceptance command runs leaves nothing of it running: a sleeping child
// that would outlive the command is gone, and the row is cancelled.
func TestCancelDuringAcceptance(t *testing.T) {
	m := acceptanceManager(t, "")
	m.KillGrace = 2 * time.Second
	j := enqueue(t, m, "do it", withAcceptance(`sleep 60 & echo $! > ../child.pid; wait`))
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
	if err := m.Cancel(j.ID); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "acceptance child gone after cancel", 5*time.Second, func() bool { return exited(child) })
	if got := get(t, m, j.ID); got.Status != store.StatusCancelled {
		t.Fatalf("status %s", got.Status)
	}
}
