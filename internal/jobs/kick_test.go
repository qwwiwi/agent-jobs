package jobs

import (
	"encoding/json"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/qwwiwi/agent-jobs/internal/store"
)

// writeDaemonPID writes a daemon.json naming pid into stateDir.
func writeDaemonPID(t *testing.T, stateDir string, pid int) {
	t.Helper()
	b, err := json.Marshal(DaemonInfo{Version: Version, PID: pid, StartedAt: time.Now().UTC()})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(stateDir, DaemonInfoFile), b, 0o600); err != nil {
		t.Fatal(err)
	}
}

// startFakeDaemon starts a process whose argv is `/bin/sh daemon` (a script named daemon), so it looks
// like `<bin> daemon …`; it appends a line to its kicked file on every SIGUSR1. kicks counts them.
func startFakeDaemon(t *testing.T) (pid int, kicks func() int) {
	t.Helper()
	dir := t.TempDir()
	script := "trap 'echo k >> kicked' USR1\necho ready > ready\nwhile :; do sleep 0.05; done\n"
	if err := os.WriteFile(filepath.Join(dir, "daemon"), []byte(script), 0o600); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("/bin/sh", "daemon")
	cmd.Dir = dir
	stop := startOwned(t, cmd)
	t.Cleanup(func() { _ = stop() })
	waitFor(t, "fake daemon ready", 5*time.Second, func() bool {
		_, err := os.Stat(filepath.Join(dir, "ready"))
		return err == nil
	})
	return cmd.Process.Pid, func() int {
		b, _ := os.ReadFile(filepath.Join(dir, "kicked"))
		return strings.Count(string(b), "k\n")
	}
}

func TestKickDaemonWithoutDaemonJSON(t *testing.T) {
	sent, err := KickDaemon(t.TempDir())
	if sent || err != nil {
		t.Fatalf("no daemon.json: sent %v, err %v; want a silent no-op", sent, err)
	}
}

// A kick to a dead pid is a no-op (G12).
func TestKickDaemonDeadPIDIsNoop(t *testing.T) {
	cmd := exec.Command("true")
	if err := cmd.Run(); err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	writeDaemonPID(t, dir, cmd.Process.Pid)
	sent, err := KickDaemon(dir)
	if sent || err != nil {
		t.Fatalf("dead pid: sent %v, err %v; want a silent no-op", sent, err)
	}
}

// A stale daemon.json whose pid now belongs to another process never signals it: SIGUSR1 would end it.
func TestKickDaemonSkipsOtherProcess(t *testing.T) {
	cmd := exec.Command("sleep", "30")
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cmd.Process.Kill(); _ = cmd.Wait() })
	dir := t.TempDir()
	writeDaemonPID(t, dir, cmd.Process.Pid)
	sent, err := KickDaemon(dir)
	if sent || err != nil {
		t.Fatalf("not a daemon: sent %v, err %v; want no signal", sent, err)
	}
	_ = cmd.Process.Signal(syscall.SIGTERM)
	err = cmd.Wait()
	var ee *exec.ExitError
	if !errors.As(err, &ee) {
		t.Fatalf("sleep: %v", err)
	}
	if ws, ok := ee.Sys().(syscall.WaitStatus); !ok || !ws.Signaled() || ws.Signal() != syscall.SIGTERM {
		t.Fatalf("sleep ended by %v, want SIGTERM (it must not have received SIGUSR1)", ee)
	}
}

func TestKickDaemonSignalsDaemon(t *testing.T) {
	pid, kicks := startFakeDaemon(t)
	dir := t.TempDir()
	writeDaemonPID(t, dir, pid)
	sent, err := KickDaemon(dir)
	if !sent || err != nil {
		t.Fatalf("live daemon: sent %v, err %v", sent, err)
	}
	waitFor(t, "SIGUSR1 at the daemon", 5*time.Second, func() bool { return kicks() == 1 })
}

// Enqueue kicks the daemon, so a queued job does not wait for the minute tick (G12).
func TestEnqueueKicksDaemon(t *testing.T) {
	m := newManager(t)
	pid, kicks := startFakeDaemon(t)
	writeDaemonPID(t, m.StateDir, pid)
	enqueue(t, m, "FAKE_SLEEP=1", nil)
	waitFor(t, "kick from Enqueue", 5*time.Second, func() bool { return kicks() == 1 })
}

// The wrapper kicks the daemon once its completion is stored, so the freed slot is refilled at once (G12).
func TestWrapperKicksDaemonAfterCompletion(t *testing.T) {
	m := newManager(t)
	pid, kicks := startFakeDaemon(t)
	j := enqueue(t, m, "FAKE_SLEEP=2", nil)
	if err := m.Tick(); err != nil { // a manual tick refuses to run while daemon.json names a live pid
		t.Fatal(err)
	}
	writeDaemonPID(t, m.StateDir, pid) // after Enqueue and the launch: only the wrapper's kick counts
	waitFor(t, "kick from the wrapper", 15*time.Second, func() bool { return kicks() >= 1 })
	if _, done := m.completedCode(get(t, m, j.ID)); !done {
		t.Fatal("the wrapper kicked before its completion was stored")
	}
	// The kick is the wrapper's last step: collecting the job may stop its scope and so end the wrapper.
	if _, ok := readExitCode(m.JobDir(j.ID)); !ok {
		t.Fatal("the wrapper kicked before it wrote exit_code")
	}
}

// End to end with the real daemon: a queued job starts within 3 s after a slot frees, not at the next
// minute tick (G12).
func TestDaemonStartsQueuedJobOnKick(t *testing.T) {
	m := newManager(t)
	if err := os.WriteFile(filepath.Join(m.StateDir, "config.json"), []byte(`{"max_running": 1}`), 0o600); err != nil {
		t.Fatal(err)
	}
	d := exec.Command(selfBin, "daemon", "--agent", "test")
	d.Stderr = io.Discard
	stopDaemon := startOwned(t, d)
	t.Cleanup(func() { _ = stopDaemon() })
	waitFor(t, "daemon.json of the daemon", 10*time.Second, func() bool {
		info, err := m.ReadDaemonInfo()
		return err == nil && info.PID == d.Process.Pid
	})
	time.Sleep(1500 * time.Millisecond) // past the first tick's debounce window
	a := enqueue(t, m, "FAKE_SLEEP=2", nil)
	waitFor(t, "first job started by the Enqueue kick", 3*time.Second, func() bool {
		return get(t, m, a.ID).Status == store.StatusRunning
	})
	b := enqueue(t, m, "FAKE_SLEEP=1", nil)
	waitFor(t, "first job's completion stored", 20*time.Second, func() bool {
		_, done := m.completedCode(get(t, m, a.ID))
		return done
	})
	waitFor(t, "second job started after the slot freed", 3*time.Second, func() bool {
		return get(t, m, b.ID).Status == store.StatusRunning || get(t, m, b.ID).Status == store.StatusSucceeded
	})
	if s := get(t, m, a.ID).Status; s != store.StatusSucceeded {
		t.Fatalf("first job %s, want succeeded", s)
	}
}
