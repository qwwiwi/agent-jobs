package jobs

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"
)

// Serialized gate runner (#134): `agent-jobs gate run` runs one heavy local gate (check.sh, a pg suite,
// a vite build) for the orchestrator so that
//   - one gate runs at a time per repository, and at most gate_max_parallel (default 2) per box
//     (v0.8.2): a gate takes its repository's lock, one of the box slots and a shared flock on the
//     pre-0.8.2 single box lock (gateLockSteps), all fixed files next to GateLockPath(), shared by every
//     agent, user and state root of the box (never derived from AGENT_JOBS_HOME, HOME, TMPDIR or a
//     flag, so no two callers can lock different files); a waiter gives up after its lock wait instead
//     of hanging;
//   - it runs on the commit it claims to check: HEAD of the work tree must resolve to the asserted
//     commit once the lock is held, and still after the command (a checkout meanwhile fails the gate);
//   - it always ends with the line exit=N on stdout, whatever happened (a refusal, a missing command, a
//     timeout, a stop signal), on a line of its own also after output that does not end a line, so a
//     waiter never waits for a marker that never comes;
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
	DefaultGateTimeout  = DefaultGateTimeoutMinutes * time.Minute
	DefaultGateLockWait = 60 * time.Minute
	GateLockName        = "agent-jobs-gate.lock"
	gateKillGrace       = 5 * time.Second
	gateLockPoll        = 100 * time.Millisecond
	gateHolderBytes     = 512
	gateNotesBytes      = 64 << 10
	gateNotesWait       = 100 * time.Millisecond
)

const gateNotesTruncated = "\ngate: runner notes truncated\n"

// gateEnvDropPrefixes are the variable name prefixes a gate's command never inherits.
var gateEnvDropPrefixes = []string{"TELEGRAM_"}

// GateRun is one gate run.
type GateRun struct {
	Dir      string        // the git work tree the command runs in
	Head     string        // the commit HEAD must be at (any revision git resolves, compared as full ids); required
	Argv     []string      // the command and its arguments (no shell)
	Timeout  time.Duration // the command's limit; 0 = GateTimeoutEnvVar of Env, else DefaultGateTimeout
	LockPath string        // tests only: another lock file; "" (every real caller, the CLI has no flag) = GateLockPath()
	LockWait time.Duration // how long to wait for the lock; 0 = DefaultGateLockWait
	// MaxParallel is the box slots (gates of any repository at once); 0 = GateMaxParallelEnvVar of Env,
	// else DefaultGateMaxParallel.
	MaxParallel int
	Env         []string  // the base environment; nil = os.Environ()
	Stdout      io.Writer // the command's stdout and the exit= line; nil = os.Stdout
	Stderr      io.Writer // command stderr; bounded runner notes follow after unlock; nil = os.Stderr
}

// gateLockDirs are the candidate directories of the box-wide gate lock, the first that exists wins:
// fixed paths, world-writable and sticky, not under any state root and not from the environment
// (os.TempDir follows TMPDIR, which differs per job). /run/lock is outside a systemd PrivateTmp; /tmp is
// the fallback where it does not exist (macOS). Only the agentjobs_testhooks build changes it
// (gaterun_testhooks.go).
var gateLockDirs = []string{"/run/lock", "/tmp"}

// GateLockPath is the box-wide gate lock, <first of gateLockDirs>/agent-jobs-gate.lock: the same file for
// every caller on the box, whatever AGENT_JOBS_HOME or HOME says.
func GateLockPath() string {
	for _, d := range gateLockDirs[:len(gateLockDirs)-1] {
		if fi, err := os.Stat(d); err == nil && fi.IsDir() {
			return filepath.Join(d, GateLockName)
		}
	}
	return filepath.Join(gateLockDirs[len(gateLockDirs)-1], GateLockName)
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
	out := &gateStdout{w: g.Stdout}
	defer func() { out.marker(code) }()
	// Like the stdout exit marker, runner notes must not write to the caller while holding the lock.
	// Keep the command's stderr file direct: wrapping it would make cmd.Wait depend on a copy goroutine
	// that can block on an unread pipe. Only the runner's own notes use the bounded buffer.
	stderr := g.Stderr
	notes := &gateNotes{}
	g.Stderr = notes
	defer notes.flush(stderr)
	// A file (the CLI's stdout) goes to the command as is: os/exec then copies nothing, so a reader that
	// stops reading blocks only the command, which the timeout ends, never cmd.Wait and with it the lock.
	// What the command wrote last is unknown then, so the marker always gets a separator newline.
	_, direct := g.Stdout.(*os.File)
	if !direct {
		g.Stdout = out
	}
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
	if g.LockWait <= 0 {
		g.LockWait = DefaultGateLockWait
	}
	if g.Env == nil {
		g.Env = os.Environ()
	}
	if g.Timeout <= 0 {
		var note string
		if g.Timeout, note = gateTimeoutFromEnv(g.Env); note != "" {
			fmt.Fprintf(g.Stderr, "gate: %s\n", note)
		}
	}
	if g.MaxParallel <= 0 {
		var note string
		if g.MaxParallel, note = gateMaxParallelFromEnv(g.Env); note != "" {
			fmt.Fprintf(g.Stderr, "gate: %s\n", note)
		}
	}
	if g.LockPath == "" {
		g.LockPath = GateLockPath()
	}

	// Inside a job (#208) the holder line names the job (diagnostic only: the lock is not reentrant, a gate
	// of the same job waits like any other), and the wait is recorded in the job's folder for job_list,
	// status and the job's timeout.
	job := envValue(g.Env, JobEnvVar)
	lease, err := acquireGateLock(ctx, gateLockReq{path: g.LockPath, wait: g.LockWait, job: job,
		detail: gateHolderDetail(g, ""), notes: g.Stderr, waits: newGateWaitRecorder(gateJobDir(g.Env, job)),
		repo: gateRepoKey(g.Dir), slots: g.MaxParallel})
	if err != nil {
		if ctx.Err() != nil {
			fmt.Fprintf(g.Stderr, "gate: stopped while waiting for the gate lock %s\n", g.LockPath)
			return 128 + int(syscall.SIGTERM)
		}
		return fail("%v", err)
	}
	defer lease.release()

	// HEAD is asserted under the lock: the tree may have moved while this gate waited.
	want, err := gateCommit(g.Dir, g.Head+"^{commit}")
	if err != nil {
		return fail("--head %s is not a commit in %s: %v", g.Head, g.Dir, err)
	}
	if code := assertGateHead(g, want, "before the run"); code != 0 {
		return code
	}
	lease.writeHolder(gateHolderLine(job, gateHolderDetail(g, want)))
	fmt.Fprintf(g.Stderr, "gate: %s at %s in %s, timeout %s\n", strings.Join(g.Argv, " "), want, g.Dir, g.Timeout)

	start := time.Now()
	if direct {
		out.unknownEnd()
	}
	code = runGateCmd(ctx, g, stderr)
	fmt.Fprintf(g.Stderr, "gate: %s exit %d after %s\n", g.Argv[0], code, time.Since(start).Round(time.Millisecond))
	if moved := assertGateHead(g, want, "after the run"); moved != 0 && code == 0 {
		code = moved
	}
	return code
}

// gateNotes bounds the diagnostics retained while a gate holds or waits for the box lock.
type gateNotes struct {
	buf       []byte
	truncated bool
}

func (n *gateNotes) Write(p []byte) (int, error) {
	size := len(p)
	remaining := gateNotesBytes - len(gateNotesTruncated) - len(n.buf)
	if len(p) > remaining {
		p = p[:remaining]
		n.truncated = true
	}
	n.buf = append(n.buf, p...)
	return size, nil
}

// flush runs after unlock and waits only a bounded time for best-effort delivery. A caller-provided
// writer can block forever; at most one delivery goroutine and gateNotesBytes bytes remain per run
// until that writer returns. No note writer can delay the timeout kill, cmd.Wait, or unlocking.
func (n *gateNotes) flush(w io.Writer) {
	p := n.buf
	if n.truncated {
		p = append(p, gateNotesTruncated...)
	}
	if len(p) == 0 {
		return
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		_, _ = w.Write(p)
	}()
	timer := time.NewTimer(gateNotesWait)
	defer timer.Stop()
	select {
	case <-done:
	case <-timer.C:
	}
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

// gateHolderDetail is the holder line's detail after "pid N job J since T": the asserted commit (once
// known), the work tree and the command.
func gateHolderDetail(g GateRun, head string) string {
	at := ""
	if head != "" {
		at = " at " + head
	}
	return fmt.Sprintf("%s in %s: %s", at, g.Dir, strings.Join(g.Argv, " "))
}

// gateJobDir is the folder of the job a gate runs in ("" outside a job): JobDirEnvVar, when it names
// that job's folder (its base name is the job id of the marker).
func gateJobDir(env []string, job string) string {
	d := envValue(env, JobDirEnvVar)
	if job == "" || d == "" || !filepath.IsAbs(d) || filepath.Base(d) != job {
		return ""
	}
	return d
}

// openGateLock opens the gate lock read-write and creates it readable and writable for every user, as it
// serializes all of them from a shared sticky directory. An existing file is opened without O_CREAT
// (Linux's fs.protected_regular refuses O_CREAT on another user's file there); it must be a regular
// file, not a symlink or a fifo someone planted.
func openGateLock(path string) (*os.File, error) {
	for range 3 {
		f, err := os.OpenFile(path, os.O_RDWR|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
		if errors.Is(err, fs.ErrNotExist) {
			f, err = os.OpenFile(path, os.O_RDWR|os.O_CREATE|os.O_EXCL|syscall.O_NOFOLLOW, 0o666)
			if errors.Is(err, fs.ErrExist) {
				continue // another gate created it meanwhile
			}
			if err == nil {
				_ = f.Chmod(0o666) // past the umask
			}
		}
		if err != nil {
			return nil, err
		}
		fi, err := f.Stat()
		if err != nil {
			f.Close()
			return nil, err
		}
		if !fi.Mode().IsRegular() {
			f.Close()
			return nil, fmt.Errorf("%s is not a regular file", path)
		}
		return f, nil
	}
	return nil, fmt.Errorf("%s keeps appearing and disappearing", path)
}

// gateStdout carries the exit= marker and, when stdout is no file, the command's stdout; it remembers
// whether the last byte written ended a line, so the marker always starts a line of its own. A failed
// write (the reader went away) drops the rest instead of failing.
type gateStdout struct {
	mu      sync.Mutex
	w       io.Writer
	midLine bool
	broken  bool
}

func (s *gateStdout) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.broken || len(p) == 0 {
		return len(p), nil
	}
	n, err := s.w.Write(p)
	if n > 0 {
		s.midLine = p[n-1] != '\n'
	}
	if err != nil {
		s.broken = true
	}
	return len(p), nil
}

// unknownEnd records output written past s (the command's own fd): the marker then starts with a newline.
func (s *gateStdout) unknownEnd() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.midLine = true
}

// marker writes exit=<code> as the last line, after a newline when the output so far did not end one.
func (s *gateStdout) marker(code int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	nl := ""
	if s.midLine {
		nl = "\n"
	}
	fmt.Fprintf(s.w, "%sexit=%d\n", nl, code)
}

// gateHolder is what the lock's holder wrote into it (writeGateHolderLine): untrusted text (the lock file
// is writable by every user), so it is only ever shown, bounded, printable and redacted, never trusted.
func gateHolder(f *os.File) string {
	b := make([]byte, gateHolderBytes)
	n, _ := f.ReadAt(b, 0)
	s := RedactPrintable(strings.ToValidUTF8(string(b[:n]), "?"), gateHolderBytes)
	if s == "" {
		return "unknown"
	}
	return s
}

// runGateCmd runs g.Argv in g.Dir as the leader of a process group of its own, stdin /dev/null, with
// GateEnv(g.Env), and returns its exit code (see RunGate).
func runGateCmd(ctx context.Context, g GateRun, stderr io.Writer) int {
	cmd := exec.Command(g.Argv[0], g.Argv[1:]...)
	cmd.Dir = g.Dir
	cmd.Env = GateEnv(g.Env)
	cmd.Stdout, cmd.Stderr = g.Stdout, stderr
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
