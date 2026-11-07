package jobs

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/qwwiwi/agent-jobs/internal/store"
)

// injectStatFaults routes procStatFull's reads through a fault table until the test ends. The returned
// func makes the read of /proc/<pid>/stat (under the current procRoot) fail with err, or read normally
// again when err is nil. Safe to call while other goroutines read.
func injectStatFaults(t *testing.T) func(pid int, err error) {
	t.Helper()
	var mu sync.Mutex
	faults := map[string]error{}
	old := procStatRead
	procStatRead = func(name string) ([]byte, error) {
		mu.Lock()
		err, ok := faults[name]
		mu.Unlock()
		if ok {
			return nil, &fs.PathError{Op: "open", Path: name, Err: err}
		}
		return old(name)
	}
	t.Cleanup(func() { procStatRead = old })
	return func(pid int, err error) {
		name := filepath.Join(procRoot, strconv.Itoa(pid), "stat")
		mu.Lock()
		defer mu.Unlock()
		if err == nil {
			delete(faults, name)
			return
		}
		faults[name] = err
	}
}

// Issue #190 (durable ownership): a process of a registered advisor call's group or session whose
// /proc stat cannot be read for another reason than its disappearance (descriptors exhausted, access
// denied) is never taken for gone: the enumeration fails and says why, so the registration is kept.
// Only ENOENT and ESRCH (the process is gone) drop a candidate; once the reads succeed again the
// member is found.
func TestAdvisorMembersStatReadFailureIsNotDisappearance(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("Linux /proc enumeration with a fake /proc")
	}
	root := t.TempDir()
	old := procRoot
	procRoot = root
	t.Cleanup(func() { procRoot = old })
	const id = "261107-000000-abcd"
	// The call's leader (4241) has exited; its markerless child (4242) is left in its group and session.
	const leader, child = 4241, 4242
	writeFakeProc(t, root, child, leader, "", "")
	r := advisorRun{N: 1, PID: leader, Start: 12000}
	fault := injectStatFaults(t)
	for _, tc := range []struct {
		name    string
		err     error
		failure bool
	}{
		{"EMFILE", syscall.EMFILE, true},
		{"EACCES", syscall.EACCES, true},
		{"ENOENT", syscall.ENOENT, false},
		{"ESRCH", syscall.ESRCH, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fault(child, tc.err)
			defer fault(child, nil)
			proven, unproven, reused, err := advisorMembers(id, r)
			if tc.failure {
				if err == nil || !errors.Is(err, tc.err) {
					t.Fatalf("a member whose stat read failed (%v) was dropped as gone: proven %v unproven %v reused %v err %v",
						tc.err, proven, unproven, reused, err)
				}
				return
			}
			if err != nil || reused || len(proven)+len(unproven) != 0 {
				t.Fatalf("a vanished process (%v) was not taken for gone: proven %v unproven %v reused %v err %v",
					tc.err, proven, unproven, reused, err)
			}
		})
	}
	proven, unproven, reused, err := advisorMembers(id, r)
	if err != nil || reused || len(proven) != 0 || !slices.Equal(unproven, []int{child}) {
		t.Fatalf("with reads succeeding: proven %v unproven %v reused %v err %v", proven, unproven, reused, err)
	}
}

// Issue #190 (durable ownership): a leaderless child of an advisor call, successfully enumerated but
// whose /proc stat then cannot be read (EMFILE), is neither signalled nor taken for gone: the call's
// registration is kept with the reason in the job's error, the finish is not a clean one, resume and
// prune are refused and the tick keeps it while the reads fail. Once they succeed again, the tick ends
// a child proven the call's (marked) and drops the registration; a markerless one stays owned (never
// signalled) until it ends by itself.
func TestAdvisorStatReadFailureKeepsOwnership(t *testing.T) {
	if !procAvailable() {
		t.Skip("proving group members needs Linux /proc")
	}
	for _, kind := range []string{"marked", "markerless"} {
		t.Run(kind, func(t *testing.T) {
			e := newAdvisor(t)
			noAdvisorPolling(t)
			noAutoContinue(e.m)
			fault := injectStatFaults(t)
			j := e.crewJob(t, 2, 1.0)
			mark := "child"
			if kind == "markerless" {
				mark = "bare"
			}
			if err := os.WriteFile(filepath.Join(e.dir, mark), nil, 0o600); err != nil {
				t.Fatal(err)
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
			// The call's leader exits; its child holds its stdout, so the call drops its registration
			// only after its wait delay: by then the child's stat cannot be read.
			fault(child, syscall.EMFILE)
			select {
			case <-done:
			case <-time.After(20 * time.Second):
				t.Fatal("the advisor call did not return")
			}
			runs := func() []string {
				r, _ := filepath.Glob(filepath.Join(e.m.AdvisorDir(j.ID), "run.*.json"))
				return r
			}
			if gone(child) {
				t.Fatal("a child whose stat could not be read was signalled")
			}
			if len(runs()) != 1 {
				t.Fatalf("the registration was dropped while a child's stat could not be read: %v", runs())
			}
			if _, _, ok := e.m.finish(j, store.StatusSucceeded, StreamResult{Runner: j.Runner, Completed: true}, false, 1, "", e.m.Now()); !ok {
				t.Fatal("finish did not record the row")
			}
			got := get(t, e.m, j.ID)
			if got.Status == store.StatusSucceeded || !strings.Contains(got.Error, ReasonSurvivors) ||
				!strings.Contains(got.Error, "not checked") || !strings.Contains(got.Error, "too many open files") {
				t.Fatalf("a finish whose advisor child could not be inspected: %s %q", got.Status, got.Error)
			}
			if gone(child) || len(runs()) != 1 {
				t.Fatalf("the finish signalled the uninspected child or dropped its registration (%v)", runs())
			}
			if err := os.WriteFile(filepath.Join(e.m.JobDir(j.ID), "prompt.md"), []byte("brief\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			if _, err := e.m.Resume(j.ID, "go on", Limits{}); err == nil || !strings.Contains(err.Error(), "advisor") {
				t.Fatalf("resume while an advisor child could not be inspected: %v", err)
			}
			if pj, ok, err := e.m.pruneOne(j.ID, e.m.Now().Add(time.Hour), true); err != nil || !ok || pj.Skipped == "" {
				t.Fatalf("prune while an advisor child could not be inspected: %+v %v %v", pj, ok, err)
			}
			if err := e.m.Tick(); err != nil {
				t.Logf("tick: %v", err)
			}
			if gone(child) || len(runs()) != 1 {
				t.Fatalf("a tick while the reads fail signalled the child or dropped its registration (%v)", runs())
			}
			// The reads succeed again.
			fault(child, nil)
			if err := e.m.Tick(); err != nil {
				t.Logf("tick: %v", err)
			}
			if kind == "marked" {
				waitFor(t, "the marked child to end with the retried end", 10*time.Second, func() bool { return gone(child) })
				if r := runs(); len(r) != 0 {
					t.Fatalf("the registration was kept after its call ended: %v", r)
				}
				return
			}
			if gone(child) || len(runs()) != 1 {
				t.Fatalf("the markerless child was signalled or its registration dropped (%v)", runs())
			}
			_ = syscall.Kill(child, syscall.SIGKILL)
			waitFor(t, "the markerless child to end", 10*time.Second, func() bool { return gone(child) })
			if err := e.m.Tick(); err != nil {
				t.Logf("tick: %v", err)
			}
			if r := runs(); len(r) != 0 {
				t.Fatalf("the registration was kept after nothing of the call was left: %v", r)
			}
		})
	}
}
