package jobs

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
)

// gateFixture is a repository with two commits (first, second; HEAD at second) and a lock path of its own.
func gateFixture(t *testing.T) (repo, first, second, lock string) {
	t.Helper()
	repo = newRepo(t)
	first = strings.TrimSpace(gitT(t, repo, "rev-parse", "HEAD"))
	if err := os.WriteFile(filepath.Join(repo, "tracked.txt"), []byte("v2\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitT(t, repo, "commit", "-qam", "second")
	second = strings.TrimSpace(gitT(t, repo, "rev-parse", "HEAD"))
	return repo, first, second, filepath.Join(t.TempDir(), "gate.lock")
}

func runGateT(t *testing.T, g GateRun) (int, string, string) {
	t.Helper()
	var out, errb bytes.Buffer
	g.Stdout, g.Stderr = &out, &errb
	if g.Timeout == 0 {
		g.Timeout = time.Minute
	}
	if g.Env == nil {
		g.Env = os.Environ()
	}
	code := RunGate(context.Background(), g)
	return code, out.String(), errb.String()
}

// lastLine is the last non-empty line of s.
func lastLine(s string) string {
	lines := strings.Split(strings.TrimRight(s, "\n"), "\n")
	return lines[len(lines)-1]
}

// gateProcAlive reports a process that exists and is no zombie.
func gateProcAlive(pid int) bool {
	b, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/stat")
	if err != nil {
		return syscall.Kill(pid, 0) == nil
	}
	s := string(b)
	if i := strings.LastIndexByte(s, ')'); i >= 0 && i+2 < len(s) {
		return s[i+2] != 'Z'
	}
	return true
}

func readPID(t *testing.T, path string) int {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(b)))
	if err != nil {
		t.Fatal(err)
	}
	return pid
}

func waitDead(t *testing.T, pid int) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for gateProcAlive(pid) {
		if time.Now().After(deadline) {
			t.Fatalf("process %d of the gate's group is still alive", pid)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// A gate whose work tree is not at the asserted commit never runs and still prints exit=.
func TestRunGateRefusesWrongHead(t *testing.T) {
	repo, first, second, lock := gateFixture(t)
	ran := filepath.Join(t.TempDir(), "ran")
	code, out, errs := runGateT(t, GateRun{Dir: repo, Head: first, LockPath: lock,
		Argv: []string{"/bin/sh", "-c", "touch " + ran}})
	if code != GateExitRunner || lastLine(out) != "exit=125" {
		t.Fatalf("code %d, out %q", code, out)
	}
	if !strings.Contains(errs, "HEAD") || !strings.Contains(errs, second) {
		t.Fatalf("stderr does not name the HEAD mismatch: %q", errs)
	}
	if _, err := os.Stat(ran); err == nil {
		t.Fatal("the command ran on the wrong HEAD")
	}
	// No asserted head at all is refused too: a gate always says which commit it checks.
	if code, out, _ := runGateT(t, GateRun{Dir: repo, LockPath: lock, Argv: []string{"true"}}); code != GateExitRunner ||
		lastLine(out) != "exit=125" {
		t.Fatalf("no head: code %d, out %q", code, out)
	}
	// An abbreviated id of the right commit passes.
	if code, out, _ := runGateT(t, GateRun{Dir: repo, Head: second[:12], LockPath: lock, Argv: []string{"true"}}); code != 0 ||
		lastLine(out) != "exit=0" {
		t.Fatalf("right head: code %d, out %q", code, out)
	}
}

// The command's exit code is returned and printed as the last line, after the command's own output.
func TestRunGatePrintsExitMarker(t *testing.T) {
	repo, _, head, lock := gateFixture(t)
	code, out, _ := runGateT(t, GateRun{Dir: repo, Head: head, LockPath: lock,
		Argv: []string{"/bin/sh", "-c", "echo hello; exit 3"}})
	if code != 3 || out != "hello\nexit=3\n" {
		t.Fatalf("code %d, out %q", code, out)
	}
	// A command that cannot start still ends with the marker.
	code, out, _ = runGateT(t, GateRun{Dir: repo, Head: head, LockPath: lock, Argv: []string{"/nonexistent/gate-cmd"}})
	if code != 127 || lastLine(out) != "exit=127" {
		t.Fatalf("missing command: code %d, out %q", code, out)
	}
	// A gate stopped from outside (ctx) ends its group and prints the marker too.
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	var o, e bytes.Buffer
	code = RunGate(ctx, GateRun{Dir: repo, Head: head, LockPath: lock, Timeout: time.Minute, Env: os.Environ(),
		Argv: []string{"sleep", "30"}, Stdout: &o, Stderr: &e})
	if code != 128+int(syscall.SIGTERM) || lastLine(o.String()) != "exit=143" {
		t.Fatalf("stopped: code %d, out %q, err %q", code, o.String(), e.String())
	}
}

// On the timeout the whole process group is killed (a child that ignores SIGTERM too), not just the
// leader; exit=124.
func TestRunGateTimeoutKillsProcessGroup(t *testing.T) {
	repo, _, head, lock := gateFixture(t)
	pidf := filepath.Join(t.TempDir(), "child.pid")
	start := time.Now()
	code, out, errs := runGateT(t, GateRun{Dir: repo, Head: head, LockPath: lock, Timeout: 500 * time.Millisecond,
		Argv: []string{"/bin/sh", "-c", "(trap '' TERM; exec sleep 60) & echo $! > " + pidf + "; wait"}})
	if code != GateExitTimeout || lastLine(out) != "exit=124" {
		t.Fatalf("code %d, out %q, err %q", code, out, errs)
	}
	if !strings.Contains(errs, "timeout") {
		t.Fatalf("stderr does not say timeout: %q", errs)
	}
	if d := time.Since(start); d > 20*time.Second {
		t.Fatalf("the gate took %s past a 500ms timeout", d)
	}
	waitDead(t, readPID(t, pidf))
}

// A suite that leaves a background process behind does not leave an orphan: the group is ended when the
// leader exits.
func TestRunGateKillsLeftoversAfterExit(t *testing.T) {
	repo, _, head, lock := gateFixture(t)
	pidf := filepath.Join(t.TempDir(), "orphan.pid")
	code, out, _ := runGateT(t, GateRun{Dir: repo, Head: head, LockPath: lock,
		Argv: []string{"/bin/sh", "-c", "sleep 60 >/dev/null 2>&1 & echo $! > " + pidf + "; exit 0"}})
	if code != 0 || lastLine(out) != "exit=0" {
		t.Fatalf("code %d, out %q", code, out)
	}
	waitDead(t, readPID(t, pidf))
}

// TELEGRAM_* never reaches the gate's command; the rest of the environment does.
func TestRunGateDropsTelegramEnv(t *testing.T) {
	repo, _, head, lock := gateFixture(t)
	env := append(os.Environ(), "TELEGRAM_BOT_TOKEN=secret-value", "TELEGRAM_CHAT_ID=1", "GATE_KEEP=yes")
	code, out, _ := runGateT(t, GateRun{Dir: repo, Head: head, LockPath: lock, Env: env, Argv: []string{"env"}})
	if code != 0 {
		t.Fatalf("code %d, out %q", code, out)
	}
	if strings.Contains(out, "TELEGRAM_") || strings.Contains(out, "secret-value") {
		t.Fatalf("TELEGRAM_* leaked into the gate: %q", out)
	}
	if !strings.Contains(out, "GATE_KEEP=yes") || !strings.Contains(out, "PATH=") {
		t.Fatalf("the rest of the environment is missing: %q", out)
	}
}

// Two gates on one box never overlap; a gate that cannot get the lock in time gives up with exit=125
// instead of hanging.
func TestRunGateSerialized(t *testing.T) {
	repo, _, head, lock := gateFixture(t)
	busy := filepath.Join(t.TempDir(), "busy")
	// mkdir is atomic: it fails when another gate's command is inside.
	script := "mkdir " + busy + " || exit 9; sleep 0.3; rmdir " + busy
	var wg sync.WaitGroup
	codes := make([]int, 3)
	for i := range codes {
		wg.Add(1)
		go func() {
			defer wg.Done()
			var o, e bytes.Buffer
			codes[i] = RunGate(context.Background(), GateRun{Dir: repo, Head: head, LockPath: lock, Timeout: time.Minute,
				LockWait: time.Minute, Env: os.Environ(), Argv: []string{"/bin/sh", "-c", script}, Stdout: &o, Stderr: &e})
		}()
	}
	wg.Wait()
	for i, c := range codes {
		if c != 0 {
			t.Fatalf("gate %d exited %d: two gates ran at once", i, c)
		}
	}

	// A held lock and a short wait: the gate gives up, names the holder and still prints the marker.
	f, err := os.OpenFile(lock, os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX); err != nil {
		t.Fatal(err)
	}
	ran := filepath.Join(t.TempDir(), "ran")
	code, out, errs := runGateT(t, GateRun{Dir: repo, Head: head, LockPath: lock, LockWait: 300 * time.Millisecond,
		Argv: []string{"/bin/sh", "-c", "touch " + ran}})
	if code != GateExitRunner || lastLine(out) != "exit=125" || !strings.Contains(errs, "lock") {
		t.Fatalf("held lock: code %d, out %q, err %q", code, out, errs)
	}
	if _, err := os.Stat(ran); err == nil {
		t.Fatal("the command ran without the lock")
	}
}

// HEAD moved while the gate ran (a checkout in the same tree): the result is not about the asserted
// commit, so the gate fails even though the command passed.
func TestRunGateHeadMovedDuringRun(t *testing.T) {
	repo, first, head, lock := gateFixture(t)
	code, out, errs := runGateT(t, GateRun{Dir: repo, Head: head, LockPath: lock,
		Argv: []string{"git", "checkout", "-q", first}})
	if code != GateExitRunner || lastLine(out) != "exit=125" || !strings.Contains(errs, "HEAD") {
		t.Fatalf("code %d, out %q, err %q", code, out, errs)
	}
}

// A command whose output does not end a line still leaves exit=N on a line of its own (#134 fix loop).
func TestRunGateExitMarkerAfterUnterminatedOutput(t *testing.T) {
	repo, _, head, lock := gateFixture(t)
	code, out, _ := runGateT(t, GateRun{Dir: repo, Head: head, LockPath: lock, Argv: []string{"printf", "hello"}})
	if code != 0 || out != "hello\nexit=0\n" {
		t.Fatalf("unterminated: code %d, out %q", code, out)
	}
	// Terminated output gets no extra blank line.
	code, out, _ = runGateT(t, GateRun{Dir: repo, Head: head, LockPath: lock, Argv: []string{"printf", `a\n`}})
	if code != 0 || out != "a\nexit=0\n" {
		t.Fatalf("terminated: code %d, out %q", code, out)
	}
	// A file as stdout (the CLI) gets the command's output directly: the marker always follows a newline.
	for _, c := range []struct{ arg, want string }{{"hello", "hello\nexit=0\n"}, {`a\n`, "a\n\nexit=0\n"}} {
		f, err := os.Create(filepath.Join(t.TempDir(), "stdout"))
		if err != nil {
			t.Fatal(err)
		}
		var e bytes.Buffer
		code := RunGate(context.Background(), GateRun{Dir: repo, Head: head, LockPath: lock, Timeout: time.Minute,
			Env: os.Environ(), Argv: []string{"printf", c.arg}, Stdout: f, Stderr: &e})
		f.Close()
		b, err := os.ReadFile(f.Name())
		if err != nil {
			t.Fatal(err)
		}
		if code != 0 || string(b) != c.want {
			t.Fatalf("file stdout %q: code %d, out %q, want %q", c.arg, code, b, c.want)
		}
	}
}

// The default gate lock is one file per box: it does not follow the agent-jobs state root (#134 fix loop).
func TestGateLockPathIgnoresStateRoot(t *testing.T) {
	a, b := t.TempDir(), t.TempDir()
	t.Setenv("AGENT_JOBS_HOME", a)
	t.Setenv("TMPDIR", a)
	pa := GateLockPath()
	t.Setenv("AGENT_JOBS_HOME", b)
	t.Setenv("HOME", b)
	t.Setenv("TMPDIR", b)
	pb := GateLockPath()
	if pa != pb || strings.HasPrefix(pa, a) || strings.HasPrefix(pb, b) {
		t.Fatalf("the gate lock follows the state root: %q under %q, %q under %q", pa, a, pb, b)
	}
}

// Two `agent-jobs gate run` processes with different state roots (AGENT_JOBS_HOME) still share the one
// box lock, so their commands never overlap (#134 fix loop). The binary under test is the
// agentjobs_testhooks build: AGENT_JOBS_TEST_GATE_LOCK_DIR moves the box lock into this test's temp dir,
// so the test never waits for a real gate on the box (go test itself may run under one).
func TestGateRunSerializedAcrossStateRoots(t *testing.T) {
	repo, _, head, _ := gateFixture(t)
	lockDir := t.TempDir()
	busy := filepath.Join(t.TempDir(), "busy")
	// mkdir is atomic: it fails when the other gate's command is inside.
	script := "mkdir " + busy + " || exit 9; sleep 0.5; rmdir " + busy
	type result struct {
		code int
		out  string
		err  error
	}
	results := make([]result, 2)
	var wg sync.WaitGroup
	for i := range results {
		home := t.TempDir()
		cmd := exec.Command(selfBin, "gate", "run", "--repo", repo, "--head", head, "--lock-wait", "1m", "--",
			"/bin/sh", "-c", script)
		cmd.Env = append(os.Environ(), "AGENT_JOBS_HOME="+home, "AGENT_JOBS_TEST_GATE_LOCK_DIR="+lockDir)
		var o bytes.Buffer
		cmd.Stdout = &o
		wg.Add(1)
		go func() {
			defer wg.Done()
			err := cmd.Run()
			code := 0
			var ee *exec.ExitError
			if errors.As(err, &ee) {
				code, err = ee.ExitCode(), nil
			}
			results[i] = result{code, o.String(), err}
		}()
	}
	wg.Wait()
	for i, r := range results {
		if r.err != nil || r.code != 0 || lastLine(r.out) != "exit=0" {
			t.Fatalf("gate %d: code %d, out %q, err %v: the gates of two state roots ran at once", i, r.code, r.out, r.err)
		}
	}
}

// The box lock is created readable and writable for every user (past the umask), reopened without
// O_CREAT, and a symlink or fifo planted in its place is refused (#134 fix loop).
func TestOpenGateLockShared(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, GateLockName)
	old := syscall.Umask(0o077)
	f, err := openGateLock(path)
	syscall.Umask(old)
	if err != nil {
		t.Fatal(err)
	}
	f.Close()
	fi, err := os.Stat(path)
	if err != nil || fi.Mode().Perm() != 0o666 {
		t.Fatalf("lock mode %v, err %v; want 0666", fi.Mode().Perm(), err)
	}
	if f, err = openGateLock(path); err != nil {
		t.Fatalf("reopen: %v", err)
	}
	f.Close()
	link := filepath.Join(dir, "link.lock")
	if err := os.Symlink(path, link); err != nil {
		t.Fatal(err)
	}
	if f, err := openGateLock(link); err == nil {
		f.Close()
		t.Fatal("a symlinked lock was opened")
	}
	fifo := filepath.Join(dir, "fifo.lock")
	if err := syscall.Mkfifo(fifo, 0o666); err != nil {
		t.Fatal(err)
	}
	if f, err := openGateLock(fifo); err == nil {
		f.Close()
		t.Fatal("a fifo lock was opened")
	}
}

// A caller that keeps its stdout pipe open but stops reading never keeps the box lock: the command
// blocks on the full pipe, the timeout ends it and the lock is free for the next gate before anyone
// reads the first gate's output again (#134 fix loop 2).
func TestRunGateUnreadStdoutReleasesLock(t *testing.T) {
	t.Run("plain", func(t *testing.T) { unreadStdoutReleasesLock(t, os.Environ(), "") })
	// Inside a job the output also feeds the artifact's tail: that copy must not hold the lock either.
	t.Run("in job", func(t *testing.T) {
		jobDir, env := gateJobFixture(t)
		unreadStdoutReleasesLock(t, env, jobDir)
	})
}

func unreadStdoutReleasesLock(t *testing.T, env []string, jobDir string) {
	repo, _, head, lock := gateFixture(t)
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	first := make(chan int, 1)
	var firstErr lockedBuffer
	go func() {
		defer w.Close()
		// Far more than a pipe holds (64 KiB on Linux), then it would end on its own.
		first <- RunGate(context.Background(), GateRun{Dir: repo, Head: head, LockPath: lock, Timeout: 500 * time.Millisecond,
			Env: env, Argv: []string{"/bin/sh", "-c", "head -c 1048576 /dev/zero; sleep 0.2"}, Stdout: w, Stderr: &firstErr})
	}()
	// Wait until the first gate holds the lock (it wrote the holder line into its repository's lock).
	repoLock := gateRepoLockIn(filepath.Dir(lock), gateRepoKey(repo))
	deadline := time.Now().Add(30 * time.Second)
	for {
		if b, _ := os.ReadFile(repoLock); len(b) > 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("the first gate never took the lock: %q", firstErr.String())
		}
		time.Sleep(20 * time.Millisecond)
	}
	// Inside a job the output goes to a spool file, so the command never blocks on the unread pipe: it ends
	// on its own, and its record (the tail read from the spool) exists while the reader is still away.
	if jobDir != "" {
		if runs := waitGateRecorded(t, jobDir, 1); runs[0].Exit != 0 {
			t.Fatalf("the run is not recorded complete while the reader is away: %+v", runs[0])
		}
	}
	// The first gate's stdout stays unread meanwhile.
	code, out, errs := runGateT(t, GateRun{Dir: repo, Head: head, LockPath: lock, LockWait: 15 * time.Second,
		Argv: []string{"true"}})
	// Resume the first gate's reader in any case, so its goroutine ends.
	got := make(chan string, 1)
	go func() {
		b, _ := io.ReadAll(r)
		got <- string(b)
	}()
	if code != 0 || lastLine(out) != "exit=0" {
		t.Fatalf("second gate behind an unread first gate: code %d, out %q, err %q (first: %q)", code, out, errs,
			firstErr.String())
	}
	// Plain: the command blocked on the full pipe and the timeout ended it. In a job it never blocked.
	wantCode := GateExitTimeout
	if jobDir != "" {
		wantCode = 0
	}
	select {
	case c := <-first:
		if c != wantCode {
			t.Fatalf("first gate exited %d, want %d: %q", c, wantCode, firstErr.String())
		}
	case <-time.After(30 * time.Second):
		t.Fatal("the first gate did not end after its reader resumed")
	}
	o := <-got
	if !strings.HasSuffix(o, "\nexit="+strconv.Itoa(wantCode)+"\n") {
		t.Fatalf("first gate's output does not end with the marker line: ...%q", o[max(0, len(o)-40):])
	}
	if jobDir != "" {
		// A live reader that comes back gets every byte of the output, then the marker.
		if len(o) != 1<<20+len("\nexit=0\n") {
			t.Fatalf("the reader received %d bytes, want the full 1 MiB and the marker", len(o))
		}
		if f := readGateResultsT(t, jobDir); len(f.Runs) != 1 || f.Runs[0].Exit != 0 {
			t.Fatalf("the run is not recorded: %+v", f)
		}
	}
}

// Neither startup notes nor the note before the timeout kill may block on unread stderr (#178).
func TestRunGateUnreadStderrReleasesLock(t *testing.T) {
	t.Run("plain", func(t *testing.T) { unreadStderrReleasesLock(t, func(*testing.T) []string { return nil }) })
	t.Run("in job", func(t *testing.T) {
		unreadStderrReleasesLock(t, func(t *testing.T) []string { _, env := gateJobFixture(t); return env })
	})
}

func unreadStderrReleasesLock(t *testing.T, newEnv func(*testing.T) []string) {
	for _, prefill := range []bool{true, false} {
		name := "timeout"
		if prefill {
			name = "startup"
		}
		t.Run(name, func(t *testing.T) {
			repo, _, head, lock := gateFixture(t)
			r, w, err := os.Pipe()
			if err != nil {
				t.Fatal(err)
			}
			defer w.Close()
			defer r.Close() // Also releases a blocked note writer on regression failures.
			if prefill {
				if err := w.SetWriteDeadline(time.Now().Add(50 * time.Millisecond)); err != nil {
					t.Fatal(err)
				}
				if _, err := w.Write(make([]byte, 1<<20)); !errors.Is(err, os.ErrDeadlineExceeded) {
					t.Fatalf("fill stderr pipe: %v, want deadline exceeded", err)
				}
				if err := w.SetWriteDeadline(time.Time{}); err != nil {
					t.Fatal(err)
				}
			}
			pidf := filepath.Join(t.TempDir(), "leader.pid")
			var out bytes.Buffer
			done := make(chan int, 1)
			returned := false
			env := newEnv(t)
			inJob := envValue(env, JobEnvVar) != ""
			go func() {
				done <- RunGate(context.Background(), GateRun{Dir: repo, Head: head, LockPath: lock,
					Timeout: 500 * time.Millisecond, Stdout: &out, Stderr: w, Env: env,
					Argv: []string{"/bin/sh", "-c", "echo $$ > " + pidf + "; head -c 1048576 /dev/zero >&2; sleep 30"}})
			}()
			t.Cleanup(func() {
				_ = r.Close()
				if !returned {
					select {
					case <-done:
					case <-time.After(3 * time.Second):
						t.Error("runner did not finish after closing its stderr reader")
					}
				}
			})
			// Stderr stays open and completely unread through the timeout and lock handoff.
			// The command is running (it wrote its pid) before anything else is judged.
			for deadline := time.Now().Add(30 * time.Second); ; time.Sleep(20 * time.Millisecond) {
				if b, _ := os.ReadFile(pidf); strings.HasSuffix(string(b), "\n") {
					break
				}
				if time.Now().After(deadline) {
					t.Fatal("the command never started")
				}
			}
			if !inJob {
				select {
				case code := <-done:
					returned = true
					if code != GateExitTimeout || lastLine(out.String()) != "exit=124" {
						t.Fatalf("code %d, stdout %q", code, out.String())
					}
				case <-time.After(3 * time.Second):
					t.Error("runner blocked on unread stderr past its timeout")
				}
			}
			code, out2, errs := runGateT(t, GateRun{Dir: repo, Head: head, LockPath: lock,
				LockWait: 5 * time.Second, Argv: []string{"true"}})
			if code != 0 || lastLine(out2) != "exit=0" {
				t.Fatalf("gate lock not released: code %d, stdout %q, stderr %q", code, out2, errs)
			}
			waitDead(t, readPID(t, pidf))
			if inJob {
				// The forwarder of the unread stderr keeps the gate process (lease long released, record
				// written) until the reader reads; then the marker follows and the gate ends.
				go func() { _, _ = io.Copy(io.Discard, r) }()
				select {
				case code := <-done:
					returned = true
					if code != GateExitTimeout || lastLine(out.String()) != "exit=124" {
						t.Fatalf("code %d, stdout %q", code, out.String())
					}
				case <-time.After(30 * time.Second):
					t.Fatal("the gate did not end after its stderr reader resumed")
				}
			}
		})
	}
}

func TestRunGateNotesAreBounded(t *testing.T) {
	repo, _, head, lock := gateFixture(t)
	code, out, errs := runGateT(t, GateRun{Dir: repo, Head: head, LockPath: lock,
		Argv: []string{"/" + strings.Repeat("x", 128<<10)}})
	if code != GateExitNotStarted || lastLine(out) != "exit=127" {
		t.Fatalf("code %d, stdout %q", code, out)
	}
	if len(errs) > 64<<10 || !strings.HasSuffix(errs, "\ngate: runner notes truncated\n") {
		t.Fatalf("runner notes: %d bytes, truncation marker present: %t", len(errs),
			strings.HasSuffix(errs, "\ngate: runner notes truncated\n"))
	}
}

// gateParallelT runs one gate per entry of dirs at once, all on the lock directory of lock, with
// maxParallel slots; each command records how many gate commands were inside at its start (the log
// file), stays inside for hold and exits 0. It returns the exit codes and the largest count seen.
func gateParallelT(t *testing.T, lock string, maxParallel int, hold time.Duration, dirs ...string) ([]int, int) {
	t.Helper()
	inside := t.TempDir()
	logf := filepath.Join(t.TempDir(), "counts")
	codes := make([]int, len(dirs))
	var wg sync.WaitGroup
	for i, dir := range dirs {
		head := strings.TrimSpace(gitT(t, dir, "rev-parse", "HEAD"))
		me := filepath.Join(inside, strconv.Itoa(i))
		script := "mkdir " + me + "; ls " + inside + " | wc -l >> " + logf +
			"; sleep " + strconv.FormatFloat(hold.Seconds(), 'f', 2, 64) + "; rmdir " + me
		wg.Add(1)
		go func() {
			defer wg.Done()
			var o, e bytes.Buffer
			codes[i] = RunGate(context.Background(), GateRun{Dir: dir, Head: head, LockPath: lock, Timeout: time.Minute,
				LockWait: time.Minute, MaxParallel: maxParallel, Env: os.Environ(), Argv: []string{"/bin/sh", "-c", script},
				Stdout: &o, Stderr: &e})
			if codes[i] != 0 {
				t.Logf("gate %d: exit %d, stderr %s", i, codes[i], e.String())
			}
		}()
	}
	wg.Wait()
	most := 0
	for _, f := range strings.Fields(readFileT(t, logf)) {
		if n, err := strconv.Atoi(f); err == nil && n > most {
			most = n
		}
	}
	return codes, most
}

// G01: gates in two different repositories run at once (the gate lock is per repository); the box
// semaphore still caps them.
func TestRunGateTwoReposOverlap(t *testing.T) {
	lock := filepath.Join(t.TempDir(), GateLockName)
	codes, most := gateParallelT(t, lock, 2, time.Second, newRepo(t), newRepo(t))
	if codes[0] != 0 || codes[1] != 0 || most != 2 {
		t.Fatalf("two repos, two slots: codes %v, at most %d inside at once; want both inside together", codes, most)
	}
}

// G01: two gates of one repository serialize, also from two work trees of it (a job's worktree and the
// main checkout share the repository's databases, ports and caches).
func TestRunGateSameRepoSerializes(t *testing.T) {
	lock := filepath.Join(t.TempDir(), GateLockName)
	repo := newRepo(t)
	wt := filepath.Join(t.TempDir(), "wt")
	gitT(t, repo, "worktree", "add", "-q", "--detach", wt, "HEAD")
	codes, most := gateParallelT(t, lock, 2, 300*time.Millisecond, repo, wt, repo)
	if codes[0] != 0 || codes[1] != 0 || codes[2] != 0 || most != 1 {
		t.Fatalf("one repository: codes %v, at most %d inside at once; want one at a time", codes, most)
	}
}

// G01: with N slots a gate of a third repository waits while two run; one slot serializes every gate.
func TestRunGateSemaphoreCap(t *testing.T) {
	lock := filepath.Join(t.TempDir(), GateLockName)
	codes, most := gateParallelT(t, lock, 2, 700*time.Millisecond, newRepo(t), newRepo(t), newRepo(t))
	for i, c := range codes {
		if c != 0 {
			t.Fatalf("gate %d exited %d", i, c)
		}
	}
	if most != 2 {
		t.Fatalf("three repos, two slots: at most %d inside at once; want exactly 2", most)
	}
	lock = filepath.Join(t.TempDir(), GateLockName)
	if _, most := gateParallelT(t, lock, 1, 300*time.Millisecond, newRepo(t), newRepo(t)); most != 1 {
		t.Fatalf("two repos, one slot: %d inside at once", most)
	}
}

// G03: an old binary holds the single box lock exclusively: a new gate (any repository) waits for it
// and gives up within its bound without running its command; while new gates run, the old binary's
// exclusive lock is refused, so the two never run gates together during an upgrade.
func TestRunGateLegacyHolderBlocks(t *testing.T) {
	repo, _, head, lock := gateFixture(t)
	old := holdGateT(t, lock) // what a v0.8.0 gate takes: an exclusive flock on the one box lock
	writeGateHolderLine(lock, gateHolderLine("", ": old binary check.sh"))
	ran := filepath.Join(t.TempDir(), "ran")
	code, out, errs := runGateT(t, GateRun{Dir: repo, Head: head, LockPath: lock, LockWait: 300 * time.Millisecond,
		Argv: []string{"touch", ran}})
	if code != GateExitRunner || lastLine(out) != "exit=125" || !strings.Contains(errs, "still held after") ||
		!strings.Contains(errs, "old binary check.sh") {
		t.Fatalf("legacy holder: code %d, out %q, err %q", code, out, errs)
	}
	if _, err := os.Stat(ran); err == nil {
		t.Fatal("the command ran while an old binary held the box lock")
	}
	old.release()

	// A new gate runs: the legacy lock cannot be taken exclusively until it ends.
	started := filepath.Join(t.TempDir(), "started")
	done := make(chan int, 1)
	go func() {
		code, _, _ := runGateT(t, GateRun{Dir: repo, Head: head, LockPath: lock,
			Argv: []string{"/bin/sh", "-c", "touch " + started + "; sleep 1"}})
		done <- code
	}()
	deadline := time.Now().Add(5 * time.Second)
	for {
		if _, err := os.Stat(started); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the new gate did not start")
		}
		time.Sleep(10 * time.Millisecond)
	}
	f, err := os.OpenFile(lock, os.O_RDWR, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); !errors.Is(err, syscall.EWOULDBLOCK) {
		t.Fatalf("an old binary's exclusive lock while a new gate runs: %v; want EWOULDBLOCK", err)
	}
	if code := <-done; code != 0 {
		t.Fatalf("new gate exited %d", code)
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		t.Fatalf("the legacy lock after the new gate ended: %v", err)
	}
}

// G02: a gate without an explicit timeout takes the job's gate timeout from the environment
// (AGENT_JOBS_GATE_TIMEOUT, set by the wrapper from the card or config); the default is 60 minutes.
func TestRunGateTimeoutFromEnv(t *testing.T) {
	if DefaultGateTimeout != 60*time.Minute {
		t.Fatalf("DefaultGateTimeout = %s, want 60m", DefaultGateTimeout)
	}
	repo, _, head, lock := gateFixture(t)
	var o, e bytes.Buffer
	start := time.Now()
	code := RunGate(context.Background(), GateRun{Dir: repo, Head: head, LockPath: lock,
		Env: append(os.Environ(), GateTimeoutEnvVar+"=400ms"), Argv: []string{"sleep", "5"}, Stdout: &o, Stderr: &e})
	if code != GateExitTimeout || time.Since(start) > 4*time.Second || !strings.Contains(e.String(), "timeout 400ms") {
		t.Fatalf("env timeout: code %d after %s, err %q", code, time.Since(start), e.String())
	}
	// An explicit timeout wins over the environment.
	o.Reset()
	e.Reset()
	code = RunGate(context.Background(), GateRun{Dir: repo, Head: head, LockPath: lock, Timeout: time.Minute,
		Env: append(os.Environ(), GateTimeoutEnvVar+"=100ms"), Argv: []string{"sleep", "0.5"}, Stdout: &o, Stderr: &e})
	if code != 0 {
		t.Fatalf("explicit timeout: code %d, err %q", code, e.String())
	}
	// An unusable value falls back to the default with a note.
	o.Reset()
	e.Reset()
	code = RunGate(context.Background(), GateRun{Dir: repo, Head: head, LockPath: lock,
		Env: append(os.Environ(), GateTimeoutEnvVar+"=soon"), Argv: []string{"true"}, Stdout: &o, Stderr: &e})
	if code != 0 || !strings.Contains(e.String(), "timeout 1h0m0s") || !strings.Contains(e.String(), GateTimeoutEnvVar) {
		t.Fatalf("bad env timeout: code %d, err %q", code, e.String())
	}
}

// G01: the box cap comes from the environment (AGENT_JOBS_GATE_MAX_PARALLEL, from gate_max_parallel)
// when the caller sets none: 1 serializes two repositories.
func TestRunGateMaxParallelFromEnv(t *testing.T) {
	t.Setenv(GateMaxParallelEnvVar, "1")
	lock := filepath.Join(t.TempDir(), GateLockName)
	if _, most := gateParallelT(t, lock, 0, 300*time.Millisecond, newRepo(t), newRepo(t)); most != 1 {
		t.Fatalf("env max parallel 1: %d inside at once", most)
	}
}

// G03: an old binary waiting for the single box lock (queued in its FIFO) is not starved by new gates:
// a new gate that comes after it waits until it has run, though a slot is free.
func TestRunGateYieldsToLegacyWaiter(t *testing.T) {
	lock := filepath.Join(t.TempDir(), GateLockName)
	repoA, repoB := newRepo(t), newRepo(t)
	headA := strings.TrimSpace(gitT(t, repoA, "rev-parse", "HEAD"))
	headB := strings.TrimSpace(gitT(t, repoB, "rev-parse", "HEAD"))
	started := filepath.Join(t.TempDir(), "a-started")
	doneA := make(chan int, 1)
	go func() {
		code, _, _ := runGateT(t, GateRun{Dir: repoA, Head: headA, LockPath: lock, MaxParallel: 2,
			Argv: []string{"/bin/sh", "-c", "touch " + started + "; sleep 1"}})
		doneA <- code
	}()
	waitFileT(t, started)
	// The old binary: an exclusive flock on the single box lock, queued behind the running new gate.
	var oldAt time.Time
	doneOld := make(chan error, 1)
	go func() {
		l, err := acquireGateLock(context.Background(), gateLockReq{path: lock, wait: 20 * time.Second, detail: ": old binary"})
		if err == nil {
			oldAt = time.Now()
			time.Sleep(300 * time.Millisecond)
			l.release()
		}
		doneOld <- err
	}()
	deadline := time.Now().Add(5 * time.Second)
	for !strings.Contains(readFileOrEmpty(lock+gateQueueSuffix), " ") {
		if time.Now().After(deadline) {
			t.Fatal("the old binary never queued for the box lock")
		}
		time.Sleep(10 * time.Millisecond)
	}
	// A new gate of another repository arrives while the old binary waits.
	var bAt time.Time
	code, _, errs := runGateT(t, GateRun{Dir: repoB, Head: headB, LockPath: lock, MaxParallel: 2, LockWait: 20 * time.Second,
		Argv: []string{"true"}})
	bAt = time.Now()
	if code != 0 {
		t.Fatalf("new gate B: %d %s", code, errs)
	}
	if err := <-doneOld; err != nil {
		t.Fatalf("old binary: %v", err)
	}
	if c := <-doneA; c != 0 {
		t.Fatalf("new gate A: %d", c)
	}
	if oldAt.IsZero() || bAt.Before(oldAt) {
		t.Fatalf("the new gate B (done %s) overtook the queued old binary (got the lock %s)", bAt.Format(time.StampMilli),
			oldAt.Format(time.StampMilli))
	}
}

func waitFileT(t *testing.T, path string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		if _, err := os.Stat(path); err == nil {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("%s never appeared", path)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func readFileOrEmpty(path string) string {
	b, _ := os.ReadFile(path)
	return string(b)
}
