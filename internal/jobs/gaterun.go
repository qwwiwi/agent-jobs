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
	"time"
)

// Serialized gate runner (#134): `agent-jobs gate run` runs one heavy local gate (check.sh, a pg suite,
// a vite build) for the orchestrator so that
//   - one gate runs at a time per box: an exclusive flock on <agent-jobs home>/gate.lock, shared by
//     every agent of the user; a waiter gives up after its lock wait instead of hanging;
//   - it runs on the commit it claims to check: HEAD of the work tree must resolve to the asserted
//     commit once the lock is held, and still after the command (a checkout meanwhile fails the gate);
//   - it always ends with the line exit=N on stdout, whatever happened (a refusal, a missing command, a
//     timeout, a stop signal), so a waiter never waits for a marker that never comes;
//   - the command is the leader of a process group of its own: on the timeout or a stop the whole group
//     gets SIGTERM, then SIGKILL after gateKillGrace, and once the leader has exited (before it is
//     reaped, so the group id cannot be reused) the group gets SIGKILL, so no orphan suite outlives it;
//   - TELEGRAM_* is dropped from the command's environment.
//
// Exit codes: the command's own, 128+signal when a signal ended it, GateExitTimeout on the timeout,
// GateExitRunner when the runner refused or failed (bad arguments, no lock, wrong HEAD), 127 when the
// command could not start.

// Gate runner exit codes and defaults.
const (
	GateExitTimeout     = 124 // as timeout(1)
	GateExitRunner      = 125 // as timeout(1): the runner itself failed
	GateExitNotStarted  = 127
	DefaultGateTimeout  = 30 * time.Minute
	DefaultGateLockWait = 60 * time.Minute
	GateLockName        = "gate.lock"
	gateKillGrace       = 5 * time.Second
	gateLockPoll        = 100 * time.Millisecond
	gateHolderBytes     = 512
)

// gateEnvDropPrefixes are the variable name prefixes a gate's command never inherits.
var gateEnvDropPrefixes = []string{"TELEGRAM_"}

// GateRun is one gate run.
type GateRun struct {
	Dir      string        // the git work tree the command runs in
	Head     string        // the commit HEAD must be at (any revision git resolves, compared as full ids); required
	Argv     []string      // the command and its arguments (no shell)
	Timeout  time.Duration // the command's limit; 0 = DefaultGateTimeout
	LockPath string        // the box-wide lock; "" = GateLockPath()
	LockWait time.Duration // how long to wait for the lock; 0 = DefaultGateLockWait
	Env      []string      // the base environment; nil = os.Environ()
	Stdout   io.Writer     // the command's stdout and the exit= line; nil = os.Stdout
	Stderr   io.Writer     // the command's stderr and the runner's notes; nil = os.Stderr
}

// GateLockPath is the box-wide gate lock, <agent-jobs home>/gate.lock.
func GateLockPath() (string, error) {
	h, err := Home()
	if err != nil {
		return "", err
	}
	return filepath.Join(h, GateLockName), nil
}

// GateEnv is env without the variables a gate's command never inherits (gateEnvDropPrefixes).
func GateEnv(env []string) []string {
	out := make([]string, 0, len(env))
	for _, kv := range env {
		k, _, _ := strings.Cut(kv, "=")
		drop := false
		for _, p := range gateEnvDropPrefixes {
			if strings.HasPrefix(k, p) {
				drop = true
				break
			}
		}
		if !drop {
			out = append(out, kv)
		}
	}
	return out
}

// RunGate runs g and returns its exit code; the last line it writes to g.Stdout is always exit=<code>.
// ctx ending (the CLI's stop signals) ends the command's group like the timeout, with 128+SIGTERM.
func RunGate(ctx context.Context, g GateRun) (code int) {
	if g.Stdout == nil {
		g.Stdout = os.Stdout
	}
	if g.Stderr == nil {
		g.Stderr = os.Stderr
	}
	defer func() { fmt.Fprintf(g.Stdout, "exit=%d\n", code) }()
	fail := func(format string, a ...any) int {
		fmt.Fprintf(g.Stderr, "gate: "+format+"\n", a...)
		return GateExitRunner
	}
	if len(g.Argv) == 0 || g.Argv[0] == "" {
		return fail("no command given")
	}
	if strings.TrimSpace(g.Head) == "" {
		return fail("no --head given: a gate must name the commit it checks")
	}
	if strings.HasPrefix(g.Head, "-") {
		return fail("--head %q is not a revision", g.Head)
	}
	if g.Dir == "" {
		g.Dir = "."
	}
	if g.Timeout <= 0 {
		g.Timeout = DefaultGateTimeout
	}
	if g.LockWait <= 0 {
		g.LockWait = DefaultGateLockWait
	}
	if g.Env == nil {
		g.Env = os.Environ()
	}
	if g.LockPath == "" {
		p, err := GateLockPath()
		if err != nil {
			return fail("gate lock: %v", err)
		}
		g.LockPath = p
	}

	unlock, err := lockGate(ctx, g)
	if err != nil {
		if ctx.Err() != nil {
			fmt.Fprintf(g.Stderr, "gate: stopped while waiting for the gate lock %s\n", g.LockPath)
			return 128 + int(syscall.SIGTERM)
		}
		return fail("%v", err)
	}
	defer unlock()

	// HEAD is asserted under the lock: the tree may have moved while this gate waited.
	want, err := gateCommit(g.Dir, g.Head+"^{commit}")
	if err != nil {
		return fail("--head %s is not a commit in %s: %v", g.Head, g.Dir, err)
	}
	if code := assertGateHead(g, want, "before the run"); code != 0 {
		return code
	}
	writeGateHolder(g, want)
	fmt.Fprintf(g.Stderr, "gate: %s at %s in %s, timeout %s\n", strings.Join(g.Argv, " "), want, g.Dir, g.Timeout)

	start := time.Now()
	code = runGateCmd(ctx, g)
	fmt.Fprintf(g.Stderr, "gate: %s exit %d after %s\n", g.Argv[0], code, time.Since(start).Round(time.Millisecond))
	if moved := assertGateHead(g, want, "after the run"); moved != 0 && code == 0 {
		code = moved
	}
	return code
}

// gateCommit resolves rev in dir to a full commit id.
func gateCommit(dir, rev string) (string, error) {
	out, err := git(dir, "rev-parse", "--verify", "--quiet", rev)
	if err != nil {
		return "", err
	}
	id := strings.TrimSpace(out)
	if id == "" {
		return "", errors.New("does not resolve")
	}
	return id, nil
}

// assertGateHead returns 0 when HEAD of g.Dir is want, GateExitRunner (with a note) otherwise.
func assertGateHead(g GateRun, want, when string) int {
	head, err := gateCommit(g.Dir, "HEAD^{commit}")
	if err != nil {
		fmt.Fprintf(g.Stderr, "gate: HEAD of %s %s: %v\n", g.Dir, when, err)
		return GateExitRunner
	}
	if head != want {
		fmt.Fprintf(g.Stderr, "gate: HEAD of %s is %s %s, not the asserted %s (%s): the gate does not check that commit\n",
			g.Dir, head, when, want, g.Head)
		return GateExitRunner
	}
	return 0
}

// lockGate takes the gate lock exclusively, polling until g.LockWait or ctx ends, and returns the unlock.
func lockGate(ctx context.Context, g GateRun) (func(), error) {
	dir := filepath.Dir(g.LockPath)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("gate lock dir: %w", err)
	}
	f, err := os.OpenFile(g.LockPath, os.O_CREATE|os.O_RDWR|syscall.O_NOFOLLOW, 0o600)
	if err != nil {
		return nil, fmt.Errorf("open gate lock: %w", err)
	}
	deadline := time.Now().Add(g.LockWait)
	told := false
	for {
		err = syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
		if err == nil {
			break
		}
		if !errors.Is(err, syscall.EWOULDBLOCK) && !errors.Is(err, syscall.EINTR) {
			f.Close()
			return nil, fmt.Errorf("flock gate lock: %w", err)
		}
		if !told {
			fmt.Fprintf(g.Stderr, "gate: waiting up to %s for the gate lock %s, held by: %s\n", g.LockWait, g.LockPath,
				gateHolder(f))
			told = true
		}
		if time.Now().After(deadline) {
			holder := gateHolder(f)
			f.Close()
			return nil, fmt.Errorf("the gate lock %s is still held after %s (by: %s): another gate runs on this box", g.LockPath,
				g.LockWait, holder)
		}
		select {
		case <-ctx.Done():
			f.Close()
			return nil, ctx.Err()
		case <-time.After(gateLockPoll):
		}
	}
	return func() {
		_ = f.Truncate(0)
		_ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
		f.Close()
	}, nil
}

// gateHolder is what the lock's holder wrote into it (writeGateHolder), one line.
func gateHolder(f *os.File) string {
	b := make([]byte, gateHolderBytes)
	n, _ := f.ReadAt(b, 0)
	s := strings.TrimSpace(strings.ToValidUTF8(string(b[:n]), "?"))
	s = strings.Map(func(r rune) rune {
		if r < 0x20 || r == 0x7f {
			return ' '
		}
		return r
	}, s)
	if s == "" {
		return "unknown"
	}
	return s
}

// writeGateHolder records the holder in the lock file, for the waiters' notes.
func writeGateHolder(g GateRun, head string) {
	f, err := os.OpenFile(g.LockPath, os.O_WRONLY|syscall.O_NOFOLLOW, 0)
	if err != nil {
		return
	}
	defer f.Close()
	_ = f.Truncate(0)
	line := fmt.Sprintf("pid %d since %s at %s in %s: %s", os.Getpid(), time.Now().UTC().Format(time.RFC3339), head, g.Dir,
		strings.Join(g.Argv, " "))
	if len(line) > gateHolderBytes-1 {
		line = line[:gateHolderBytes-1]
	}
	_, _ = f.WriteString(line + "\n")
}

// runGateCmd runs g.Argv in g.Dir as the leader of a process group of its own, stdin /dev/null, with
// GateEnv(g.Env), and returns its exit code (see RunGate).
func runGateCmd(ctx context.Context, g GateRun) int {
	cmd := exec.Command(g.Argv[0], g.Argv[1:]...)
	cmd.Dir = g.Dir
	cmd.Env = GateEnv(g.Env)
	cmd.Stdout, cmd.Stderr = g.Stdout, g.Stderr
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	// A process that left the group (setsid) and still holds a pipe must not hold Wait forever.
	cmd.WaitDelay = gateKillGrace
	if err := cmd.Start(); err != nil {
		fmt.Fprintf(g.Stderr, "gate: start %s: %v\n", g.Argv[0], err)
		return GateExitNotStarted
	}
	pgid := cmd.Process.Pid // Setpgid: the leader's pid is the group id
	exited := make(chan struct{})
	go func() {
		// Wait for the leader's exit without reaping it: while it is a zombie its pid, the group id, stays
		// allocated, so the group kill below never reaches a group that took the id over.
		_ = waitExitNoReap(pgid)
		close(exited)
	}()
	endGroup := func() {
		_ = syscall.Kill(-pgid, syscall.SIGTERM)
		select {
		case <-exited:
		case <-time.After(gateKillGrace):
			_ = syscall.Kill(-pgid, syscall.SIGKILL)
			<-exited
		}
	}
	timer := time.NewTimer(g.Timeout)
	defer timer.Stop()
	result := 0
	select {
	case <-exited:
	case <-timer.C:
		fmt.Fprintf(g.Stderr, "gate: timeout after %s: ending process group %d\n", g.Timeout, pgid)
		endGroup()
		result = GateExitTimeout
	case <-ctx.Done():
		fmt.Fprintf(g.Stderr, "gate: stopped: ending process group %d\n", pgid)
		endGroup()
		result = 128 + int(syscall.SIGTERM)
	}
	// Nothing of the group outlives its leader (a suite's background servers, workers, watchers).
	_ = syscall.Kill(-pgid, syscall.SIGKILL)
	err := cmd.Wait()
	if result != 0 {
		return result
	}
	if err == nil || errors.Is(err, exec.ErrWaitDelay) {
		return 0
	}
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		if ws, ok := ee.Sys().(syscall.WaitStatus); ok && ws.Signaled() {
			return 128 + int(ws.Signal())
		}
		return ee.ExitCode()
	}
	fmt.Fprintf(g.Stderr, "gate: wait %s: %v\n", g.Argv[0], err)
	return GateExitRunner
}
