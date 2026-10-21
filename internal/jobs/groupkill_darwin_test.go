//go:build darwin

package jobs

import (
	"os"
	"path/filepath"
	"slices"
	"syscall"
	"testing"
	"time"

	"github.com/qwwiwi/agent-jobs/internal/store"
)

// Verify Darwin's dispatch uses sysctl even if /proc is absent. The shared signal algorithm is
// covered on Linux too; this test checks the platform's enumeration and membership callbacks.
func TestDarwinGroupIdentityWithoutProc(t *testing.T) {
	const g, member = 1 << 22, 1<<22 + 1
	const id = "261021-000000-test"
	oldRoot := procRoot
	procRoot = filepath.Join(t.TempDir(), "no-proc")
	t.Cleanup(func() { procRoot = oldRoot })
	fakeSysctl(t, func(int) (sysctlInfo, error) {
		return sysctlInfo{pgid: g, env: []string{JobEnvVar + "=" + id}}, nil
	}, func(int) ([]int, error) { return []int{member}, nil })
	got, err := groupMembers(g)
	if err != nil || !slices.Equal(got, []int{member}) {
		t.Fatalf("members %v, error %v", got, err)
	}
	if !acceptanceMember(id, g)(member) || !jobGroupMember(&store.Job{ID: id}, g)(member) {
		t.Fatal("sysctl identity refused without /proc")
	}
	if jobGroupMember(&store.Job{ID: id, ScopeUnit: "unverifiable.scope"}, g)(member) {
		t.Fatal("scope accepted without a cgroup identity check")
	}
}

// Use kernel group liveness here: /proc-based test helpers cannot tell whether a Mac child exited.
func TestDarwinKillJobEndsAcceptanceGroup(t *testing.T) {
	for _, mode := range []string{"leader", "leaderless", "leaderless ignores TERM", "mixed", "unmarked", "other job"} {
		t.Run(mode, func(t *testing.T) {
			m := newManager(t)
			m.KillGrace = 100 * time.Millisecond
			j := runningRow(t, m, "261021-000000-test", time.Now())
			var g, member int
			switch mode {
			case "leader":
				g = liveChild(t, JobEnvVar+"="+j.ID).Process.Pid
				member = g
			case "leaderless":
				g, member = startOrphanGroup(t, JobEnvVar+"="+j.ID)
			case "leaderless ignores TERM":
				ready := filepath.Join(t.TempDir(), "ignoring-term")
				g, member = startOrphanGroupScript(t, `sleep 30 >/dev/null 2>&1 & echo $!`, JobEnvVar+"="+j.ID,
					"AGENT_JOBS_TEST_SLEEP_IGNORE_TERM=1", "AGENT_JOBS_TEST_SLEEP_READY="+ready)
				waitFor(t, "child ignoring TERM", 3*time.Second, func() bool {
					_, err := os.Stat(ready)
					return err == nil
				})
			case "mixed":
				g, member = startOrphanGroupScript(t, `env -u `+JobEnvVar+` sleep 30 >/dev/null 2>&1 & sleep 30 >/dev/null 2>&1 & echo $!`, JobEnvVar+"="+j.ID)
			case "unmarked":
				g, member = startOrphanGroup(t)
			case "other job":
				g, member = startOrphanGroup(t, JobEnvVar+"=another-job")
			}
			if !groupAlive(g) {
				t.Fatal("acceptance group is already gone; nothing to test")
			}
			// The shell prints $! before its child necessarily finishes exec. Wait for
			// the fixture, rather than inspecting the restricted shell in that window.
			var info sysctlInfo
			waitFor(t, "sleep identity", 3*time.Second, func() bool {
				var err error
				info, err = sysctlProc(member)
				return err == nil && info.pgid == g && !info.zombie && len(info.argv) == 2 && filepath.Base(info.argv[0]) == "sleep"
			})
			marked := mode != "unmarked" && mode != "other job"
			if (envMarker(info.env) == j.ID) != marked {
				t.Fatalf("member marker %q, want this job: %v", envMarker(info.env), marked)
			}
			if err := m.Store.SetAcceptancePGID(j.ID, "tok", g); err != nil {
				t.Fatal(err)
			}
			var signals []syscall.Signal
			realKill := killFn
			killFn = func(pid int, sig syscall.Signal) error {
				if pid != -g {
					t.Fatalf("signalled %d, want group %d", pid, g)
				}
				signals = append(signals, sig)
				return realKill(pid, sig)
			}
			t.Cleanup(func() { killFn = realKill })
			note := m.killJob(j, 0, 0)
			if !marked {
				if note == "" || len(signals) != 0 {
					t.Fatalf("refusal note=%q signals=%v", note, signals)
				}
				if !groupAlive(g) {
					t.Fatal("unmarked acceptance group was killed")
				}
			} else {
				if note != "" {
					t.Fatalf("marked group not ended: %s", note)
				}
				waitFor(t, "acceptance group gone", 3*time.Second, func() bool { return !groupAlive(g) })
				if !slices.Contains(signals, syscall.SIGTERM) || mode == "leaderless ignores TERM" && !slices.Contains(signals, syscall.SIGKILL) {
					t.Fatalf("missing termination signal: %v", signals)
				}
			}
		})
	}
}
