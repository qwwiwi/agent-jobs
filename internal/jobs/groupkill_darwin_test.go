//go:build darwin

package jobs

import (
	"path/filepath"
	"slices"
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
	for _, mode := range []string{"leader", "leaderless", "unmarked"} {
		t.Run(mode, func(t *testing.T) {
			m := newManager(t)
			m.KillGrace = 100 * time.Millisecond
			j := runningRow(t, m, "261021-000000-test", time.Now())
			var g int
			switch mode {
			case "leader":
				g = liveChild(t, JobEnvVar+"="+j.ID).Process.Pid
			case "leaderless":
				g, _ = startOrphanGroup(t, JobEnvVar+"="+j.ID)
			case "unmarked":
				g, _ = startOrphanGroup(t)
			}
			if !groupAlive(g) {
				t.Fatal("acceptance group is already gone; nothing to test")
			}
			if err := m.Store.SetAcceptancePGID(j.ID, "tok", g); err != nil {
				t.Fatal(err)
			}
			m.killJob(j, 0, 0)
			if mode == "unmarked" {
				if !groupAlive(g) {
					t.Fatal("unmarked acceptance group was killed")
				}
			} else {
				waitFor(t, "acceptance group gone", 3*time.Second, func() bool { return !groupAlive(g) })
			}
		})
	}
}
