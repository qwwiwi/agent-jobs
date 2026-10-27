package jobs

import (
	"bytes"
	"errors"
	"log"
	"os"
	"os/exec"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
)

// lockedBuffer is a bytes.Buffer safe for a logger in one goroutine and the test in another.
type lockedBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (l *lockedBuffer) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.Write(p)
}

func (l *lockedBuffer) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.String()
}

// recordGroupKills records the process groups signalled through signalGroup for the test.
func recordGroupKills(t *testing.T) func() []int {
	t.Helper()
	var mu sync.Mutex
	var pgids []int
	orig := signalGroup
	signalGroup = func(pgid int, sig syscall.Signal) error {
		mu.Lock()
		pgids = append(pgids, pgid)
		mu.Unlock()
		return orig(pgid, sig)
	}
	t.Cleanup(func() { signalGroup = orig })
	return func() []int {
		mu.Lock()
		defer mu.Unlock()
		return append([]int(nil), pgids...)
	}
}

// A git that does not die at its kill (uninterruptible I/O: the SIGKILL stays pending and the process is
// never reaped) does not hold the call (#100): it returns errGitTimeout at the deadline plus the grace,
// without the output git wrote before, and the unreaped git is logged once.
func TestGitReturnsAtDeadlineWhenNotReaped(t *testing.T) {
	fakeGitOnPath(t, "echo partial\nexec sleep 60\n")
	shortGitTimeout(t, 300*time.Millisecond)
	origGrace, origUnreaped, origWait, origLog := gitKillGrace, gitUnreapedLog, gitWaitExit, gitLog
	gitKillGrace, gitUnreapedLog = 200*time.Millisecond, 200*time.Millisecond
	var logBuf lockedBuffer
	gitLog = log.New(&logBuf, "", 0)
	// The reaper does not see git's exit until released: the stand-in for a git the kernel keeps.
	release := make(chan struct{})
	gitWaitExit = func(pid int) error {
		<-release
		return origWait(pid)
	}
	t.Cleanup(func() {
		gitKillGrace, gitUnreapedLog, gitWaitExit, gitLog = origGrace, origUnreaped, origWait, origLog
	})
	released := false
	defer func() {
		if !released {
			close(release)
		}
	}()
	kills := recordGroupKills(t)

	type result struct {
		out string
		err error
	}
	start := time.Now()
	r := returnsWithin(t, 10*time.Second, "git", func() result {
		out, err := gitEnv(t.TempDir(), []string{"PATH=" + os.Getenv("PATH")}, "status")
		return result{out, err}
	})
	took := time.Since(start)
	if !errors.Is(r.err, errGitTimeout) || r.out != "" {
		t.Fatalf("want a timeout error and no output, got %q %v", r.out, r.err)
	}
	if took > 2*time.Second {
		t.Fatalf("returned after %s, want the 300ms deadline plus the 200ms grace", took)
	}
	if len(kills()) != 1 {
		t.Fatalf("git's group signalled %d times, want once", len(kills()))
	}
	for deadline := time.Now().Add(5 * time.Second); !strings.Contains(logBuf.String(), "still not reaped"); time.Sleep(20 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatalf("the unreaped git was not logged: %q", logBuf.String())
		}
	}
	if n := strings.Count(logBuf.String(), "still not reaped"); n != 1 {
		t.Fatalf("logged %d times, want once: %q", n, logBuf.String())
	}
	close(release)
	released = true
}

// The group kill reaches git's group only while git is not reaped (#100): once the reaper has reaped
// it, its PID, the group's ID, may belong to another process, and kill signals nothing.
func TestGitGroupKillOnlyBeforeReap(t *testing.T) {
	kills := recordGroupKills(t)

	fakeGitOnPath(t, "exit 0\n")
	p, err := startGit("", os.Environ(), nil, "status")
	if err != nil {
		t.Fatal(err)
	}
	if err := returnsWithin(t, 10*time.Second, "git", func() error { return <-p.done }); err != nil {
		t.Fatal(err)
	}
	p.kill()
	if k := kills(); len(k) != 0 {
		t.Fatalf("a reaped git's group was signalled: %v", k)
	}

	fakeGitOnPath(t, "exec sleep 60\n")
	p, err = startGit("", os.Environ(), nil, "status")
	if err != nil {
		t.Fatal(err)
	}
	p.kill()
	err = returnsWithin(t, 10*time.Second, "git", func() error { return <-p.done })
	if ee := (*exec.ExitError)(nil); !errors.As(err, &ee) || ee.Sys().(syscall.WaitStatus).Signal() != syscall.SIGKILL {
		t.Fatalf("a running git was not killed: %v", err)
	}
	p.kill()
	if k := kills(); len(k) != 1 || k[0] != p.cmd.Process.Pid {
		t.Fatalf("want one kill of group %d, got %v", p.cmd.Process.Pid, k)
	}
}
