package jobs

import (
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"syscall"
	"testing"
	"time"

	"golang.org/x/sys/unix"

	"github.com/qwwiwi/agent-jobs/internal/store"
)

// fakeProc points procRoot at a temp dir and returns a writer of /proc/<pid>/stat entries.
func fakeProc(t *testing.T) func(pid, sid int) {
	t.Helper()
	root := t.TempDir()
	old := procRoot
	procRoot = root
	t.Cleanup(func() { procRoot = old })
	return func(pid, sid int) {
		dir := filepath.Join(root, fmt.Sprint(pid))
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		// pid (comm) state ppid pgrp session …
		line := fmt.Sprintf("%d (sleep) S 1 %d %d 0 -1\n", pid, sid, sid)
		if err := os.WriteFile(filepath.Join(dir, "stat"), []byte(line), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

// fakePidfd stands in for the kernel calls: open hands out a real (harmless) descriptor after
// running onOpen, send records which pid was signalled with what, and does not deliver anything.
func fakePidfd(t *testing.T, onOpen func(pid int) error) *[]string {
	t.Helper()
	var sent []string
	byFD := map[int]int{}
	oldOpen, oldSend, oldGrace := pidfdOpen, pidfdSend, SweepGrace
	t.Cleanup(func() { pidfdOpen, pidfdSend, SweepGrace = oldOpen, oldSend, oldGrace })
	SweepGrace = 10 * time.Millisecond
	pidfdOpen = func(pid int) (int, error) {
		if onOpen != nil {
			if err := onOpen(pid); err != nil {
				return -1, err
			}
		}
		fd, err := unix.Open(os.DevNull, unix.O_RDONLY|unix.O_CLOEXEC, 0)
		if err == nil {
			byFD[fd] = pid
		}
		return fd, err
	}
	pidfdSend = func(fd int, sig syscall.Signal) error {
		sent = append(sent, fmt.Sprintf("%d:%d", byFD[fd], sig))
		return nil
	}
	return &sent
}

const testSID = 4000

// PID reuse: the member list saw pid 4242 in our session, but by the time the descriptor is open the
// pid belongs to an unrelated process in another session. It must never be signalled.
func TestSweepNeverSignalsAReusedPid(t *testing.T) {
	write := fakeProc(t)
	write(4242, testSID)
	sent := fakePidfd(t, func(pid int) error {
		write(pid, 777) // reused just before pidfd_open: now a stranger's process
		return nil
	})
	var log bytes.Buffer
	r := sweepSid(testSID, &log)
	if len(*sent) != 0 {
		t.Fatalf("a reused pid was signalled: %v", *sent)
	}
	if r.Incomplete() || r.Signalled != 0 {
		t.Fatalf("result %+v", r)
	}
	// Control: the same pid still in our session is signalled (SIGTERM, then SIGKILL).
	write(4242, testSID)
	sent = fakePidfd(t, nil)
	r = sweepSid(testSID, &log)
	if len(*sent) == 0 || (*sent)[0] != fmt.Sprintf("4242:%d", syscall.SIGTERM) {
		t.Fatalf("member not signalled: %v", *sent)
	}
	if !slices.Contains(*sent, fmt.Sprintf("4242:%d", syscall.SIGKILL)) || !slices.Equal(r.Survivors, []int{4242}) {
		t.Fatalf("sent %v, result %+v", *sent, r)
	}
}

// No pidfd (old kernel): nothing is signalled at all, the members are survivors, the log says why.
func TestSweepWithoutPidfdSignalsNothingAndFailsClosed(t *testing.T) {
	write := fakeProc(t)
	write(4242, testSID)
	sent := fakePidfd(t, func(int) error { return unix.ENOSYS })
	var log bytes.Buffer
	r := sweepSid(testSID, &log)
	if len(*sent) != 0 {
		t.Fatalf("signalled without a pidfd: %v", *sent)
	}
	if !r.Incomplete() || !slices.Equal(r.Survivors, []int{4242}) || !strings.Contains(log.String(), "pidfd unsupported") {
		t.Fatalf("result %+v log %q", r, log.String())
	}
}

// A session that cannot be enumerated is not a clean sweep.
func TestSweepEnumerationErrorIsIncomplete(t *testing.T) {
	old := procRoot
	procRoot = filepath.Join(t.TempDir(), "missing")
	t.Cleanup(func() { procRoot = old })
	if r := sweepSid(testSID, &bytes.Buffer{}); !r.Incomplete() || r.Err == nil {
		t.Fatalf("result %+v", r)
	}
}

// The marker is read safely: a FIFO in its place neither hangs the daemon nor passes as clean.
func TestSweepFailureReadsMarkerSafely(t *testing.T) {
	dir := t.TempDir()
	if got := sweepFailure(dir); got != "" {
		t.Fatalf("no marker: %q", got)
	}
	if err := writeSweepMarker(dir, SweepResult{Survivors: []int{12, 34}}); err != nil {
		t.Fatal(err)
	}
	if got := sweepFailure(dir); got != ReasonSurvivors+": pids 12, 34" {
		t.Fatalf("marker: %q", got)
	}
	_ = os.Remove(filepath.Join(dir, SweepMarker))
	if err := syscall.Mkfifo(filepath.Join(dir, SweepMarker), 0o600); err != nil {
		t.Fatal(err)
	}
	done := make(chan string, 1)
	go func() { done <- sweepFailure(dir) }()
	select {
	case got := <-done:
		if !strings.HasPrefix(got, ReasonSurvivors) {
			t.Fatalf("fifo marker: %q", got)
		}
	case <-time.After(3 * time.Second):
		f, _ := os.OpenFile(filepath.Join(dir, SweepMarker), os.O_WRONLY, 0) // unblock the reader
		if f != nil {
			f.Close()
		}
		t.Fatal("sweepFailure hung on a FIFO")
	}
}

// End to end with the real wrapper: a leftover that survives every round (made unkillable through
// the test-only hook) fails the job with ReasonSurvivors, and the job-done line says so.
func TestWrapperFailsJobWhenProcessesSurvive(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("session sweep reads /proc (Linux)")
	}
	hooked := filepath.Join(t.TempDir(), "agent-jobs")
	if out, err := exec.Command("go", "build", "-tags", "agentjobs_testhooks", "-o", hooked, "../../cmd/agent-jobs").CombinedOutput(); err != nil {
		t.Fatalf("build: %v\n%s", err, out)
	}
	t.Setenv("AGENT_JOBS_TEST_UNKILLABLE", "1")
	t.Setenv("AGENT_JOBS_TEST_SWEEP_GRACE_MS", "20")
	m := newManager(t)
	m.SelfBin = hooked
	j := runToEnd(t, m, enqueue(t, m, "FAKE_SURVIVOR", nil).ID)
	if b, err := os.ReadFile(filepath.Join(m.JobDir(j.ID), "survivor.pid")); err == nil {
		var pid int
		if _, err := fmt.Sscan(strings.TrimSpace(string(b)), &pid); err == nil && pid > 1 {
			t.Cleanup(func() { _ = syscall.Kill(pid, syscall.SIGKILL) })
			if !alive(pid) {
				t.Fatalf("survivor %d is gone: the hook did not make it unkillable", pid)
			}
		}
	}
	if j.Status != store.StatusFailed || !strings.HasPrefix(j.Error, ReasonSurvivors) {
		t.Fatalf("job: %s %q", j.Status, j.Error)
	}
	if _, err := os.Stat(filepath.Join(m.JobDir(j.ID), "exit_code")); err != nil {
		t.Fatalf("exit_code missing: %v", err)
	}
	if line := DoneLine(j); !strings.HasSuffix(line, " · "+ReasonSurvivors) {
		t.Fatalf("job-done line: %q", line)
	}
}
