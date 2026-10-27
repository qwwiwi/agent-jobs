package jobs

import (
	"bytes"
	"errors"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

// fakeGitOnPath puts a git that runs script (a sh body) first on PATH.
func fakeGitOnPath(t *testing.T, script string) {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "git"), []byte("#!/bin/sh\n"+script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
}

// shortGitTimeout sets gitTimeout to d for the test.
func shortGitTimeout(t *testing.T, d time.Duration) {
	t.Helper()
	orig := gitTimeout
	gitTimeout = d
	t.Cleanup(func() { gitTimeout = orig })
}

// A git that never exits (a lock, a stalled file system, a prompt) fails with errGitTimeout within the
// bound (#100): its whole process group is killed, also a child that holds its stdout, and it runs with
// GIT_TERMINAL_PROMPT=0 whatever environment the caller passed.
func TestGitTimesOut(t *testing.T) {
	gitTimesOut(t, "")
}

// A fake git that is slower to reach its first line than the 300ms bound (a loaded macOS host) is
// killed before it records anything: that proves nothing about the environment, and TestGitTimesOut
// runs it again with a longer bound instead of failing on the missing files.
func TestGitTimesOutSlowStart(t *testing.T) {
	gitTimesOut(t, "sleep 0.5\n")
}

// gitTimesOut is TestGitTimesOut with a fake git that runs prelude first. The fake writes to a state
// directory named in the script, so that whether it ran does not depend on the environment under test,
// and records GIT_TERMINAL_PROMPT and FAKE_GIT_ENV, the caller's variable. A call whose fake was killed
// before it got to its child proves nothing and is repeated with a four times longer bound.
func gitTimesOut(t *testing.T, prelude string) {
	state := t.TempDir()
	if strings.ContainsRune(state, '\'') {
		t.Fatalf("state directory %q has a quote", state)
	}
	fakeGitOnPath(t, prelude+`echo "$GIT_TERMINAL_PROMPT" > '`+state+`/prompt'
echo "$FAKE_GIT_ENV" > '`+state+`/env'
sleep 60 &
echo $! > '`+state+`/child'
wait
`)
	type result struct {
		out string
		err error
	}
	var child []byte
	for timeout := 300 * time.Millisecond; ; timeout *= 4 {
		shortGitTimeout(t, timeout)
		done := make(chan result, 1)
		start := time.Now()
		go func() {
			out, err := gitEnv(t.TempDir(), []string{"PATH=" + os.Getenv("PATH"), "FAKE_GIT_ENV=passed"}, "status")
			done <- result{out, err}
		}()
		var r result
		select {
		case r = <-done:
		case <-time.After(timeout + 10*time.Second):
			t.Fatalf("git did not return within 10s of a %s timeout", timeout)
		}
		if !errors.Is(r.err, errGitTimeout) || !strings.Contains(r.err.Error(), "git status") {
			t.Fatalf("want a timeout error naming the command, got %q %v", r.out, r.err)
		}
		if took := time.Since(start); took > timeout+5*time.Second {
			t.Fatalf("returned after %s", took)
		}
		var err error
		if child, err = os.ReadFile(filepath.Join(state, "child")); err == nil {
			break
		}
		if !errors.Is(err, os.ErrNotExist) {
			t.Fatal(err)
		}
		if timeout >= 4*time.Second {
			t.Fatalf("the fake git did not get to its child within a %s timeout", timeout)
		}
		t.Logf("the fake git was not up within %s: again with a longer timeout", timeout)
	}
	if b, err := os.ReadFile(filepath.Join(state, "prompt")); err != nil || strings.TrimSpace(string(b)) != "0" {
		t.Fatalf("GIT_TERMINAL_PROMPT: %q %v", b, err)
	}
	if b, err := os.ReadFile(filepath.Join(state, "env")); err != nil || strings.TrimSpace(string(b)) != "passed" {
		t.Fatalf("the caller's environment did not reach git: FAKE_GIT_ENV %q %v", b, err)
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(child)))
	if err != nil {
		t.Fatal(err)
	}
	for deadline := time.Now().Add(5 * time.Second); ; time.Sleep(50 * time.Millisecond) {
		if processGone(t, pid) {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("git's child %d still runs: the process group was not killed", pid)
		}
	}
}

// A git call of the shipped check that hangs in one repository leaves it not shipped with a logged
// warning, and the tick goes on (#100): it returns without an error within the bound, another
// repository ships in the same tick, and the hung one ships at the next tick once git answers.
func TestMergeGateShippedGitTimeout(t *testing.T) {
	m := newManager(t)
	hung, _, hungX := shippedRepo(t, m)
	good, _, goodX := shippedRepo(t, m)
	gitT(t, hung, "update-ref", "refs/remotes/origin/main", hungX)
	gitT(t, good, "update-ref", "refs/remotes/origin/main", goodX)
	var logBuf bytes.Buffer
	m.Log = log.New(&logBuf, "", 0)

	real, err := exec.LookPath("git")
	if err != nil {
		t.Fatal(err)
	}
	// merge-base in the hung repository never answers; every other git call is the real one.
	t.Setenv("FAKE_GIT_HANG", mainWorktree(hung))
	fakeGitOnPath(t, `if [ -n "$FAKE_GIT_HANG" ] && [ "$2" = "$FAKE_GIT_HANG" ]; then
  for a in "$@"; do [ "$a" = merge-base ] && exec sleep 1000; done
fi
exec '`+real+`' "$@"
`)
	shortGitTimeout(t, 2*time.Second)
	done := make(chan error, 1)
	go func() { done <- m.Tick() }()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("tick: %v", err)
		}
	case <-time.After(60 * time.Second):
		t.Fatal("tick did not return within 60s of a 2s git timeout")
	}
	if g, err := m.Store.GateRepoOf(mainWorktree(hung)); err != nil || g.ShippedSeq != 0 {
		t.Fatalf("shipped although git timed out: %+v %v", g, err)
	}
	if g, err := m.Store.GateRepoOf(mainWorktree(good)); err != nil || g.ShippedSeq == 0 || g.ShippedSHA != goodX {
		t.Fatalf("the other repository did not ship in the same tick: %+v %v", g, err)
	}
	if l := logBuf.String(); !strings.Contains(l, "merge gate: warning") || !strings.Contains(l, "timed out") ||
		!strings.Contains(l, "next tick") {
		t.Fatalf("no timeout warning logged: %q", l)
	}

	t.Setenv("FAKE_GIT_HANG", "")
	if err := m.Tick(); err != nil {
		t.Fatal(err)
	}
	if g, err := m.Store.GateRepoOf(mainWorktree(hung)); err != nil || g.ShippedSeq == 0 || g.ShippedSHA != hungX {
		t.Fatalf("not shipped at the next tick: %+v %v", g, err)
	}
}

// shortGitLongTimeout sets gitLongTimeout to d for the test.
func shortGitLongTimeout(t *testing.T, d time.Duration) {
	t.Helper()
	orig := gitLongTimeout
	gitLongTimeout = d
	t.Cleanup(func() { gitLongTimeout = orig })
}

// returnsWithin runs f and fails the test when it does not return within limit.
func returnsWithin[T any](t *testing.T, limit time.Duration, what string, f func() T) T {
	t.Helper()
	done := make(chan T, 1)
	go func() { done <- f() }()
	select {
	case v := <-done:
		return v
	case <-time.After(limit):
		t.Fatalf("%s did not return within %s", what, limit)
	}
	panic("unreachable")
}

// gitRaw, the raw-output call of Cleanup (config, ls-files, format-patch, diff), is bounded like the
// other daemon git calls (#100): a hung git fails with errGitTimeout and no output.
func TestGitRawTimesOut(t *testing.T) {
	fakeGitOnPath(t, "echo partial\nexec sleep 60\n")
	shortGitLongTimeout(t, 300*time.Millisecond)
	type result struct {
		out []byte
		err error
	}
	r := returnsWithin(t, 10*time.Second, "gitRaw", func() result {
		out, err := gitRaw(t.TempDir(), "format-patch", "--stdout")
		return result{out, err}
	})
	if !errors.Is(r.err, errGitTimeout) || len(r.out) != 0 || !strings.Contains(r.err.Error(), "git format-patch") {
		t.Fatalf("want a timeout error and no output, got %q %v", r.out, r.err)
	}
}

// gitRaw keeps git's stdout byte for byte: leading and trailing blanks, NULs, final newlines.
func TestGitRawKeepsBytes(t *testing.T) {
	fakeGitOnPath(t, `printf '  a\000b \n\n'`+"\n")
	out, err := gitRaw(t.TempDir(), "ls-files", "-z")
	if err != nil || string(out) != "  a\x00b \n\n" {
		t.Fatalf("got %q %v", out, err)
	}
}

// The other daemon git calls outside the helper are bounded too: the remotes read of the project cards
// and the streaming git cat-file of the judge tree (#100).
func TestDaemonGitCallsTimeOut(t *testing.T) {
	repo, sha := gateRepo(t)
	real, err := exec.LookPath("git")
	if err != nil {
		t.Fatal(err)
	}
	// config and cat-file never answer; every other git call is the real one.
	fakeGitOnPath(t, `for a in "$@"; do case "$a" in config|cat-file) exec sleep 60;; esac; done
exec '`+real+`' "$@"
`)
	shortGitTimeout(t, 300*time.Millisecond)
	shortGitLongTimeout(t, 300*time.Millisecond)
	if err := returnsWithin(t, 10*time.Second, "repoRemotes", func() error { _, err := repoRemotes(repo); return err }); !errors.Is(err, errGitTimeout) {
		t.Fatalf("repoRemotes: %v", err)
	}
	if err := returnsWithin(t, 10*time.Second, "materializeTree", func() error {
		return materializeTree(repo, sha, t.TempDir(), nil)
	}); !errors.Is(err, errGitTimeout) {
		t.Fatalf("materializeTree: %v", err)
	}
}

// processGone counts a zombie as ended, where kill(pid, 0) still succeeds (#100).
func TestProcessGoneZombie(t *testing.T) {
	cmd := exec.Command("true")
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	pid := cmd.Process.Pid
	// Not reaped until the end: a zombie meanwhile.
	defer func() { _ = cmd.Wait() }()
	for deadline := time.Now().Add(5 * time.Second); !processGone(t, pid); time.Sleep(10 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatalf("process %d not seen as ended", pid)
		}
	}
	if err := syscall.Kill(pid, 0); err != nil {
		t.Fatalf("kill(pid, 0) on the zombie: %v, want success (the check processGone replaces)", err)
	}
}

// A timeout of the judge tree's git cat-file is errGitTimeout whatever ends first after the deadline
// (#100): the closed pipes, or git's exit status the reaper hands over before the deferred wait sees
// the deadline. Repeated, since the order is a race.
func TestMaterializeTreeTimeoutAlwaysNamed(t *testing.T) {
	repo, sha := gateRepo(t)
	real, err := exec.LookPath("git")
	if err != nil {
		t.Fatal(err)
	}
	fakeGitOnPath(t, `for a in "$@"; do [ "$a" = cat-file ] && exec sleep 60; done
exec '`+real+`' "$@"
`)
	shortGitLongTimeout(t, 50*time.Millisecond)
	for i := 0; i < 40; i++ {
		err := returnsWithin(t, 10*time.Second, "materializeTree", func() error {
			return materializeTree(repo, sha, t.TempDir(), nil)
		})
		if !errors.Is(err, errGitTimeout) {
			t.Fatalf("run %d: want errGitTimeout, got %v", i, err)
		}
	}
}

// A git that exits while a process of its group still holds its stdout does not leave that process
// running past the deadline (#100): the group is killed while git, not reaped yet, still reserves the
// group's ID, and the call is errGitTimeout without output.
func TestGitTimeoutKillsChildAfterGitExit(t *testing.T) {
	state := t.TempDir()
	fakeGitOnPath(t, `echo partial
sleep 60 &
echo $! > "$FAKE_GIT_STATE/child"
exit 0
`)
	// A deadline wide enough for the fake git to write the child's pid on a busy host, and a wait delay
	// past it, so that the kill comes from the deadline.
	shortGitTimeout(t, time.Second)
	origDelay := gitWaitDelay
	gitWaitDelay = 30 * time.Second
	t.Cleanup(func() { gitWaitDelay = origDelay })
	type result struct {
		out string
		err error
	}
	r := returnsWithin(t, 10*time.Second, "git", func() result {
		out, err := gitEnv(t.TempDir(), []string{"PATH=" + os.Getenv("PATH"), "FAKE_GIT_STATE=" + state}, "status")
		return result{out, err}
	})
	b, err := os.ReadFile(filepath.Join(state, "child"))
	if err != nil {
		t.Fatal(err)
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(b)))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = syscall.Kill(pid, syscall.SIGKILL) }()
	if !errors.Is(r.err, errGitTimeout) || r.out != "" {
		t.Fatalf("want a timeout error and no output, got %q %v", r.out, r.err)
	}
	for deadline := time.Now().Add(5 * time.Second); !processGone(t, pid); time.Sleep(50 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatalf("git's child %d still runs after the deadline: git's group was not killed", pid)
		}
	}
}
