package jobs

import (
	"bytes"
	"context"
	"errors"
	"log"
	"os/exec"
	"slices"
	"strings"
	"sync"
	"syscall"
	"time"

	"golang.org/x/sys/unix"
)

// The bounded runner of the daemon's git calls (#100). A call returns by its deadline plus gitKillGrace
// whatever git does: at the deadline git's process group is killed, and when git is still not reaped
// after the grace (a process in uninterruptible I/O, such as on a stalled NFS mount, keeps its SIGKILL
// pending, and Wait cannot return) the call returns errGitTimeout anyway. The reaping goes on in the
// background and is logged once when it lasts past gitUnreapedLog. A call that did not finish never
// hands out its output. Variables only so that tests replace them.
var (
	gitKillGrace   = 2 * time.Second
	gitUnreapedLog = 10 * time.Minute
	// gitWaitExit blocks until process pid has exited, without reaping it.
	gitWaitExit = waitExitNoReap
	// signalGroup sends sig to process group pgid.
	signalGroup = func(pgid int, sig syscall.Signal) error { return syscall.Kill(-pgid, sig) }
	gitLog      = log.Default()
)

// gitProc is a started git of the daemon, the leader of its own process group.
type gitProc struct {
	cmd  *exec.Cmd
	name string     // "git <args>", for the log
	done chan error // cmd.Wait's result once git is reaped; buffered, so the reaper never blocks on it

	// mu orders the group kill against the reaping. reap sets exited under mu before git is reaped, and
	// kill signals the group only under mu while exited is false. Until then git is running or a zombie,
	// so its PID, which is the group's ID, stays allocated: the group kill reaches git's group and never
	// a group that took the ID over after git was reaped.
	mu     sync.Mutex
	exited bool
}

// startGit starts git with args in dir ("" = the daemon's working directory) and environment env plus
// GIT_TERMINAL_PROMPT=0 (git never waits on a prompt, whatever env holds), in its own process group.
// setup, when not nil, sets the command's standard streams before the start.
func startGit(dir string, env []string, setup func(*exec.Cmd), args ...string) (*gitProc, error) {
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = append(slices.Clip(env), "GIT_TERMINAL_PROMPT=0")
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	// Once git has exited, a process that left its group and still holds git's stdout or stderr does not
	// hold Wait: the pipes are closed after WaitDelay.
	cmd.WaitDelay = time.Second
	if setup != nil {
		setup(cmd)
	}
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	p := &gitProc{cmd: cmd, name: "git " + strings.Join(args, " "), done: make(chan error, 1)}
	go p.reap()
	return p, nil
}

// reap waits for git's exit, marks it exited and only then reaps it (see mu).
func (p *gitProc) reap() {
	// An error here (no such child) leaves nothing to wait for: exited is set all the same, so no group
	// is signalled from now on, and Wait reports the error.
	_ = gitWaitExit(p.cmd.Process.Pid)
	p.mu.Lock()
	p.exited = true
	p.mu.Unlock()
	p.done <- p.cmd.Wait()
}

// kill sends SIGKILL to git's process group, a no-op once git has exited.
func (p *gitProc) kill() {
	p.mu.Lock()
	defer p.mu.Unlock()
	if !p.exited {
		_ = signalGroup(p.cmd.Process.Pid, syscall.SIGKILL)
	}
}

// wait returns git's Wait result, or errGitTimeout once ctx has ended: then git's group is killed and git
// gets gitKillGrace to be reaped. A git that exits successfully by then is no timeout, its output is
// whole; any other end is errGitTimeout, also when git is not reaped yet (the reaping is left to reap,
// watched by watchUnreaped).
func (p *gitProc) wait(ctx context.Context) error {
	select {
	case err := <-p.done:
		return err
	case <-ctx.Done():
	}
	p.kill()
	grace := time.NewTimer(gitKillGrace)
	defer grace.Stop()
	select {
	case err := <-p.done:
		if err == nil {
			return nil
		}
	case <-grace.C:
		go p.watchUnreaped()
	}
	return errGitTimeout
}

// watchUnreaped logs once when git, killed at its timeout, is still not reaped after gitUnreapedLog.
func (p *gitProc) watchUnreaped() {
	t := time.NewTimer(gitUnreapedLog)
	defer t.Stop()
	select {
	case <-p.done:
	case <-t.C:
		gitLog.Printf("warning: %s (pid %d) is still not reaped %s after it was killed at its timeout "+
			"(stuck in uninterruptible I/O?); the daemon goes on and reaps it when the kernel lets it go",
			p.name, p.cmd.Process.Pid, gitKillGrace+gitUnreapedLog)
	}
}

// runGit runs git (startGit) bounded by ctx (gitProc.wait) and returns its stdout and stderr. A non-zero
// exit is an *exec.ExitError returned with both outputs; git still running when ctx ends is
// errGitTimeout; any error but an exit status comes without output.
func runGit(ctx context.Context, dir string, env []string, args ...string) (stdout, stderr []byte, err error) {
	var out, errOut bytes.Buffer
	p, err := startGit(dir, env, func(c *exec.Cmd) { c.Stdout, c.Stderr = &out, &errOut }, args...)
	if err != nil {
		return nil, nil, err
	}
	// After a timeout the buffers are not read: the abandoned Wait may still be writing them.
	if err := p.wait(ctx); err != nil {
		if ee := (*exec.ExitError)(nil); errors.As(err, &ee) {
			return out.Bytes(), errOut.Bytes(), err
		}
		return nil, nil, err
	}
	return out.Bytes(), errOut.Bytes(), nil
}

// runGitFor is runGit bounded by timeout.
func runGitFor(timeout time.Duration, dir string, env []string, args ...string) (stdout, stderr []byte, err error) {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	return runGit(ctx, dir, env, args...)
}

// waitExitNoReap blocks until process pid has exited and leaves it a zombie (waitid WNOWAIT).
func waitExitNoReap(pid int) error {
	for {
		var info unix.Siginfo
		err := unix.Waitid(unix.P_PID, pid, &info, unix.WEXITED|unix.WNOWAIT, nil)
		if !errors.Is(err, unix.EINTR) {
			return err
		}
	}
}
