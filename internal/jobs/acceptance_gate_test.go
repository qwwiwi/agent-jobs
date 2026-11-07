package jobs

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/qwwiwi/agent-jobs/internal/store"
)

// The acceptance step runs under the box-wide gate lock (#208): while another gate holds it the
// acceptance command waits, visibly (gate-wait.json, GateWaitOf: "waiting for gate lock held by ..."),
// and its start is recorded only once it holds the lock, so the wait counts neither against the
// acceptance timeout nor in its duration. While it runs the lock stays held by its wrapper.
func TestAcceptanceRunsUnderGateLock(t *testing.T) {
	m := acceptanceManager(t, "")
	lock := GateLockPath() // the tests' own (TestMain)
	holder := holdGateT(t, lock)
	writeGateHolderLine(lock, gateHolderLine("", ": the orchestrator's check.sh"))
	j := enqueue(t, m, "do it", withAcceptance(`if flock -n `+lock+` true; then echo FREE; else echo HELD; fi > ../acc-lock.txt`))
	type seen struct {
		view     *GateWaitView
		released time.Time
		err      string
	}
	ch := make(chan seen, 1)
	go func() {
		var s seen
		deadline := time.Now().Add(15 * time.Second)
		for {
			st, err := ReadGateWait(m.StateDir, j.ID)
			if err == nil && len(st.Waits) == 1 {
				if row, err := m.Store.Get(j.ID); err == nil {
					s.view = m.GateWaitOf(row)
				}
				break
			}
			if time.Now().After(deadline) {
				s.err = fmt.Sprintf("no gate lock wait recorded: %+v %v", st, err)
				break
			}
			time.Sleep(20 * time.Millisecond)
		}
		time.Sleep(1500 * time.Millisecond)
		s.released = time.Now()
		holder.release()
		ch <- s
	}()
	j = runToEnd(t, m, j.ID)
	s := <-ch
	if s.err != "" {
		t.Fatal(s.err)
	}
	if s.view == nil || !s.view.Waiting || !strings.HasPrefix(s.view.Note, "waiting for gate lock held by pid ") ||
		!strings.Contains(s.view.Note, "the orchestrator's check.sh") {
		t.Fatalf("view while waiting: %+v", s.view)
	}
	if j.Status != store.StatusSucceeded || j.AcceptanceExit == nil || *j.AcceptanceExit != 0 {
		t.Fatalf("got %s err=%q exit=%v", j.Status, j.Error, j.AcceptanceExit)
	}
	if j.AcceptanceStartedAt == nil || j.AcceptanceStartedAt.Before(s.released.Truncate(time.Second)) || j.AcceptanceDurationSec > 1 {
		t.Fatalf("acceptance started %v (lock released %v), duration %ds: the lock wait counted as acceptance time",
			j.AcceptanceStartedAt, s.released, j.AcceptanceDurationSec)
	}
	dir := m.JobDir(j.ID)
	if got := strings.TrimSpace(readFile(t, filepath.Join(dir, "acc-lock.txt"))); got != "HELD" {
		t.Fatalf("the gate lock was %s while the acceptance command ran", got)
	}
	if log := readFile(t, filepath.Join(dir, "stderr.log")); !strings.Contains(log, "acceptance: waited ") {
		t.Fatalf("stderr.log does not report the wait:\n%s", log)
	}
	if st, err := ReadGateWait(m.StateDir, j.ID); err != nil || len(st.Waits) != 0 || st.WaitedSec < 1 {
		t.Fatalf("gate-wait.json after the run: %+v %v", st, err)
	}
	if line := readFileT(t, lock); line != "" {
		t.Fatalf("the wrapper did not release the gate lock: %q", line)
	}
}

// The acceptance step never waits for a gate of its own job (one left running past the session sweep):
// it does not run, and the job fails as an acceptance that did not run, naming the holder.
func TestAcceptanceRefusesOwnJobGate(t *testing.T) {
	m := acceptanceManager(t, "")
	lock := GateLockPath()
	holdGateT(t, lock)
	j := enqueue(t, m, "do it", withAcceptance(`echo should-not-run > ../acc-ran.txt`))
	writeGateHolderLine(lock, fmt.Sprintf("pid %d job %s since x: a gate the job left", startSleeper(t), j.ID))
	j = runToEnd(t, m, j.ID)
	if j.Status != store.StatusFailed || !IsAcceptanceFailure(j.Error) {
		t.Fatalf("got %s err=%q, want an acceptance that did not run", j.Status, j.Error)
	}
	dir := m.JobDir(j.ID)
	if _, err := os.Stat(filepath.Join(dir, "acc-ran.txt")); err == nil {
		t.Fatal("the acceptance command ran while its own job's gate held the lock")
	}
	if log := readFile(t, filepath.Join(dir, "stderr.log")); !strings.Contains(log, "a gate the job left") {
		t.Fatalf("stderr.log does not name the holder:\n%s", log)
	}
}

// The daemon moves a running job's deadline by its attempt's gate lock wait (gate-wait.json), at most
// by the job's timeout; waits of an earlier attempt do not count.
func TestCreditGateWait(t *testing.T) {
	m := newManager(t)
	j := enqueue(t, m, "x", nil)
	if err := os.MkdirAll(m.JobDir(j.ID), 0o700); err != nil {
		t.Fatal(err)
	}
	now := time.Now().Truncate(time.Second)
	start := now.Add(-15 * time.Minute)
	if err := m.Store.Claim(j.ID, "tok", start); err != nil {
		t.Fatal(err)
	}
	row, err := m.Store.Get(j.ID)
	if err != nil {
		t.Fatal(err)
	}
	row.TimeoutMinutes = 10
	write := func(s GateWaitState) {
		t.Helper()
		if err := writeAtomic(filepath.Join(m.JobDir(j.ID), GateWaitFile), []byte(mustJSON(t, s))); err != nil {
			t.Fatal(err)
		}
	}
	// A finished wait of an earlier attempt and one of this attempt: 3 minutes.
	write(GateWaitState{Done: []GateWaitDone{{Since: start.Add(-time.Hour), Sec: 3000}, {Since: start.Add(time.Minute), Sec: 180}}})
	m.creditGateWait(row, now)
	if row.GateWaitSec != 180 || !jobDeadline(row).Equal(start.Add(13*time.Minute)) {
		t.Fatalf("credit %ds, deadline %v", row.GateWaitSec, jobDeadline(row))
	}
	// A current wait since the start: 15 minutes, capped at the 10 minute timeout.
	write(GateWaitState{Waits: []GateWaitEntry{{PID: os.Getpid(), Since: start, Holder: "x"}}})
	m.creditGateWait(row, now)
	if row.GateWaitSec != 600 {
		t.Fatalf("credit %ds, want the 600 s cap", row.GateWaitSec)
	}
	if got, err := m.Store.Get(j.ID); err != nil || got.GateWaitSec != 600 {
		t.Fatalf("stored credit %d %v", got.GateWaitSec, err)
	}
}

// Two jobs' heavy steps never overlap (#208, simulated): the acceptance steps of two jobs running at the
// same time and a gate a third job runs itself all take the one box-wide gate lock. Each step fails if
// another is inside (an atomic mkdir), and at least one of them had to wait.
func TestJobsHeavyStepsNeverOverlap(t *testing.T) {
	m := acceptanceManager(t, `, "max_running": 3`)
	busy := filepath.Join(t.TempDir(), "busy")
	step := "mkdir " + busy + " || exit 9; sleep 1; rmdir " + busy
	a := enqueue(t, m, "a", withAcceptance(step))
	b := enqueue(t, m, "b", withAcceptance(step))
	repo, _, head, _ := gateFixture(t)
	third := filepath.Join(m.StateDir, "jobs", "c-third")
	if err := os.MkdirAll(third, 0o700); err != nil {
		t.Fatal(err)
	}
	gate := exec.Command(selfBin, "gate", "run", "--repo", repo, "--head", head, "--", "/bin/sh", "-c", step)
	gate.Env = append(os.Environ(), JobEnvVar+"=c-third", JobDirEnvVar+"="+third)
	var gateOut strings.Builder
	gate.Stdout = &gateOut
	if err := m.Tick(); err != nil {
		t.Fatal(err)
	}
	if err := gate.Start(); err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{a.ID, b.ID} {
		waitFor(t, "exit_code", 30*time.Second, func() bool { _, ok := readExitCode(m.JobDir(id)); return ok })
	}
	if err := gate.Wait(); err != nil || lastLine(gateOut.String()) != "exit=0" {
		t.Fatalf("the third job's gate: %v, %q: a heavy step overlapped it", err, gateOut.String())
	}
	if err := m.Tick(); err != nil {
		t.Fatal(err)
	}
	var waited int64
	for _, id := range []string{a.ID, b.ID, "c-third"} {
		if id != "c-third" {
			if j := get(t, m, id); j.Status != store.StatusSucceeded || j.AcceptanceExit == nil || *j.AcceptanceExit != 0 {
				t.Fatalf("job %s: %s %q exit %v: its acceptance step overlapped another heavy step", id, j.Status, j.Error, j.AcceptanceExit)
			}
		}
		st, err := ReadGateWait(m.StateDir, id)
		if err != nil {
			t.Fatal(err)
		}
		waited += st.WaitedSec
	}
	if waited < 1 {
		t.Fatal("no heavy step waited: they did not run at the same time, so the test proves nothing")
	}
}

// The acceptance step's lock wait is bounded by what the job has left (#208 review): its timeout from the
// start plus what the gate-wait credit can still add (at most the timeout). A job near that end gives up
// the wait, naming the holder and the job's time, instead of waiting until the daemon times it out.
func TestAcceptanceLockWaitBoundedByJobTime(t *testing.T) {
	m := acceptanceManager(t, "")
	lock := GateLockPath()
	holder := holdGateT(t, lock)
	writeGateHolderLine(lock, gateHolderLine("", ": the orchestrator's check.sh"))
	j := enqueue(t, m, "FAKE_SLEEP=1", func(r *StartRequest) {
		withAcceptance(`echo ran > ../acc-ran.txt`)(r)
		r.TimeoutMinutes = 1
	})
	if err := m.Tick(); err != nil {
		t.Fatal(err)
	}
	// Started long enough ago that 2 minutes (timeout + the credit cap) less the margin leave ~6 s.
	started := time.Now().Add(-(2*time.Minute - acceptanceLockMargin - 7*time.Second))
	mustExec(t, rawDB(t, m), `UPDATE jobs SET started_at=? WHERE id=?`, started.Unix(), j.ID)
	go func() { time.Sleep(12 * time.Second); holder.release() }()
	waitFor(t, "exit_code", 20*time.Second, func() bool { _, ok := readExitCode(m.JobDir(j.ID)); return ok })
	dir := m.JobDir(j.ID)
	if _, err := os.Stat(filepath.Join(dir, "acc-ran.txt")); err == nil {
		t.Fatal("the acceptance step waited past the job's time for the gate lock and ran")
	}
	log := readFile(t, filepath.Join(dir, "stderr.log"))
	if !strings.Contains(log, "the orchestrator's check.sh") || !strings.Contains(log, "the job's time") {
		t.Fatalf("stderr.log does not name the holder and the job's time:\n%s", log)
	}
}

// A job the daemon times out while one of its heavy steps waits for the gate lock says so in its reason.
func TestTimeoutReasonNamesGateWait(t *testing.T) {
	m := newManager(t)
	noAutoContinue(m)
	j := enqueue(t, m, "FAKE_SLEEP=60", func(r *StartRequest) { r.TimeoutMinutes = 1 })
	if err := m.Tick(); err != nil {
		t.Fatal(err)
	}
	j = get(t, m, j.ID)
	st := GateWaitState{Waits: []GateWaitEntry{{PID: os.Getpid(), Since: *j.StartedAt, Holder: "pid 1 job other since x: npm run build"}}}
	if err := writeAtomic(filepath.Join(m.JobDir(j.ID), GateWaitFile), []byte(mustJSON(t, st))); err != nil {
		t.Fatal(err)
	}
	real := time.Now
	m.Now = func() time.Time { return real().Add(3 * time.Minute) }
	if err := m.Tick(); err != nil {
		t.Fatal(err)
	}
	j = get(t, m, j.ID)
	if j.Status != store.StatusTimeout || !strings.HasPrefix(j.Error, "timeout after 1 minutes") ||
		!strings.Contains(j.Error, "waiting for gate lock held by pid 1 job other since x: npm run build") {
		t.Fatalf("status %s, error %q: want a timeout naming the gate lock wait", j.Status, j.Error)
	}
}

// acceptanceLockWait: start + 2 x timeout less the margin, at most AcceptanceGateLockWait, never negative.
func TestAcceptanceLockWaitBounds(t *testing.T) {
	now := time.Unix(1770000000, 0)
	for _, c := range []struct {
		started  time.Time
		timeout  time.Duration
		want     time.Duration
		jobBound bool
	}{
		{now, 45 * time.Minute, AcceptanceGateLockWait, false},
		{now, 10 * time.Minute, 19 * time.Minute, true},
		{now.Add(-15 * time.Minute), 10 * time.Minute, 4 * time.Minute, true},
		{now.Add(-time.Hour), 10 * time.Minute, 0, true},
		{time.Time{}, 10 * time.Minute, AcceptanceGateLockWait, false},
	} {
		got, bound := acceptanceLockWait(c.started, c.timeout, now)
		if got != c.want || bound != c.jobBound {
			t.Errorf("acceptanceLockWait(%v, %s) = %s, %v; want %s, %v", c.started, c.timeout, got, bound, c.want, c.jobBound)
		}
	}
}
