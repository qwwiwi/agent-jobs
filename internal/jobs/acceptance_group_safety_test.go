package jobs

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/qwwiwi/agent-jobs/internal/store"
)

// Linux has the same lost-outcome gap as Darwin. Check both terminal paths, including a new
// unmarked member left after the pinned leader exits, without changing Linux's signal algorithm.
func TestAcceptanceGroupFailuresRecordedOnRow(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("fake Linux process identities and pidfds")
	}
	const g = 1 << 22
	for _, path := range []string{"timeout", "orphan"} {
		for _, mode := range []string{"signal failure", "leader marker lost", "leaderless marker lost", "marker lost after TERM", "leaderless marker lost after TERM"} {
			t.Run(path+"/"+mode, func(t *testing.T) {
				m := newManager(t)
				m.KillGrace = 0
				start := time.Now()
				if path == "timeout" {
					start = start.Add(-20 * time.Minute)
				}
				j := runningRow(t, m, "261021-000000-test", start)
				if path == "orphan" {
					if err := m.Store.SetPID(j.ID, "tok", g+10, 0); err != nil {
						t.Fatal(err)
					}
				}
				// j was read before the acceptance group was recorded: kill must re-read the row.
				if err := m.Store.SetAcceptancePGID(j.ID, "tok", g); err != nil {
					t.Fatal(err)
				}
				root := t.TempDir()
				oldRoot := procRoot
				procRoot = root
				t.Cleanup(func() { procRoot = oldRoot })
				marker := j.ID
				if mode == "leader marker lost" || mode == "leaderless marker lost" {
					marker = ""
				}
				pid := g
				if strings.HasPrefix(mode, "leaderless") {
					pid++
				}
				writeFakeProc(t, root, pid, g, marker, "")
				dead := map[int]bool{}
				sent := fakeGroupPidfd(t, dead, func(pid int, sig syscall.Signal) {
					if mode == "leaderless marker lost after TERM" && sig == syscall.SIGTERM {
						writeFakeProc(t, root, pid, g, "", "")
					}
					if mode == "marker lost after TERM" && pid == g && sig == syscall.SIGTERM {
						dead[g] = true
						writeFakeProc(t, root, g, g+1, "", "")
						writeFakeProc(t, root, g+1, g, "", "")
					}
				})
				realSend := pidfdSend
				pidfdSend = func(fd int, sig syscall.Signal) error {
					if mode == "signal failure" && sig != 0 {
						return syscall.EPERM
					}
					return realSend(fd, sig)
				}
				if err := m.collect(); err != nil {
					t.Fatal(err)
				}
				got := get(t, m, j.ID)
				wantStatus := store.StatusTimeout
				if path == "orphan" {
					wantStatus = store.StatusFailed
				}
				if got.Status != wantStatus || got.AcceptancePGID != g || !strings.Contains(got.Error, "acceptance process group") {
					t.Fatalf("status %s group %d error %q", got.Status, got.AcceptancePGID, got.Error)
				}
				want := "operation not permitted"
				if mode != "signal failure" {
					want = "no marker"
				}
				if mode == "leaderless marker lost after TERM" {
					want = "group still exists after termination"
				}
				if !strings.Contains(got.Error, want) {
					t.Fatalf("row error %q lacks %q (signals %v)", got.Error, want, *sent)
				}
				if mode == "signal failure" || strings.Contains(mode, "after TERM") {
					if !strings.Contains(got.Error, ReasonSurvivors) {
						t.Fatalf("missing survivor reason: %q", got.Error)
					}
				}
			})
		}
	}
}

// The lifecycle gate is shared by Linux and Darwin and uses group liveness, not job markers or
// /proc. A gone group releases the group gate; resume still requires escaped-process verification.
func TestLiveAcceptanceGroupBlocksResumeCleanupAndPrune(t *testing.T) {
	m := newManager(t)
	j := runningRow(t, m, "261021-000000-test", time.Now())
	// This fixture has no actual wrapper launch; runningRow's placeholder child is unrelated.
	if err := m.Store.SetPID(j.ID, "tok", 0, 0); err != nil {
		t.Fatal(err)
	}
	const g = 1 << 22
	if err := m.Store.SetAcceptancePGID(j.ID, "tok", g); err != nil {
		t.Fatal(err)
	}
	if err := m.Store.Finish(j.ID, "tok", store.StatusFailed, 0, 0, 0, "wrapper died", time.Now()); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(m.JobDir(j.ID), "prompt.md"), []byte("test"), 0o600); err != nil {
		t.Fatal(err)
	}
	oldRoot, oldExists, oldKill := procRoot, groupExists, killFn
	procRoot = filepath.Join(t.TempDir(), "no-proc")
	live := true
	groupExists = func(got int) bool { return got == g && live }
	if runtime.GOOS == "darwin" {
		killFn = func(pid int, sig syscall.Signal) error {
			if pid == -g && sig == 0 {
				if live {
					return nil
				}
				return syscall.ESRCH
			}
			return oldKill(pid, sig)
		}
	}
	t.Cleanup(func() { procRoot, groupExists, killFn = oldRoot, oldExists, oldKill })
	got := get(t, m, j.ID)
	got.RepoPath = t.TempDir() // Cleanup must refuse before inspecting a worktree.
	if _, err := m.Resume(j.ID, "more", Limits{}); err == nil || !strings.Contains(err.Error(), "acceptance process group") || !strings.Contains(err.Error(), "still alive") {
		t.Fatalf("resume with a live acceptance group: %v", err)
	}
	if err := m.Cleanup(got); err == nil || !strings.Contains(err.Error(), "acceptance process group") || !strings.Contains(err.Error(), "still alive") {
		t.Fatalf("cleanup with a live acceptance group: %v", err)
	}
	if pj := m.pruneJob(got, true); !strings.Contains(pj.Skipped, "acceptance process group") || !strings.Contains(pj.Skipped, "still alive") {
		t.Fatalf("prune with a live acceptance group: %+v", pj)
	}
	live = false
	if runtime.GOOS == "linux" {
		if err := m.checkEscaped(j.ID); err == nil || !strings.Contains(err.Error(), "Linux /proc is unavailable") {
			t.Fatalf("missing /proc allowed resume: %v", err)
		}
		procRoot = oldRoot
		if err := m.checkEscaped(j.ID); err != nil {
			t.Fatalf("resume gate after group exits: %v", err)
		}
	} else if runtime.GOOS == "darwin" {
		if err := m.checkEscaped(j.ID); err != nil {
			t.Fatalf("Darwin gone groups: %v", err)
		}
	}
	got = get(t, m, j.ID)
	got.RepoPath = t.TempDir()
	if err := m.Cleanup(got); err != nil {
		t.Fatalf("cleanup after group exits: %v", err)
	}
	if got.AcceptancePGID != 0 {
		t.Fatalf("gone acceptance group still recorded: %d", got.AcceptancePGID)
	}
	live = true // An unrelated group reuses the id.
	if m.acceptanceGroupAlive(get(t, m, j.ID)) {
		t.Fatal("reused acceptance group blocked a later call")
	}
}
