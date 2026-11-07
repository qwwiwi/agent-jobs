package jobs

import (
	"bytes"
	"context"
	"errors"
	"fmt"
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

// holdGateT takes the gate lock at path in this process and returns its lease (released on cleanup).
func holdGateT(t *testing.T, path string) *gateLease {
	t.Helper()
	l, err := acquireGateLock(context.Background(), gateLockReq{path: path, wait: time.Second, detail: ": test holder"})
	if err != nil {
		t.Fatal(err)
	}
	var once sync.Once
	rel := l.release
	l.release = func() { once.Do(rel) }
	t.Cleanup(func() { once.Do(rel) })
	return l
}

// startSleeper starts a process that lives until the test ends and returns its pid.
func startSleeper(t *testing.T) int {
	t.Helper()
	cmd := exec.Command("sleep", "60")
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cmd.Process.Kill(); _ = cmd.Wait() })
	return cmd.Process.Pid
}

// Waiters get the gate lock in the order they started waiting (#208: the daemon's, the jobs' and the
// main session's gates share the lock fairly), not in the order their polls happen to hit a free lock.
func TestGateLockFIFO(t *testing.T) {
	lock := filepath.Join(t.TempDir(), GateLockName)
	holder := holdGateT(t, lock)
	const n = 6
	var mu sync.Mutex
	var order []int
	var wg sync.WaitGroup
	for i := range n {
		wg.Add(1)
		go func() {
			defer wg.Done()
			l, err := acquireGateLock(context.Background(), gateLockReq{path: lock, wait: time.Minute, detail: fmt.Sprintf(": waiter %d", i)})
			if err != nil {
				t.Error(err)
				return
			}
			mu.Lock()
			order = append(order, i)
			mu.Unlock()
			time.Sleep(3 * gateLockPoll)
			l.release()
		}()
		time.Sleep(3 * gateLockPoll) // waiter i has joined the queue before i+1 starts
	}
	holder.release()
	wg.Wait()
	for i, got := range order {
		if got != i {
			t.Fatalf("lock order %v, want 0..%d: a later waiter overtook an earlier one", order, n-1)
		}
	}
	if len(order) != n {
		t.Fatalf("only %d of %d waiters got the lock", len(order), n)
	}
}

// A waiter that died, or stopped refreshing its heartbeat, does not hold up the queue.
func TestGateLockQueueForgetsDeadWaiters(t *testing.T) {
	lock := filepath.Join(t.TempDir(), GateLockName)
	dead := exec.Command("true")
	if err := dead.Run(); err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	queue := fmt.Sprintf("%d %d %d\n%d %d %d\n", now.Add(-time.Minute).UnixNano(), dead.Process.Pid, now.UnixMilli(),
		now.Add(-time.Second).UnixNano(), os.Getppid(), now.Add(-time.Hour).UnixMilli())
	if err := os.WriteFile(lock+gateQueueSuffix, []byte(queue), 0o666); err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	l, err := acquireGateLock(context.Background(), gateLockReq{path: lock, wait: 2 * time.Second})
	if err != nil {
		t.Fatalf("a dead and a stale waiter held up the queue: %v", err)
	}
	defer l.release()
	if d := time.Since(start); d > time.Second || l.waited != 0 {
		t.Fatalf("waited %s (recorded %s) behind waiters that are gone", d, l.waited)
	}
}

// A gate never waits for the lock its own process tree holds: under an ancestor's gate it runs inside it
// (a gate inside a gate, a gate in the acceptance command); a gate of the same job that is no ancestor
// makes it fail at once, naming the holder, instead of waiting for itself.
func TestGateLockOwnTree(t *testing.T) {
	lock := filepath.Join(t.TempDir(), GateLockName)
	holdGateT(t, lock)

	writeGateHolderLine(lock, fmt.Sprintf("pid %d job j1 since x: parent gate", os.Getppid()))
	marker := []string{GateHolderEnv + "=" + strconv.Itoa(os.Getppid())}
	start := time.Now()
	l, err := acquireGateLock(context.Background(), gateLockReq{path: lock, wait: time.Minute, job: "j1", env: marker})
	if err != nil || !l.reentrant || time.Since(start) > 2*time.Second {
		t.Fatalf("under an ancestor's gate: lease %+v, err %v after %s; want reentrant at once", l, err, time.Since(start))
	}
	// An ancestor holding the lock without having marked this process as its gate's command (a daemon
	// that holds it, a shell) is waited for like any other holder.
	_, err = acquireGateLock(context.Background(), gateLockReq{path: lock, wait: 300 * time.Millisecond, job: "j2"})
	if err == nil || !strings.Contains(err.Error(), "still held after") {
		t.Fatalf("an unmarked descendant of the holder passed through: %v", err)
	}
	// A marker naming the holder while it is no ancestor (copied) does not pass either.
	sleeper := startSleeper(t)
	writeGateHolderLine(lock, fmt.Sprintf("pid %d job - since x: unrelated gate", sleeper))
	_, err = acquireGateLock(context.Background(), gateLockReq{path: lock, wait: 300 * time.Millisecond,
		env: []string{GateHolderEnv + "=" + strconv.Itoa(sleeper)}})
	if err == nil || !strings.Contains(err.Error(), "unrelated gate") {
		t.Fatalf("a copied marker passed through: %v", err)
	}

	sibling := startMarkedSleeper(t, "j1") // a gate inside job j1 carries its marker
	writeGateHolderLine(lock, fmt.Sprintf("pid %d job j1 since x: background gate", sibling))
	start = time.Now()
	_, err = acquireGateLock(context.Background(), gateLockReq{path: lock, wait: time.Minute, job: "j1"})
	if !errors.Is(err, ErrGateOwnJob) || !strings.Contains(err.Error(), "background gate") || time.Since(start) > 2*time.Second {
		t.Fatalf("held by a gate of the same job: err %v after %s; want ErrGateOwnJob naming the holder at once", err, time.Since(start))
	}

	// Another job's gate: a bounded wait, then an error naming the holder.
	_, err = acquireGateLock(context.Background(), gateLockReq{path: lock, wait: 300 * time.Millisecond, job: "j2"})
	if err == nil || errors.Is(err, ErrGateOwnJob) || !strings.Contains(err.Error(), "still held after 300ms (by: pid ") ||
		!strings.Contains(err.Error(), "background gate") {
		t.Fatalf("held by another job's gate: %v", err)
	}
}

// A holder line written by a holder that died is no evidence: the lock itself decides.
func TestGateLockStaleHolderLine(t *testing.T) {
	lock := filepath.Join(t.TempDir(), GateLockName)
	dead := exec.Command("true")
	if err := dead.Run(); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(lock, []byte(fmt.Sprintf("pid %d job j1 since x: killed gate\n", dead.Process.Pid)), 0o666); err != nil {
		t.Fatal(err)
	}
	l, err := acquireGateLock(context.Background(), gateLockReq{path: lock, wait: time.Second, job: "j1"})
	if err != nil || l.reentrant {
		t.Fatalf("free lock with a stale holder line: lease %+v, err %v", l, err)
	}
	l.release()
}

// A gate inside a gate (the command of `agent-jobs gate run` runs it again, e.g. scripts/check.sh calling
// the gate runner) runs inside its parent's lock instead of waiting for it until its lock wait ends.
func TestGateRunNestedRunsInsideParent(t *testing.T) {
	repo, _, head, _ := gateFixture(t)
	lockDir := t.TempDir()
	inner := fmt.Sprintf("%s gate run --repo %s --head %s --lock-wait 3s -- /bin/sh -c 'echo inner-ran'", selfBin, repo, head)
	cmd := exec.Command(selfBin, "gate", "run", "--repo", repo, "--head", head, "--lock-wait", "3s", "--", "/bin/sh", "-c", inner)
	cmd.Env = append(os.Environ(), "AGENT_JOBS_TEST_GATE_LOCK_DIR="+lockDir)
	var out, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errb
	start := time.Now()
	if err := cmd.Run(); err != nil {
		t.Fatalf("nested gate: %v\nout %s\nerr %s", err, out.String(), errb.String())
	}
	if !strings.Contains(out.String(), "inner-ran") || strings.Count(out.String(), "exit=0") != 2 ||
		!strings.Contains(errb.String(), "running inside it") || time.Since(start) > 3*time.Second {
		t.Fatalf("nested gate after %s:\nout %s\nerr %s", time.Since(start), out.String(), errb.String())
	}
}

// A gate killed while it holds the lock releases it: the flock belongs to the runner's open file and
// ends with its process (the job's process group or scope is killed on a timeout, cancel or stop).
func TestGateLockReleasedOnKill(t *testing.T) {
	repo, _, head, _ := gateFixture(t)
	lockDir := t.TempDir()
	marker := filepath.Join(t.TempDir(), "started")
	cmd := exec.Command(selfBin, "gate", "run", "--repo", repo, "--head", head, "--", "/bin/sh", "-c",
		"echo $$ > "+marker+"; exec sleep 60")
	cmd.Env = append(os.Environ(), "AGENT_JOBS_TEST_GATE_LOCK_DIR="+lockDir)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true} // the "job's" process group
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(10 * time.Second)
	for {
		if b, err := os.ReadFile(marker); err == nil && len(bytes.TrimSpace(b)) > 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the gate's command did not start")
		}
		time.Sleep(20 * time.Millisecond)
	}
	suite := readPID(t, marker)
	t.Cleanup(func() { _ = syscall.Kill(-suite, syscall.SIGKILL) })
	lock := filepath.Join(lockDir, GateLockName)
	if pid, _ := parseGateHolder(readFileT(t, lock)); pid != cmd.Process.Pid {
		t.Fatalf("holder line %q does not name the runner %d", readFileT(t, lock), cmd.Process.Pid)
	}
	// The job is killed: its process group (the runner) and, as the job's scope or session sweep does,
	// the suite's own group.
	_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
	_ = syscall.Kill(-suite, syscall.SIGKILL)
	_ = cmd.Wait()
	l, err := acquireGateLock(context.Background(), gateLockReq{path: lock, wait: 3 * time.Second})
	if err != nil {
		t.Fatalf("the lock of a killed gate is still held: %v", err)
	}
	l.release()
}

// A gate inside a job records its wait in the job's folder (gate-wait.json), which job_list, status
// and the job's timeout read; the wait ends with the total it took, and the runner reports it.
func TestGateRunRecordsWaitInJobDir(t *testing.T) {
	repo, _, head, _ := gateFixture(t)
	state := t.TempDir()
	jobDir := filepath.Join(state, "jobs", "j1")
	if err := os.MkdirAll(jobDir, 0o700); err != nil {
		t.Fatal(err)
	}
	lock := filepath.Join(t.TempDir(), GateLockName)
	holder := holdGateT(t, lock)
	writeGateHolderLine(lock, gateHolderLine("", ": the orchestrator's check.sh"))
	env := append(os.Environ(), JobEnvVar+"=j1", JobDirEnvVar+"="+jobDir)
	type result struct {
		code     int
		out, err string
	}
	done := make(chan result, 1)
	go func() {
		code, out, errs := runGateT(t, GateRun{Dir: repo, Head: head, Argv: []string{"true"}, LockPath: lock, LockWait: time.Minute, Env: env})
		done <- result{code, out, errs}
	}()
	deadline := time.Now().Add(5 * time.Second)
	for {
		s, err := ReadGateWait(state, "j1")
		if err == nil && len(s.Waits) == 1 {
			if !strings.Contains(s.Waits[0].Holder, "the orchestrator's check.sh") || s.Waits[0].PID != os.Getpid() {
				t.Fatalf("recorded wait %+v", s.Waits[0])
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("no wait recorded in gate-wait.json: %+v %v", s, err)
		}
		time.Sleep(20 * time.Millisecond)
	}
	time.Sleep(1200 * time.Millisecond)
	holder.release()
	r := <-done
	if r.code != 0 || !strings.Contains(r.err, "gate: waited ") {
		t.Fatalf("code %d, stderr %q", r.code, r.err)
	}
	s, err := ReadGateWait(state, "j1")
	if err != nil || len(s.Waits) != 0 || s.WaitedSec < 1 {
		t.Fatalf("after the wait: %+v %v; want no current wait and waited_sec >= 1", s, err)
	}
	if line := readFileT(t, lock); line != "" {
		t.Fatalf("lock still names a holder after release: %q", line)
	}
}

// Without the job's folder in the environment (outside a job, or a folder that is not the marker's job)
// nothing is recorded.
func TestGateJobDirMustMatchMarker(t *testing.T) {
	for _, c := range []struct {
		env  []string
		job  string
		want string
	}{
		{[]string{JobDirEnvVar + "=/s/jobs/j1"}, "j1", "/s/jobs/j1"},
		{[]string{JobDirEnvVar + "=/s/jobs/j2"}, "j1", ""},
		{[]string{JobDirEnvVar + "=rel/j1"}, "j1", ""},
		{[]string{JobDirEnvVar + "=/s/jobs/j1"}, "", ""},
		{nil, "j1", ""},
	} {
		if got := gateJobDir(c.env, c.job); got != c.want {
			t.Errorf("gateJobDir(%v, %q) = %q, want %q", c.env, c.job, got, c.want)
		}
	}
}

func readFileT(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return strings.TrimSpace(string(b))
}

// The queue file is shared and writable by every user: a planted entry (a heartbeat in the future, pid 1)
// does not stop the queue, and a process that holds the queue file's own flock does not hang a gate
// past its lock wait (it falls back to polling the lock).
func TestGateLockQueueCannotBeJammed(t *testing.T) {
	lock := filepath.Join(t.TempDir(), GateLockName)
	now := time.Now()
	planted := fmt.Sprintf("0 1 %d\n1 %d %d\n", now.Add(time.Hour).UnixMilli(), os.Getppid(), now.Add(100*365*24*time.Hour).UnixMilli())
	if err := os.WriteFile(lock+gateQueueSuffix, []byte(planted), 0o666); err != nil {
		t.Fatal(err)
	}
	l, err := acquireGateLock(context.Background(), gateLockReq{path: lock, wait: 2 * time.Second})
	if err != nil {
		t.Fatalf("planted queue entries held up a free lock: %v", err)
	}
	l.release()

	q, err := os.OpenFile(lock+gateQueueSuffix, os.O_RDWR, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer q.Close()
	if err := syscall.Flock(int(q.Fd()), syscall.LOCK_EX); err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	l, err = acquireGateLock(context.Background(), gateLockReq{path: lock, wait: 3 * time.Second})
	if err != nil || time.Since(start) > 3*time.Second {
		t.Fatalf("with the queue file locked by someone: %v after %s", err, time.Since(start))
	}
	l.release()
	holdGateT(t, lock)
	start = time.Now()
	_, err = acquireGateLock(context.Background(), gateLockReq{path: lock, wait: 500 * time.Millisecond})
	if err == nil || time.Since(start) > 3*time.Second {
		t.Fatalf("a held lock with the queue file locked: %v after %s; want a bounded wait", err, time.Since(start))
	}
}

// Waits that overlap in time count once: the credit is the union of the attempt's wait intervals.
func TestGateWaitCreditUnion(t *testing.T) {
	start := time.Unix(1770000000, 0)
	s := GateWaitState{
		Done: []GateWaitDone{
			{Since: start.Add(time.Minute), Sec: 600},     // 1..11 min
			{Since: start.Add(2 * time.Minute), Sec: 600}, // 2..12 min, overlaps
			{Since: start.Add(-time.Hour), Sec: 3600},     // an earlier attempt, ends at start
			{Since: start.Add(20 * time.Minute), Sec: 60}, // 20..21 min
		},
		Waits: []GateWaitEntry{{PID: 1, Since: start.Add(20*time.Minute + 30*time.Second)}}, // 20.5..25 min
	}
	if got := s.Credit(start, start.Add(25*time.Minute)); got != 16*time.Minute {
		t.Fatalf("credit %s, want 16m (1..12 and 20..25 min)", got)
	}
}

// lockFileT opens path (created when missing) and holds an exclusive flock on it, as a job process can
// on its own gate-wait.json; unlock releases it (also on cleanup).
func lockFileT(t *testing.T, path string) (unlock func()) {
	t.Helper()
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX); err != nil {
		t.Fatal(err)
	}
	var once sync.Once
	unlock = func() { once.Do(func() { _ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN); f.Close() }) }
	t.Cleanup(unlock)
	return unlock
}

// acquireWithinT runs acquireGateLock(r) and fails the test when it has not returned within limit.
func acquireWithinT(t *testing.T, r gateLockReq, limit time.Duration) (*gateLease, error) {
	t.Helper()
	type res struct {
		l   *gateLease
		err error
	}
	done := make(chan res, 1)
	go func() {
		l, err := acquireGateLock(context.Background(), r)
		done <- res{l, err}
	}()
	select {
	case got := <-done:
		return got.l, got.err
	case <-time.After(limit):
		t.Fatalf("acquireGateLock still blocked after %s", limit)
		return nil, nil
	}
}

// The job's gate-wait.json is a diagnostic the job itself can lock: a lock held on it before the wait
// starts never stretches the wait past its bound (#208 review).
func TestGateWaitRecorderLockedBeforeStart(t *testing.T) {
	jobDir := t.TempDir()
	lock := filepath.Join(t.TempDir(), GateLockName)
	holdGateT(t, lock)
	lockFileT(t, filepath.Join(jobDir, GateWaitFile))
	_, err := acquireWithinT(t, gateLockReq{path: lock, wait: 300 * time.Millisecond, waits: newGateWaitRecorder(jobDir)}, 5*time.Second)
	if err == nil || !strings.Contains(err.Error(), "still held") {
		t.Fatalf("err %v, want the bounded wait's timeout", err)
	}
}

// A lock taken on gate-wait.json while the gate waits never keeps a gate that got the box lock from
// returning (it would hold the box-wide lock with no lease to release it).
func TestGateWaitRecorderLockedDuringAcquisition(t *testing.T) {
	jobDir := t.TempDir()
	lock := filepath.Join(t.TempDir(), GateLockName)
	holder := holdGateT(t, lock)
	type res struct {
		l   *gateLease
		err error
	}
	done := make(chan res, 1)
	go func() {
		l, err := acquireGateLock(context.Background(), gateLockReq{path: lock, wait: time.Minute, waits: newGateWaitRecorder(jobDir)})
		done <- res{l, err}
	}()
	wait := filepath.Join(jobDir, GateWaitFile)
	deadline := time.Now().Add(5 * time.Second)
	for b, _ := os.ReadFile(wait); !bytes.Contains(b, []byte(`"pid"`)); b, _ = os.ReadFile(wait) {
		if time.Now().After(deadline) {
			t.Fatal("the wait was never recorded")
		}
		time.Sleep(20 * time.Millisecond)
	}
	unlock := lockFileT(t, wait)
	holder.release()
	select {
	case got := <-done:
		if got.err != nil {
			t.Fatalf("acquisition: %v", got.err)
		}
		got.l.release()
	case <-time.After(5 * time.Second):
		unlock()
		t.Fatal("a locked gate-wait.json kept the gate from returning with the box lock held")
	}
}

// startMarkedSleeper is startSleeper with the job marker of job in its environment.
func startMarkedSleeper(t *testing.T, job string) int {
	t.Helper()
	cmd := exec.Command("sleep", "60")
	cmd.Env = append(os.Environ(), JobEnvVar+"="+job)
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cmd.Process.Kill(); _ = cmd.Wait() })
	return cmd.Process.Pid
}

// The holder line is in a file anyone can write (#208 review): a line naming a live pid and a victim
// job id fails the victim's gates at once only when that pid really is of the victim job (its job
// marker in /proc/<pid>/environ). A forged one is a held lock like any other: the gate waits, bounded.
func TestGateLockForgedOwnJobHolder(t *testing.T) {
	if _, err := procParent(os.Getpid()); err != nil {
		t.Skip("no /proc: the holder line decides")
	}
	lock := filepath.Join(t.TempDir(), GateLockName)
	holdGateT(t, lock)
	writeGateHolderLine(lock, fmt.Sprintf("pid %d job victim since x: forged", startSleeper(t)))
	_, err := acquireGateLock(context.Background(), gateLockReq{path: lock, wait: 300 * time.Millisecond, job: "victim"})
	if err == nil || errors.Is(err, ErrGateOwnJob) {
		t.Fatalf("forged holder line: %v; want the bounded wait's timeout, not ErrGateOwnJob", err)
	}
	writeGateHolderLine(lock, fmt.Sprintf("pid %d job victim since x: a gate the job left", startMarkedSleeper(t, "victim")))
	_, err = acquireGateLock(context.Background(), gateLockReq{path: lock, wait: 10 * time.Second, job: "victim"})
	if !errors.Is(err, ErrGateOwnJob) {
		t.Fatalf("a real gate of the job: %v; want ErrGateOwnJob", err)
	}
	// An ancestor without the marker (the job's wrapper during its acceptance step) is of the caller's job.
	writeGateHolderLine(lock, fmt.Sprintf("pid %d job victim since x: acceptance", os.Getppid()))
	_, err = acquireGateLock(context.Background(), gateLockReq{path: lock, wait: 10 * time.Second, job: "victim"})
	if !errors.Is(err, ErrGateOwnJob) {
		t.Fatalf("held by an ancestor named as the job: %v; want ErrGateOwnJob", err)
	}
}
