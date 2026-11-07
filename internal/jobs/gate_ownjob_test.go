package jobs

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

// The gate lock is not reentrant (#208): a successful acquisition requires a successful exclusive flock
// on the shared lock file, nothing else grants entry. A gate of the caller's own job, its own parent, or
// a holder line or environment that claims either, is waited for like any other holder, within the
// caller's bound; a holder that releases lets the next gate in, one that does not times it out.

// wantBoundedWait fails unless err is the bounded wait's timeout.
func wantBoundedWait(t *testing.T, what string, err error) {
	t.Helper()
	if err == nil || !strings.Contains(err.Error(), "still held after") {
		t.Fatalf("%s: %v; want the bounded wait's timeout", what, err)
	}
}

// childGateT runs `agent-jobs gate run` of job "victim" on the lock in lockDir with lockWait, the command
// writing a file, plus env, and returns its output and whether the command ran. This test process may
// hold the lock: it is then the child gate's parent holding it, as a job's wrapper during its acceptance
// step or a gate around a gate is.
func childGateT(t *testing.T, lockDir, lockWait string, env ...string) (out string, ran bool) {
	t.Helper()
	repo, _, head, _ := gateFixture(t)
	marker := filepath.Join(t.TempDir(), "ran")
	child := exec.Command(selfBin, "gate", "run", "--repo", repo, "--head", head, "--lock-wait", lockWait, "--",
		"/bin/sh", "-c", "echo ran > "+marker)
	child.Env = append(append(os.Environ(), "AGENT_JOBS_TEST_GATE_LOCK_DIR="+lockDir, JobEnvVar+"=victim"), env...)
	b, _ := child.CombinedOutput()
	_, err := os.Stat(marker)
	return string(b), err == nil
}

// A gate of the same job that holds the lock and releases it (here this test process, the child gate's
// parent, its holder line naming the victim job) lets the next gate of the job succeed after waiting.
func TestGateSameJobReleaseLetsNextSucceed(t *testing.T) {
	lockDir := t.TempDir()
	lock := filepath.Join(lockDir, GateLockName)
	held := holdGateT(t, lock)
	writeGateHolderLine(lock, fmt.Sprintf("pid %d job victim since x: acceptance", os.Getpid()))
	go func() { time.Sleep(1500 * time.Millisecond); held.release() }()
	out, ran := childGateT(t, lockDir, "20s")
	if !ran || lastLine(out) != "exit=0" || !strings.Contains(out, "gate: waited ") {
		t.Fatalf("a gate of the same job after its holder released: ran %v:\n%s", ran, out)
	}
}

// A gate of the same job that keeps the lock times the next one out within its bound, naming the
// (untrusted) holder line, without running its command.
func TestGateSameJobContentionTimesOut(t *testing.T) {
	lockDir := t.TempDir()
	lock := filepath.Join(lockDir, GateLockName)
	holdGateT(t, lock)
	writeGateHolderLine(lock, fmt.Sprintf("pid %d job victim since x: acceptance", os.Getpid()))
	start := time.Now()
	out, ran := childGateT(t, lockDir, "1s")
	if ran || lastLine(out) != fmt.Sprintf("exit=%d", GateExitRunner) || !strings.Contains(out, "still held after 1s") ||
		!strings.Contains(out, "untrusted holder line: pid ") || time.Since(start) > 10*time.Second {
		t.Fatalf("persistent contention by the same job after %s: ran %v:\n%s", time.Since(start), ran, out)
	}
}

// A holder line naming the caller's parent and job, with the parent marker that granted reentry before
// (AGENT_JOBS_GATE_HOLDER naming that pid) copied into the caller's environment, grants nothing: the
// caller waits, bounded, and its command does not run.
func TestGateForgedHolderMarkersNeverGrantEntry(t *testing.T) {
	lockDir := t.TempDir()
	lock := filepath.Join(lockDir, GateLockName)
	holdGateT(t, lock)
	writeGateHolderLine(lock, fmt.Sprintf("pid %d job victim since x: forged parent gate", os.Getpid()))
	out, ran := childGateT(t, lockDir, "1s", "AGENT_JOBS_GATE_HOLDER="+strconv.Itoa(os.Getpid()))
	if ran || !strings.Contains(out, "still held after 1s") {
		t.Fatalf("forged holder line and parent marker: ran %v:\n%s", ran, out)
	}
	// In process: a holder line naming a live process of the victim job (its marker in its environ) or
	// this process's own parent is a held lock like any other.
	for _, pid := range []int{startMarkedSleeper(t, "victim"), os.Getppid()} {
		writeGateHolderLine(lock, fmt.Sprintf("pid %d job victim since x: forged", pid))
		_, err := acquireGateLock(context.Background(), gateLockReq{path: lock, wait: 300 * time.Millisecond, job: "victim"})
		wantBoundedWait(t, fmt.Sprintf("holder line naming pid %d", pid), err)
	}
}

// Nested gates are unsupported (#208): a gate inside a gate (the command of `agent-jobs gate run` runs
// it again, e.g. scripts/check.sh calling the gate runner) waits for its parent's lock within its bound
// and times out without running its command; the outer gate ends with that exit code.
func TestGateRunNestedTimesOut(t *testing.T) {
	repo, _, head, _ := gateFixture(t)
	lockDir := t.TempDir()
	marker := filepath.Join(t.TempDir(), "inner-ran")
	inner := fmt.Sprintf("%s gate run --repo %s --head %s --lock-wait 1s -- touch %s", selfBin, repo, head, marker)
	cmd := exec.Command(selfBin, "gate", "run", "--repo", repo, "--head", head, "--lock-wait", "3s", "--", "/bin/sh", "-c", inner)
	cmd.Env = append(os.Environ(), "AGENT_JOBS_TEST_GATE_LOCK_DIR="+lockDir)
	start := time.Now()
	b, _ := cmd.CombinedOutput()
	out := string(b)
	want := fmt.Sprintf("exit=%d", GateExitRunner)
	_, err := os.Stat(marker)
	if err == nil || strings.Count(out, want) != 2 || !strings.Contains(out, "still held after 1s") ||
		time.Since(start) > 10*time.Second {
		t.Fatalf("nested gate after %s:\n%s", time.Since(start), out)
	}
}
