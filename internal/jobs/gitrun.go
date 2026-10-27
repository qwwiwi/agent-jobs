package jobs

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log"
	"os"
	"os/exec"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
)

// The bounded runner of the daemon's git calls (#100). A call returns by its deadline plus gitKillGrace
// whatever git does: at the deadline git's process group is killed, and when git is still not reaped
// after the grace (a process in uninterruptible I/O, such as on a stalled NFS mount, keeps its SIGKILL
// pending, and Wait cannot return) the call returns errGitTimeout anyway and closes the output pipes it
// copies, so that nothing but the background reaping stays. The reaping goes on in the background and is
// logged once when it lasts past gitUnreapedLog. A call that did not finish never hands out its output.
// Variables only so that tests replace them.
var (
	gitKillGrace   = 2 * time.Second
	gitUnreapedLog = 10 * time.Minute
	// gitWaitDelay is how long the output copies may go on after git has exited (a process of its group
	// still holding stdout or stderr) before that group is killed and the pipes are closed.
	gitWaitDelay = time.Second
	// gitWaitExit blocks until process pid has exited, without reaping it (gitrun_wait_*.go).
	gitWaitExit = waitExitNoReap
	// signalGroup sends sig to process group pgid.
	signalGroup = func(pgid int, sig syscall.Signal) error { return syscall.Kill(-pgid, sig) }
	gitLog      = log.Default()
)

// errGitOutputCut is the result of a git whose output copies were cut off: its group still held the
// output past gitWaitDelay after git's exit, or the call ended first.
var errGitOutputCut = errors.New("git's output was cut off: a process of its group still held it")

// gitProc is a started git of the daemon, the leader of its own process group.
type gitProc struct {
	cmd  *exec.Cmd
	name string     // "git <args>", for the log
	done chan error // the result once git is reaped and its output copied; buffered, so the reaper never blocks on it

	// outs are the read ends of the output pipes the runner copies (Stdout and Stderr writers that are no
	// file); copied is closed once every copy has ended. closeOutputs closes outs, which ends the copies;
	// cut records that it did so before they ended on their own.
	outs     []*os.File
	copied   chan struct{}
	copyErr  error // the first copy error, read only after copied
	outOnce  sync.Once
	cut      atomic.Bool
	killed   chan struct{} // closed by kill, after the group was signalled
	killOnce sync.Once

	// mu orders the group kill against the reaping. reap sets reaped under mu right before git is
	// reaped, and kill signals the group only under mu while reaped is false. Until then git is running
	// or a zombie, so its PID, which is the group's ID, stays allocated: the group kill reaches git's
	// group, also the processes left in it once git has exited, and never a group that took the ID over
	// after git was reaped.
	mu     sync.Mutex
	reaped bool
}

// startGit starts git with args in dir ("" = the daemon's working directory) and environment env plus
// GIT_TERMINAL_PROMPT=0 (git never waits on a prompt, whatever env holds), in its own process group.
// setup, when not nil, sets the command's standard streams before the start: Stdin nil or an *os.File,
// Stdout and Stderr distinct writers. A writer that is no *os.File is fed by the runner through a pipe
// of its own, never by exec, so that the runner can close it (see gitProc.outs).
func startGit(dir string, env []string, setup func(*exec.Cmd), args ...string) (*gitProc, error) {
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = append(slices.Clip(env), "GIT_TERMINAL_PROMPT=0")
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if setup != nil {
		setup(cmd)
	}
	p := &gitProc{cmd: cmd, name: "git " + strings.Join(args, " "), done: make(chan error, 1),
		copied: make(chan struct{}), killed: make(chan struct{})}
	var dsts []io.Writer
	var ends []*os.File // the write ends, git's after the start
	closeEnds := func() {
		for _, f := range ends {
			_ = f.Close()
		}
	}
	for _, w := range []*io.Writer{&cmd.Stdout, &cmd.Stderr} {
		if _, isFile := (*w).(*os.File); *w == nil || isFile {
			continue
		}
		r, pw, err := os.Pipe()
		if err != nil {
			closeEnds()
			p.closeOutputs()
			return nil, err
		}
		p.outs, dsts, ends = append(p.outs, r), append(dsts, *w), append(ends, pw)
		*w = pw
	}
	err := cmd.Start()
	closeEnds()
	if err != nil {
		p.closeOutputs()
		return nil, err
	}
	var copies sync.WaitGroup
	var errMu sync.Mutex
	for i, r := range p.outs {
		copies.Go(func() {
			if _, err := io.Copy(dsts[i], r); err != nil && !p.cut.Load() {
				errMu.Lock()
				if p.copyErr == nil {
					p.copyErr = err
				}
				errMu.Unlock()
			}
		})
	}
	go func() {
		copies.Wait()
		close(p.copied)
	}()
	go p.reap()
	return p, nil
}

// closeOutputs closes the read ends of the output pipes (once): copies still going end there.
func (p *gitProc) closeOutputs() {
	p.outOnce.Do(func() {
		for _, r := range p.outs {
			_ = r.Close()
		}
	})
}

// cutOutputs closes the output pipes before the copies have ended on their own: the call has no output.
func (p *gitProc) cutOutputs() {
	p.cut.Store(true)
	p.closeOutputs()
}

// reap waits for git's exit and its output, then marks it reaped and only then reaps it (see mu). Once git
// has exited, a process of its group that still holds the output past gitWaitDelay or the kill is ended
// with the group while git, a zombie, still reserves the group's ID, and the pipes are closed.
func (p *gitProc) reap() {
	if err := gitWaitExit(p.cmd.Process.Pid); err != nil {
		// Nothing to wait for (no such child): no group is signalled from now on, and Wait reports it.
		p.mu.Lock()
		p.reaped = true
		p.mu.Unlock()
		p.cutOutputs()
	}
	select {
	case <-p.copied:
	default:
		t := time.NewTimer(gitWaitDelay)
		select {
		case <-p.copied:
		case <-p.killed: // kill has signalled the group already
			p.cutOutputs()
		case <-t.C:
			p.signal()
			p.cutOutputs()
		}
		t.Stop()
	}
	p.mu.Lock()
	p.reaped = true
	p.mu.Unlock()
	err := p.cmd.Wait()
	<-p.copied
	p.closeOutputs()
	// A cut output is no result, also beside an exit status whose output a caller would read.
	if p.cut.Load() {
		var ee *exec.ExitError
		if err == nil || errors.As(err, &ee) {
			err = errGitOutputCut
		}
	}
	if err == nil {
		err = p.copyErr
	}
	p.done <- err
}

// signal sends SIGKILL to git's process group, a no-op once git is reaped.
func (p *gitProc) signal() {
	p.mu.Lock()
	defer p.mu.Unlock()
	if !p.reaped {
		_ = signalGroup(p.cmd.Process.Pid, syscall.SIGKILL)
	}
}

// kill ends git's process group (signal), also when git has exited and a process of the group holds its
// output, and has the reaper close the output pipes.
func (p *gitProc) kill() {
	p.signal()
	p.killOnce.Do(func() { close(p.killed) })
}

// wait returns git's result, or errGitTimeout once ctx has ended: then git's group is killed and git gets
// gitKillGrace to be reaped. A git that exits successfully with its whole output by then is no timeout;
// any other end is errGitTimeout, also when git is not reaped yet: then the output pipes are closed and
// the reaping is left to reap, watched by watchUnreaped.
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
		p.cutOutputs()
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
