package jobs

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

// ErrGateOwnJob (a fast fail) only on kernel-verified membership (#208 fix loop 2): the holder pid holds
// the flock on the lock (/proc/locks) and is an ancestor of the caller or in the caller's own job scope
// (/proc/<pid>/cgroup). The holder line and the job marker in /proc/<pid>/environ are written by the
// job's own user, so they never decide; an unverified or unknown holder is waited for, bounded.

// victimScope is a job scope cgroup path of job "victim", as systemd names it.
const victimScope = "/agents.slice/user@1000.service/app.slice/agent-jobs-thrall-victim.scope"

// fakeProcT points procRoot at a stand-in /proc for this process: /proc/locks and, for this process's
// ancestors and pids, their real stat and environ, with the cgroup v2 path cgroups gives (the real
// /proc/<pid>/cgroup otherwise). A pid in noEnviron has no environ file (unreadable).
func fakeProcT(t *testing.T, cgroups map[int]string, noEnviron map[int]bool, pids ...int) {
	t.Helper()
	root := t.TempDir()
	if err := os.Symlink("/proc/locks", filepath.Join(root, "locks")); err != nil {
		t.Fatal(err)
	}
	all := append([]int{}, pids...)
	for cur := os.Getpid(); cur > 1; {
		all = append(all, cur)
		parent, err := procParent(cur)
		if err != nil {
			t.Fatalf("no /proc: %v", err)
		}
		cur = parent
	}
	for _, pid := range all {
		dir := filepath.Join(root, strconv.Itoa(pid))
		if _, err := os.Stat(dir); err == nil {
			continue
		}
		if err := os.Mkdir(dir, 0o700); err != nil {
			t.Fatal(err)
		}
		real := filepath.Join("/proc", strconv.Itoa(pid))
		links := []string{"stat"}
		if !noEnviron[pid] {
			links = append(links, "environ")
		}
		if cg, ok := cgroups[pid]; ok {
			if err := os.WriteFile(filepath.Join(dir, "cgroup"), []byte("0::"+cg+"\n"), 0o600); err != nil {
				t.Fatal(err)
			}
		} else {
			links = append(links, "cgroup")
		}
		for _, l := range links {
			if err := os.Symlink(filepath.Join(real, l), filepath.Join(dir, l)); err != nil {
				t.Fatal(err)
			}
		}
	}
	old := procRoot
	procRoot = root
	t.Cleanup(func() { procRoot = old })
}

// skipNoProc skips a test that needs a real /proc with /proc/locks.
func skipNoProc(t *testing.T) {
	t.Helper()
	if _, err := os.Stat("/proc/locks"); err != nil {
		t.Skip("no /proc/locks")
	}
}

// wantBoundedWait fails unless err is the bounded wait's timeout (not ErrGateOwnJob).
func wantBoundedWait(t *testing.T, what string, err error) {
	t.Helper()
	if err == nil || errors.Is(err, ErrGateOwnJob) || !strings.Contains(err.Error(), "still held after") {
		t.Fatalf("%s: %v; want the bounded wait's timeout, never ErrGateOwnJob", what, err)
	}
}

// Without /proc (macOS, a mount the caller cannot read) nothing is verified: a forged holder line naming
// the victim job and a live pid is waited for like any held lock.
func TestGateOwnJobNoProcWaits(t *testing.T) {
	lock := filepath.Join(t.TempDir(), GateLockName)
	holdGateT(t, lock)
	writeGateHolderLine(lock, fmt.Sprintf("pid %d job victim since x: forged", startSleeper(t)))
	old := procRoot
	procRoot = filepath.Join(t.TempDir(), "no-proc")
	t.Cleanup(func() { procRoot = old })
	_, err := acquireGateLock(context.Background(), gateLockReq{path: lock, wait: 300 * time.Millisecond, job: "victim"})
	wantBoundedWait(t, "no /proc, forged holder line", err)
}

// Without /proc even a real gate of the job holding the flock cannot be verified: it is waited for.
func TestGateOwnJobNoProcRealHolderWaits(t *testing.T) {
	lockDir := t.TempDir()
	lock := filepath.Join(lockDir, GateLockName)
	startJobGateT(t, lockDir, "victim")
	old := procRoot
	procRoot = filepath.Join(t.TempDir(), "no-proc")
	t.Cleanup(func() { procRoot = old })
	_, err := acquireGateLock(context.Background(), gateLockReq{path: lock, wait: 300 * time.Millisecond, job: "victim"})
	wantBoundedWait(t, "no /proc, a real gate of the job", err)
}

// A process of the same user that really holds the flock and carries a copied AGENT_JOBS_JOB=victim, but
// is outside the victim's scope and process tree, is not of the victim job: the victim waits.
func TestGateOwnJobCopiedMarkerWaits(t *testing.T) {
	skipNoProc(t)
	lockDir := t.TempDir()
	lock := filepath.Join(lockDir, GateLockName)
	gate := startJobGateT(t, lockDir, "victim") // its environ carries the marker; it holds the flock
	fakeProcT(t, map[int]string{os.Getpid(): victimScope, gate: "/user.slice/user-1000.slice/session-7.scope"}, nil, gate)
	_, err := acquireGateLock(context.Background(), gateLockReq{path: lock, wait: 300 * time.Millisecond, job: "victim"})
	wantBoundedWait(t, "a flock holder outside the scope with a copied job marker", err)
}

// A gate of the job left in the background (in the caller's own job scope, holding the flock) fails the
// caller at once, whatever its environ says (here: unreadable).
func TestGateOwnJobSameScopeFails(t *testing.T) {
	skipNoProc(t)
	lockDir := t.TempDir()
	lock := filepath.Join(lockDir, GateLockName)
	gate := startJobGateT(t, lockDir, "victim")
	fakeProcT(t, map[int]string{os.Getpid(): victimScope, gate: victimScope + "/sub"}, map[int]bool{gate: true}, gate)
	start := time.Now()
	_, err := acquireGateLock(context.Background(), gateLockReq{path: lock, wait: 10 * time.Second, job: "victim"})
	if !errors.Is(err, ErrGateOwnJob) || !strings.Contains(err.Error(), fmt.Sprintf("pid %d job victim", gate)) ||
		time.Since(start) > 2*time.Second {
		t.Fatalf("a gate in the caller's own job scope: %v after %s; want ErrGateOwnJob at once", err, time.Since(start))
	}
}

// The same scope must be the caller's job scope of that job: a holder sharing a scope that is no job
// scope, or a job scope of another job id, is waited for.
func TestGateOwnJobScopeMustBeTheJobs(t *testing.T) {
	skipNoProc(t)
	lockDir := t.TempDir()
	lock := filepath.Join(lockDir, GateLockName)
	gate := startJobGateT(t, lockDir, "victim")
	for _, scope := range []string{
		"/user.slice/user-1000.slice/session-7.scope",
		"/agents.slice/user@1000.service/app.slice/agent-jobs-thrall-other.scope",
	} {
		fakeProcT(t, map[int]string{os.Getpid(): scope, gate: scope}, map[int]bool{gate: true}, gate)
		_, err := acquireGateLock(context.Background(), gateLockReq{path: lock, wait: 300 * time.Millisecond, job: "victim"})
		wantBoundedWait(t, "shared cgroup "+scope, err)
	}
}

// An ancestor holding the flock (the job's wrapper during its acceptance step; here this test process,
// for a gate it starts) is of the caller's job: the child gate fails at once. The holder carries no job
// marker in its environ: ancestry alone decides.
func TestGateOwnJobAncestorFails(t *testing.T) {
	skipNoProc(t)
	if envValue(os.Environ(), JobEnvVar) == "victim" {
		t.Skip("this process carries the victim marker")
	}
	lockDir := t.TempDir()
	lock := filepath.Join(lockDir, GateLockName)
	holdGateT(t, lock)
	writeGateHolderLine(lock, fmt.Sprintf("pid %d job victim since x: acceptance", os.Getpid()))
	repo, _, head, _ := gateFixture(t)
	child := exec.Command(selfBin, "gate", "run", "--repo", repo, "--head", head, "--lock-wait", "20s", "--", "true")
	child.Env = append(os.Environ(), "AGENT_JOBS_TEST_GATE_LOCK_DIR="+lockDir, JobEnvVar+"=victim")
	start := time.Now()
	out, _ := child.CombinedOutput()
	if !strings.Contains(string(out), ErrGateOwnJob.Error()) || time.Since(start) > 10*time.Second {
		t.Fatalf("a gate under its own job's holding ancestor after %s:\n%s", time.Since(start), out)
	}
}
