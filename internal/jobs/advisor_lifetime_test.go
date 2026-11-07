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
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/qwwiwi/agent-jobs/internal/store"
)

// noAdvisorPolling makes the polling fallback of a running advisor call (whileJobRuns) far too slow to
// matter: what a test sees is the parent lifecycle's own termination.
func noAdvisorPolling(t *testing.T) {
	t.Helper()
	old := advisorWatchEvery
	advisorWatchEvery = time.Hour
	t.Cleanup(func() { advisorWatchEvery = old })
}

// askInBackground runs AskAdvisor for j in a goroutine and returns its result channel.
func askInBackground(e *advEnv, j *store.Job) <-chan error {
	done := make(chan error, 1)
	go func() {
		_, err := e.m.AskAdvisor(context.Background(), j, "q", "")
		done <- err
	}()
	return done
}

// An advisor call reserved while its job runs, whose job is then cancelled or timed out before the
// call starts its process, never starts it: admission re-checks the job under the advisor lock. Nothing
// ran, so the call is recorded as failed at a known zero cost.
func TestAdvisorNotStartedAfterParentEnds(t *testing.T) {
	for _, how := range []string{"cancel", "timeout"} {
		t.Run(how, func(t *testing.T) {
			e := newAdvisor(t)
			noAdvisorPolling(t)
			j := e.crewJob(t, 2, 1.0)
			paused, release := make(chan struct{}), make(chan struct{})
			e.m.beforeAdvisorLaunch = func(*store.Job) {
				close(paused)
				<-release
			}
			done := askInBackground(e, j)
			select {
			case <-paused:
			case <-time.After(10 * time.Second):
				t.Fatal("the advisor call never reached its launch")
			}
			if how == "cancel" {
				if err := e.m.Cancel(j.ID); err != nil {
					t.Fatal(err)
				}
			} else if ok, err := e.m.Store.ClaimTimeout(j.ID, j.LaunchToken, e.m.Now().Add(24*time.Hour)); err != nil || !ok {
				t.Fatalf("claim timeout: %v %v", ok, err)
			}
			close(release)
			var err error
			select {
			case err = <-done:
			case <-time.After(15 * time.Second):
				t.Fatal("the advisor call did not return")
			}
			if err == nil {
				t.Fatal("an advisor call of an ended job succeeded")
			}
			if n := e.calls(); n != 0 {
				t.Fatalf("the advisor process started %d time(s) after the job's %s", n, how)
			}
			calls, cerr := e.m.Store.AdvisorCalls(j.ID, e.m.Now(), advisorStaleAfter)
			if cerr != nil || len(calls) != 1 || calls[0].Outcome != store.AdvisorFailed || calls[0].CostUnknown || calls[0].CostUSD != 0 {
				t.Fatalf("recorded: %+v %v", calls, cerr)
			}
		})
	}
}

// parentGroup gives running job j a process group of its own (a sleeping session leader recorded as
// its spawned wrapper), so the daemon's timeout and cancel paths have something to end.
func parentGroup(t *testing.T, m *Manager, j *store.Job) {
	t.Helper()
	cmd := exec.Command("sleep", "60")
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	pid := cmd.Process.Pid
	go func() { _ = cmd.Wait() }()
	t.Cleanup(func() { _ = syscall.Kill(-pid, syscall.SIGKILL) })
	if err := m.Store.SetSpawnedPID(j.ID, j.LaunchToken, pid); err != nil {
		t.Fatal(err)
	}
}

// advisorPID waits until the fake advisor of call n records its pid.
func (e *advEnv) advisorPID(t *testing.T, n int) int {
	t.Helper()
	f := filepath.Join(e.dir, "pid."+strconv.Itoa(n))
	var pid int
	waitFor(t, "the advisor process", 15*time.Second, func() bool {
		b, err := os.ReadFile(f)
		if err != nil {
			return false
		}
		pid, err = strconv.Atoi(strings.TrimSpace(string(b)))
		return err == nil && pid > 1
	})
	return pid
}

// advisorGroupGone reports that no process of group pgid is left (zombies aside), by enumerating the
// group's members: a dead leader is no proof that its group is gone (issue #190).
func advisorGroupGone(t *testing.T, pgid int) bool {
	t.Helper()
	if errors.Is(syscall.Kill(-pgid, 0), syscall.ESRCH) {
		return true
	}
	members, err := groupMembers(pgid)
	if err != nil {
		t.Fatalf("enumerate process group %d: %v", pgid, err)
	}
	return len(members) == 0
}

// An active advisor call ends with its job's cancel or timeout through the parent lifecycle itself
// (Manager.Cancel, the timeout in Tick), not the polling fallback: its whole process group (and its
// scope, when it has one) is gone when the cancel or the tick returns, and the call is recorded as
// stopped at an unknown cost.
func TestAdvisorEndsWithParent(t *testing.T) {
	for _, contain := range []string{"session", "scope"} {
		for _, how := range []string{"cancel", "timeout"} {
			t.Run(contain+"/"+how, func(t *testing.T) {
				if contain == "scope" {
					requireUserScope(t)
				}
				e := newAdvisor(t)
				if contain == "scope" {
					t.Setenv("AGENT_JOBS_CGROUP_SCOPE", ScopeRequired)
				}
				noAdvisorPolling(t)
				noAutoContinue(e.m)
				j := e.crewJob(t, 2, 1.0)
				scopeID := fmt.Sprintf("advisor-%s-%d", j.ID, 1)
				t.Cleanup(func() {
					if contain == "scope" {
						_ = e.m.stopScope(scopeID)
					}
				})
				if how == "timeout" {
					parentGroup(t, e.m, j)
				}
				if err := os.WriteFile(filepath.Join(e.dir, "slow"), nil, 0o600); err != nil {
					t.Fatal(err)
				}
				done := askInBackground(e, j)
				pid := e.advisorPID(t, 1)
				t.Cleanup(func() { _ = syscall.Kill(-pid, syscall.SIGKILL) })
				want := store.StatusCancelled
				if how == "cancel" {
					if err := e.m.Cancel(j.ID); err != nil {
						t.Fatal(err)
					}
				} else {
					want = store.StatusTimeout
					real := e.m.Now
					e.m.Now = func() time.Time { return real().Add(3 * time.Hour) }
					if err := e.m.Tick(); err != nil {
						t.Logf("tick: %v", err)
					}
				}
				if got := get(t, e.m, j.ID); got.Status != want {
					t.Fatalf("status %s %q, want %s", got.Status, got.Error, want)
				}
				if !advisorGroupGone(t, pid) {
					t.Fatalf("the advisor's process group %d outlived the job's %s", pid, how)
				}
				if contain == "scope" {
					if st, _, err := scopeState(ScopeUnit(e.m.Agent, scopeID)); err != nil || (st != "inactive" && st != "") {
						t.Fatalf("advisor scope after the %s: %q %v", how, st, err)
					}
				}
				select {
				case err := <-done:
					if err == nil || !strings.Contains(err.Error(), "stopped") {
						t.Fatalf("advisor call of an ended job: %v", err)
					}
				case <-time.After(10 * time.Second):
					t.Fatal("the advisor call did not return after its job ended")
				}
				calls, err := e.m.Store.AdvisorCalls(j.ID, e.m.Now(), advisorStaleAfter)
				if err != nil || len(calls) != 1 || calls[0].Outcome != store.AdvisorFailed || !calls[0].CostUnknown {
					t.Fatalf("recorded: %+v %v", calls, err)
				}
				if runs, _ := filepath.Glob(filepath.Join(e.m.AdvisorDir(j.ID), "run.*.json")); len(runs) != 0 {
					t.Fatalf("advisor registrations left: %v", runs)
				}
			})
		}
	}
}

// A registration whose pid now belongs to another process (its start time differs) is dropped without
// a signal.
func TestEndAdvisorsSkipsStaleRegistration(t *testing.T) {
	if !procAvailable() {
		t.Skip("process start times need /proc")
	}
	e := newAdvisor(t)
	j := e.crewJob(t, 2, 1.0)
	cmd := exec.Command("sleep", "60")
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	pid := cmd.Process.Pid
	go func() { _ = cmd.Wait() }()
	t.Cleanup(func() { _ = syscall.Kill(-pid, syscall.SIGKILL) })
	if err := os.MkdirAll(e.m.AdvisorDir(j.ID), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := writeAdvisorRun(e.m.AdvisorDir(j.ID), advisorRun{N: 1, PID: pid, Start: 1, Token: j.LaunchToken}); err != nil {
		t.Fatal(err)
	}
	if note := e.m.endAdvisors(j.ID); note != "" {
		t.Fatalf("note: %q", note)
	}
	time.Sleep(100 * time.Millisecond)
	if gone(pid) {
		t.Fatal("a process whose start time did not match was killed")
	}
	if runs, _ := filepath.Glob(filepath.Join(e.m.AdvisorDir(j.ID), "run.*.json")); len(runs) != 0 {
		t.Fatalf("stale registration kept: %v", runs)
	}
}

// The transitions that close advisor admission (Cancel, the timeout claim, finish) are serialized with
// the advisor's admission (issue #190, F2 admission): an advisor call that passed its final liveness
// check holds the advisor lock until its process is started and registered, so a transition that comes
// in after that check waits, and then ends the call it finds registered. A transition can never complete
// between the check and the start, which would let a paid call launch after the job left running.
func TestAdvisorAdmissionSerializedWithTransition(t *testing.T) {
	for _, how := range []string{"cancel", "stop", "timeout", "finish"} {
		t.Run(how, func(t *testing.T) {
			e := newAdvisor(t)
			noAdvisorPolling(t)
			noAutoContinue(e.m)
			j := e.crewJob(t, 2, 1.0)
			// The timeout case's tick signals when it reaches its claim: the check below waits for that,
			// so a slow tick cannot make the case pass without the lock.
			claiming := make(chan struct{})
			if how == "timeout" {
				parentGroup(t, e.m, j)
				real := e.m.Now // shifted before any goroutine reads it
				e.m.Now = func() time.Time { return real().Add(3 * time.Hour) }
				var once sync.Once
				beforeTimeoutClaim = func(string) { once.Do(func() { close(claiming) }) }
				t.Cleanup(func() { beforeTimeoutClaim = func(string) {} })
			} else {
				close(claiming)
			}
			if err := os.WriteFile(filepath.Join(e.dir, "slow"), nil, 0o600); err != nil {
				t.Fatal(err)
			}
			paused, release := make(chan struct{}), make(chan struct{})
			e.m.afterAdvisorAdmit = func(*store.Job) {
				close(paused)
				<-release
			}
			done := askInBackground(e, j)
			select {
			case <-paused:
			case <-time.After(10 * time.Second):
				close(release)
				t.Fatal("the advisor call never passed its liveness check")
			}
			transition := make(chan error, 1)
			go func() {
				switch how {
				case "cancel":
					transition <- e.m.Cancel(j.ID)
				case "stop":
					_, err := e.m.Stop(j.ID, "")
					transition <- err
				case "timeout":
					transition <- e.m.Tick()
				default:
					_, _, ok := e.m.finish(j, store.StatusSucceeded, StreamResult{Runner: j.Runner, Completed: true}, false, 1, "", e.m.Now())
					if !ok {
						transition <- errors.New("finish did not record the row")
						return
					}
					transition <- nil
				}
			}()
			select {
			case <-claiming:
			case <-time.After(15 * time.Second):
				close(release)
				t.Fatal("the tick never reached its timeout claim")
			}
			crossed := false
			select {
			case <-transition:
				crossed = true
			case <-time.After(700 * time.Millisecond):
			}
			if crossed {
				close(release)
				<-done
				t.Fatalf("the %s completed between the advisor's liveness check and its start", how)
			}
			if got := get(t, e.m, j.ID); got.Status != store.StatusRunning || got.TimedOutAt != nil {
				close(release)
				t.Fatalf("the %s changed the row (%s) while an admitted advisor call was starting", how, got.Status)
			}
			close(release)
			select {
			case err := <-transition:
				if err != nil {
					t.Logf("%s: %v", how, err)
				}
			case <-time.After(30 * time.Second):
				t.Fatalf("the %s did not complete after the advisor call started", how)
			}
			if got := get(t, e.m, j.ID); got.Status == store.StatusRunning {
				t.Fatalf("the job still runs after the %s", how)
			}
			select {
			case err := <-done:
				if err == nil {
					t.Fatal("the advisor call of an ended job succeeded")
				}
			case <-time.After(15 * time.Second):
				t.Fatal("the advisor call did not end with its job")
			}
			if b, err := os.ReadFile(filepath.Join(e.dir, "pid.1")); err == nil {
				if pid, _ := strconv.Atoi(strings.TrimSpace(string(b))); pid > 1 && !advisorGroupGone(t, pid) {
					t.Fatalf("the advisor's process group %d outlived the job's %s", pid, how)
				}
			}
			if runs, _ := filepath.Glob(filepath.Join(e.m.AdvisorDir(j.ID), "run.*.json")); len(runs) != 0 {
				t.Fatalf("advisor registrations left: %v", runs)
			}
		})
	}
}

// An advisor call whose leader exits while a child of it lives on in its process group (holding the
// call's stdout) does not leave that child behind (issue #190, F2 session): the group's members are
// enumerated and ended even without the leader, and the registration is dropped only once none is
// left. "returned": the call came back before its job finished successfully; "draining": the job
// finished successfully while the call still waited for its inherited pipes, its leader already reaped;
// "escaped": the child left the call's group (a group of its own in the call's session) and the call
// came back: the call's session is enumerated too (and a crew job's successful finish sweeps what
// escaped its groups).
func TestAdvisorLeaderlessGroupEnded(t *testing.T) {
	if !procAvailable() {
		t.Skip("proving group members needs Linux /proc")
	}
	for _, when := range []string{"returned", "draining", "escaped"} {
		t.Run(when, func(t *testing.T) {
			e := newAdvisor(t)
			noAdvisorPolling(t)
			noAutoContinue(e.m)
			j := e.crewJob(t, 2, 1.0)
			marks := []string{"child"}
			if when == "escaped" {
				marks = append(marks, "escape")
			}
			for _, f := range marks {
				if err := os.WriteFile(filepath.Join(e.dir, f), nil, 0o600); err != nil {
					t.Fatal(err)
				}
			}
			done := askInBackground(e, j)
			leader := e.advisorPID(t, 1)
			t.Cleanup(func() { _ = syscall.Kill(-leader, syscall.SIGKILL) })
			var child int
			waitFor(t, "the advisor's child", 15*time.Second, func() bool {
				b, err := os.ReadFile(filepath.Join(e.dir, "child.1"))
				if err != nil {
					return false
				}
				child, err = strconv.Atoi(strings.TrimSpace(string(b)))
				return err == nil && child > 1
			})
			t.Cleanup(func() { _ = syscall.Kill(child, syscall.SIGKILL) })
			if when != "draining" {
				select {
				case <-done:
				case <-time.After(20 * time.Second):
					t.Fatal("the advisor call did not return")
				}
			} else {
				waitFor(t, "the advisor's leader to exit", 15*time.Second, func() bool { return gone(leader) })
				if runs, _ := filepath.Glob(filepath.Join(e.m.AdvisorDir(j.ID), "run.*.json")); len(runs) != 1 {
					t.Skipf("the call was no longer draining its pipes (registrations %v)", runs)
				}
			}
			if _, _, ok := e.m.finish(j, store.StatusSucceeded, StreamResult{Runner: j.Runner, Completed: true}, false, 1, "", e.m.Now()); !ok {
				t.Fatal("finish did not record the row")
			}
			if !advisorGroupGone(t, leader) {
				members, _ := groupMembers(leader)
				t.Fatalf("members %v of the advisor's process group %d outlived its job's successful finish", members, leader)
			}
			if !gone(child) {
				t.Fatalf("the advisor's child %d outlived its job's successful finish", child)
			}
			if when == "draining" {
				select {
				case <-done:
				case <-time.After(20 * time.Second):
					t.Fatal("the advisor call did not return after its job finished")
				}
			}
			if runs, _ := filepath.Glob(filepath.Join(e.m.AdvisorDir(j.ID), "run.*.json")); len(runs) != 0 {
				t.Fatalf("advisor registrations left: %v", runs)
			}
		})
	}
}

// breakScopeEnd makes every scope stop fail until the returned func restores it: systemctl fails and
// the direct cgroup sweep finds no cgroup (cgroupRoot is an empty temp dir, without the parent slice).
func breakScopeEnd(t *testing.T) (restore func()) {
	t.Helper()
	bin := filepath.Join(t.TempDir(), "systemctl")
	if err := os.WriteFile(bin, []byte("#!/bin/bash\necho 'Failed to connect to bus' >&2\nexit 1\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	oldBin, oldRoot := systemctlBin, cgroupRoot
	systemctlBin, cgroupRoot = bin, t.TempDir()
	restore = func() { systemctlBin, cgroupRoot = oldBin, oldRoot }
	t.Cleanup(restore)
	return restore
}

// An advisor call contained in a scope keeps its durable registration until its scope is proven empty
// (issue #190, F2 scope): a descendant that left the call's group and session (setsid) survives a
// failed scope stop and cgroup sweep; the registration persists, the job's successful finish is not
// recorded as a clean one, resume and prune are refused, and the daemon's tick retries the stop until
// it succeeds, which ends the descendant and only then drops the registration.
func TestAdvisorScopeSurvivorKeepsOwnership(t *testing.T) {
	requireUserScope(t)
	requireSetsid(t)
	e := newAdvisor(t)
	t.Setenv("AGENT_JOBS_CGROUP_SCOPE", ScopeRequired)
	noAdvisorPolling(t)
	noAutoContinue(e.m)
	j := e.crewJob(t, 2, 1.0)
	scopeID := fmt.Sprintf("advisor-%s-%d", j.ID, 1)
	t.Cleanup(func() { _ = e.m.stopScope(scopeID) })
	for _, f := range []string{"desc", "slow"} {
		if err := os.WriteFile(filepath.Join(e.dir, f), nil, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	done := askInBackground(e, j)
	leader := e.advisorPID(t, 1)
	t.Cleanup(func() { _ = syscall.Kill(-leader, syscall.SIGKILL) })
	var desc int
	waitFor(t, "the advisor's setsid descendant", 15*time.Second, func() bool {
		b, err := os.ReadFile(filepath.Join(e.dir, "desc.1"))
		if err != nil {
			return false
		}
		desc, err = strconv.Atoi(strings.TrimSpace(string(b)))
		return err == nil && desc > 1
	})
	t.Cleanup(func() { _ = syscall.Kill(desc, syscall.SIGKILL) })
	waitFor(t, "the descendant's own session", 5*time.Second, func() bool {
		sid, err := getsid(desc)
		return err == nil && sid == desc
	})
	restore := breakScopeEnd(t)
	if _, _, ok := e.m.finish(j, store.StatusSucceeded, StreamResult{Runner: j.Runner, Completed: true}, false, 1, "", e.m.Now()); !ok {
		t.Fatal("finish did not record the row")
	}
	select {
	case <-done:
	case <-time.After(20 * time.Second):
		t.Fatal("the advisor call did not return after its job finished")
	}
	runs := func() []string {
		r, _ := filepath.Glob(filepath.Join(e.m.AdvisorDir(j.ID), "run.*.json"))
		return r
	}
	if gone(desc) {
		t.Skip("the descendant ended without the scope stop: nothing to retry")
	}
	if len(runs()) != 1 {
		t.Fatalf("the registration of an advisor call with a live descendant was dropped: %v", runs())
	}
	got := get(t, e.m, j.ID)
	if got.Status == store.StatusSucceeded || !strings.Contains(got.Error, ReasonSurvivors) {
		t.Fatalf("a finish with an advisor survivor counted as clean: %s %q", got.Status, got.Error)
	}
	if err := os.WriteFile(filepath.Join(e.m.JobDir(j.ID), "prompt.md"), []byte("brief\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := e.m.Resume(j.ID, "go on", Limits{}); err == nil || !strings.Contains(err.Error(), "advisor") {
		t.Fatalf("resume with an advisor survivor: %v", err)
	}
	if pj, ok, err := e.m.pruneOne(j.ID, e.m.Now().Add(time.Hour), true); err != nil || !ok || pj.Skipped == "" {
		t.Fatalf("prune with an advisor survivor: %+v %v %v", pj, ok, err)
	}
	if err := e.m.Tick(); err != nil {
		t.Logf("tick: %v", err)
	}
	if len(runs()) != 1 || gone(desc) {
		t.Fatalf("a tick whose scope stop failed dropped the registration (%v) or the descendant ended", runs())
	}
	restore()
	if err := e.m.Tick(); err != nil {
		t.Logf("tick: %v", err)
	}
	waitFor(t, "the descendant to end with the retried scope stop", 10*time.Second, func() bool { return gone(desc) })
	if r := runs(); len(r) != 0 {
		t.Fatalf("the registration was kept after its scope stopped: %v", r)
	}
}
