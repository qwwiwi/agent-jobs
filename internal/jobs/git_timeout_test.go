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
	state := t.TempDir()
	t.Setenv("FAKE_GIT_STATE", state)
	fakeGitOnPath(t, `echo "$GIT_TERMINAL_PROMPT" > "$FAKE_GIT_STATE/prompt"
sleep 1000 &
echo $! > "$FAKE_GIT_STATE/child"
wait
`)
	shortGitTimeout(t, 300*time.Millisecond)
	type result struct {
		out string
		err error
	}
	done := make(chan result, 1)
	start := time.Now()
	go func() {
		out, err := gitEnv(t.TempDir(), []string{"PATH=" + os.Getenv("PATH"), "FAKE_GIT_STATE=" + state}, "status")
		done <- result{out, err}
	}()
	var r result
	select {
	case r = <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("git did not return within 10s of a 300ms timeout")
	}
	if !errors.Is(r.err, errGitTimeout) || !strings.Contains(r.err.Error(), "git status") {
		t.Fatalf("want a timeout error naming the command, got %q %v", r.out, r.err)
	}
	if took := time.Since(start); took > 5*time.Second {
		t.Fatalf("returned after %s", took)
	}
	if b, err := os.ReadFile(filepath.Join(state, "prompt")); err != nil || strings.TrimSpace(string(b)) != "0" {
		t.Fatalf("GIT_TERMINAL_PROMPT: %q %v", b, err)
	}
	b, err := os.ReadFile(filepath.Join(state, "child"))
	if err != nil {
		t.Fatal(err)
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(b)))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = syscall.Kill(pid, syscall.SIGKILL) })
	for deadline := time.Now().Add(5 * time.Second); ; time.Sleep(50 * time.Millisecond) {
		if err := syscall.Kill(pid, 0); errors.Is(err, syscall.ESRCH) {
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
