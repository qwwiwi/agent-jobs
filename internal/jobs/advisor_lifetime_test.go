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

// advisorGroupGone reports that no process of group pgid is left (zombies aside).
func advisorGroupGone(pgid int) bool {
	return errors.Is(syscall.Kill(-pgid, 0), syscall.ESRCH) || gone(pgid)
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
				if !advisorGroupGone(pid) {
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
