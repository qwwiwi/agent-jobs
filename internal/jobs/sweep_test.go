package jobs

import (
	"bytes"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
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
	id := enqueue(t, m, "FAKE_SURVIVOR", nil).ID
	if err := m.Tick(); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "exit_code", 15*time.Second, func() bool {
		_, ok := readExitCode(m.JobDir(id))
		return ok
	})
	// Checked before the daemon collects the job: its sweep of a failed job (no test hook in this
	// process) then ends the survivor.
	survivor := 0
	if b, err := os.ReadFile(filepath.Join(m.JobDir(id), "survivor.pid")); err == nil {
		if _, err := fmt.Sscan(strings.TrimSpace(string(b)), &survivor); err == nil && survivor > 1 {
			t.Cleanup(func() { _ = syscall.Kill(survivor, syscall.SIGKILL) })
			if !alive(survivor) {
				t.Fatalf("survivor %d is gone: the hook did not make it unkillable", survivor)
			}
		}
	}
	if err := m.Tick(); err != nil {
		t.Fatal(err)
	}
	j := get(t, m, id)
	if survivor > 1 {
		waitFor(t, "survivor swept by the daemon", 10*time.Second, func() bool { return !procAlive(survivor) })
	}
	if j.Status != store.StatusFailed || !strings.HasPrefix(j.Error, ReasonSurvivors) {
		t.Fatalf("job: %s %q", j.Status, j.Error)
	}
	if _, err := os.Stat(filepath.Join(m.JobDir(j.ID), "exit_code")); err != nil {
		t.Fatalf("exit_code missing: %v", err)
	}
	if line := DoneLine(j, 0); !strings.HasSuffix(line, " · "+ReasonSurvivors) {
		t.Fatalf("job-done line: %q", line)
	}
}

// Off Linux (selected here on every OS), the sweep inside the manifest step neither ignores SIGTERM nor
// signals its own group: the step's stop-signal catch stays armed, so a SIGTERM after the sweep is a
// stop (not missed) and the runner does not inherit SIGTERM ignored. The wrapper's other sweeps still
// ignore SIGTERM and send it to the group.
func TestSweepOffLinuxKeepsTheManifestStepStopCatch(t *testing.T) {
	oldIgnore, oldKill, oldSid, oldGOOS := signalIgnore, killGroup, getsid, sweepGOOS
	t.Cleanup(func() {
		signalIgnore, killGroup, getsid, sweepGOOS = oldIgnore, oldKill, oldSid, oldGOOS
		signal.Reset(syscall.SIGTERM)
	})
	sweepGOOS = "darwin"
	getsid = func(int) (int, error) { return os.Getpid(), nil } // as the wrapper: the session leader
	var killed []int
	killGroup = func(pid int, _ syscall.Signal) error {
		killed = append(killed, pid) // never the real group of the test binary
		return nil
	}
	ctx, release := catchStopSignals()
	if r := sweepJob(t.TempDir(), io.Discard, true); r.Incomplete() || len(killed) != 0 {
		release()
		t.Fatalf("sweep in the step: %+v, signalled groups %v", r, killed)
	}
	if ctx.Err() != nil {
		release()
		t.Fatal("the sweep itself was caught as a stop signal")
	}
	if err := syscall.Kill(os.Getpid(), syscall.SIGTERM); err != nil {
		release()
		t.Fatal(err)
	}
	select {
	case <-ctx.Done():
	case <-time.After(5 * time.Second):
		release()
		t.Fatal("a SIGTERM after the step's sweep was missed: SIGTERM is ignored")
	}
	if !release() {
		t.Fatal("release does not report the caught stop signal")
	}

	var ignored []os.Signal
	signalIgnore = func(sig ...os.Signal) { ignored = append(ignored, sig...) }
	if r := sweepSession(io.Discard, false); r.Incomplete() || !slices.Equal(ignored, []os.Signal{syscall.SIGTERM}) ||
		!slices.Equal(killed, []int{-os.Getpid()}) {
		t.Fatalf("last sweep: %+v, ignored %v, signalled groups %v", r, ignored, killed)
	}
}
