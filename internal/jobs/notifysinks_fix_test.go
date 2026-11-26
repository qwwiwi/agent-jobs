package jobs

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/qwwiwi/agent-jobs/internal/store"
)

// waitFile waits up to d for path to exist.
func waitFile(t *testing.T, path string, d time.Duration) {
	t.Helper()
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(path); err == nil {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("%s did not appear within %s", path, d)
}

// slowSink is a notify.command that touches <dir>/started, sleeps secs and touches <dir>/done.
func slowSink(t *testing.T, secs string) (argv []string, dir string) {
	t.Helper()
	dir = t.TempDir()
	script := filepath.Join(dir, "slow.sh")
	body := "#!/bin/sh\nd=$(dirname \"$0\")\ncat >/dev/null\ntouch \"$d/started\"\nsleep " + secs + "\ntouch \"$d/done\"\n"
	if err := os.WriteFile(script, []byte(body), 0o700); err != nil {
		t.Fatal(err)
	}
	return []string{script}, dir
}

// A slow notify.command never holds the lifecycle lock: while it runs, cancel (or any lifecycle
// step) gets the lock at once.
func TestNotifyCommandRunsOutsideLifecycleLock(t *testing.T) {
	m := newManager(t)
	argv, dir := slowSink(t, "3")
	m.Cfg.Notify.Command = argv
	now := coalesceClock(m)
	finishAt(t, m, now, *now, nil)
	tickDone := make(chan error, 1)
	go func() { tickDone <- m.Tick() }()
	waitFile(t, filepath.Join(dir, "started"), 5*time.Second)
	got := make(chan error, 1)
	go func() {
		unlock, err := m.lock()
		if err == nil {
			unlock()
		}
		got <- err
	}()
	select {
	case err := <-got:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("the lifecycle lock is held while notify.command runs")
	}
	if err := <-tickDone; err != nil {
		t.Fatal(err)
	}
}

// In the daemon a tick does not wait for notify.command at all: the next tick (its tmux lines,
// its cancellations) is never held up by a slow helper.
func TestNotifyCommandDaemonTickDoesNotWait(t *testing.T) {
	m := newManager(t)
	m.daemonLockHeld = true
	argv, dir := slowSink(t, "2")
	m.Cfg.Notify.Command = argv
	now := coalesceClock(m)
	finishAt(t, m, now, *now, nil)
	start := time.Now()
	if err := m.Tick(); err != nil {
		t.Fatal(err)
	}
	if d := time.Since(start); d > 1500*time.Millisecond {
		t.Fatalf("daemon tick waited %s for notify.command", d)
	}
	waitFile(t, filepath.Join(dir, "started"), 5*time.Second)
	m.WaitNotifySinks()
	if _, err := os.Stat(filepath.Join(dir, "done")); err != nil {
		t.Fatalf("the background run was not waited for: %v", err)
	}
}

// notify.command runs at most two at a time, and two do run side by side.
func TestNotifyCommandBoundedConcurrency(t *testing.T) {
	m := newManager(t)
	dir := t.TempDir()
	script := filepath.Join(dir, "count.sh")
	body := "#!/bin/sh\nd=$(dirname \"$0\")\ncat >/dev/null\nmkdir -p \"$d/run\"\ntouch \"$d/run/$$\"\n" +
		"ls \"$d/run\" | wc -l >> \"$d/counts\"\nsleep 0.5\nls \"$d/run\" | wc -l >> \"$d/counts\"\nrm \"$d/run/$$\"\n"
	if err := os.WriteFile(script, []byte(body), 0o700); err != nil {
		t.Fatal(err)
	}
	m.Cfg.Notify.Command = []string{script}
	now := coalesceClock(m)
	for i := range 5 {
		finishAt(t, m, now, now.Add(time.Second*time.Duration(i)), func(r *StartRequest) { r.Title = fmt.Sprintf("job %d", i) })
	}
	if err := m.Tick(); err != nil {
		t.Fatal(err)
	}
	maxSeen := 0
	lines := strings.Fields(sinkFile(t, filepath.Join(dir, "counts")))
	for _, l := range lines {
		n, err := strconv.Atoi(l)
		if err != nil {
			t.Fatal(err)
		}
		maxSeen = max(maxSeen, n)
	}
	if len(lines) != 10 || maxSeen != 2 {
		t.Fatalf("runs %d (want 5), max side by side %d (want 2): %q", len(lines)/2, maxSeen, lines)
	}
}

// ownerState is the owner_pending state of job id.
func ownerState(t *testing.T, m *Manager, id string) string {
	t.Helper()
	list, _, err := m.Store.OwnerPendingList(m.Now(), time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range list {
		if p.JobID == id {
			return p.State
		}
	}
	return ""
}

// Without tmux the sinks alone carry the line: a sink that fails (a missing program) is a failed
// delivery, so an owner-waits job is not recorded as delivered; the job is still marked notified so
// its cleanup proceeds. A sink that works records the delivery.
func TestNotifySinkFailureIsNotDelivered(t *testing.T) {
	for name, tc := range map[string]struct {
		argv []string
		want string
	}{
		"missing": {[]string{filepath.Join(t.TempDir(), "no-such-binary")}, store.OwnerDelivering},
		"ok":      {[]string{"/bin/sh", "-c", "cat >/dev/null"}, store.OwnerDelivered},
	} {
		t.Run(name, func(t *testing.T) {
			m := newManager(t)
			buf := logBuffer(m)
			m.Cfg.Notify.Command = tc.argv
			now := coalesceClock(m)
			j := finishAt(t, m, now, *now, func(r *StartRequest) { r.OwnerWaits = true })
			if err := m.Tick(); err != nil {
				t.Fatal(err)
			}
			if get(t, m, j.ID).NotifiedAt == nil {
				t.Fatal("job not marked notified: its cleanup would wait forever")
			}
			if got := ownerState(t, m, j.ID); got != tc.want {
				t.Fatalf("owner state %q, want %q; log %q", got, tc.want, buf.String())
			}
			if name == "missing" && !strings.Contains(buf.String(), "notify.command failed") {
				t.Fatalf("failure not logged: %q", buf.String())
			}
		})
	}
}

// The sinks run once per batch: a retry of the same batch (the same jobs, attempts and finish
// times, here after its acknowledgement was lost) neither runs the command again nor brings back
// an inbox file that was pulled and acknowledged.
func TestNotifySinksOncePerBatch(t *testing.T) {
	m := newManager(t)
	inbox := filepath.Join(t.TempDir(), "inbox")
	argv, dir := stdinSink(t)
	m.Cfg.Notify = NotifyConfig{Command: argv, InboxDir: inbox}
	now := coalesceClock(m)
	j := finishAt(t, m, now, *now, nil)
	if err := m.Tick(); err != nil {
		t.Fatal(err)
	}
	if evs := pullAck(t, inbox); len(evs) != 1 {
		t.Fatalf("first delivery: %+v", evs)
	}
	mustExec(t, rawDB(t, m), `UPDATE jobs SET notified_at=NULL WHERE id=?`, j.ID)
	if err := m.Tick(); err != nil {
		t.Fatal(err)
	}
	if evs := pullAck(t, inbox); len(evs) != 0 {
		t.Fatalf("an acknowledged event came back: %+v", evs)
	}
	if got := strings.Count(sinkFile(t, filepath.Join(dir, "stdin")), "\n"); got != 1 {
		t.Fatalf("notify.command ran %d times for one batch", got)
	}
}

// A sink run cut off by a crash (its row claimed, never finished) is not run again: the next pass
// past the stale age records it failed.
func TestNotifySinkInterruptedNotRerun(t *testing.T) {
	m := newManager(t)
	argv, dir := stdinSink(t)
	inbox := filepath.Join(t.TempDir(), "inbox")
	m.Cfg.Notify = NotifyConfig{Command: argv, InboxDir: inbox}
	now := coalesceClock(m)
	if err := m.Store.MarkNotifiedSinks(nil, *now, nil, &store.SinkDelivery{Ref: "job-done:x@1.1", Line: "[jobs:job-done] job x"}); err != nil {
		t.Fatal(err)
	}
	claimed, err := m.Store.ClaimSinkDelivery(*now)
	if err != nil || claimed == nil {
		t.Fatalf("claim: %v %v", claimed, err)
	}
	// The process dies here. A later pass, past the stale age:
	*now = now.Add(sinkStaleAfter + time.Second)
	m.deliverSinks()
	if got := sinkFile(t, filepath.Join(dir, "stdin")); got != "" {
		t.Fatalf("an interrupted sink run was run again: %q", got)
	}
	if evs := pullAck(t, inbox); len(evs) != 0 {
		t.Fatalf("an interrupted sink run wrote the inbox later: %+v", evs)
	}
	d, err := m.Store.GetSinkDelivery("job-done:x@1.1")
	if err != nil || d.State != store.SinkFailed || !strings.Contains(d.Error, "interrupted") {
		t.Fatalf("interrupted row: %+v %v", d, err)
	}
}

// A helper that exits leaves nothing behind: its process group is killed on exit, also a background
// child that still holds its output open (which must not turn a success into a timeout) or one
// that let go of it.
func TestNotifyCommandProcessGroupKilledOnExit(t *testing.T) {
	old := NotifyCommandTimeout
	NotifyCommandTimeout = 3 * time.Second
	t.Cleanup(func() { NotifyCommandTimeout = old })
	for name, bg := range map[string]string{
		"holds output":    "sleep 30 &",
		"released output": "sleep 30 >/dev/null 2>&1 </dev/null &",
	} {
		t.Run(name, func(t *testing.T) {
			pidFile := filepath.Join(t.TempDir(), "pid")
			script := bg + "\necho $! > " + pidFile + "\necho done\nexit 0\n"
			start := time.Now()
			err := runNotifyCommand([]string{"/bin/sh", "-c", script}, "line")
			if err != nil {
				t.Fatalf("a helper that exited 0: %v", err)
			}
			if d := time.Since(start); d > 2*time.Second {
				t.Fatalf("waited %s for a background child", d)
			}
			pid, perr := strconv.Atoi(strings.TrimSpace(sinkFile(t, pidFile)))
			if perr != nil {
				t.Fatal(perr)
			}
			deadline := time.Now().Add(2 * time.Second)
			for syscall.Kill(pid, 0) == nil {
				if time.Now().After(deadline) {
					_ = syscall.Kill(pid, syscall.SIGKILL)
					t.Fatalf("background child %d outlived the helper", pid)
				}
				time.Sleep(20 * time.Millisecond)
			}
		})
	}
}

// The output a helper writes is capped in memory: a helper that prints 50 MB costs a few KB, and
// the logged error stays short.
func TestNotifyCommandOutputCapped(t *testing.T) {
	var before, after runtime.MemStats
	runtime.ReadMemStats(&before)
	err := runNotifyCommand([]string{"/bin/sh", "-c", "head -c 50000000 /dev/zero; exit 4"}, "line")
	runtime.ReadMemStats(&after)
	if err == nil || !strings.Contains(err.Error(), "exit status 4") || len(err.Error()) > 1000 {
		t.Fatalf("error of a loud failing helper: %.200v", err)
	}
	if grew := after.TotalAlloc - before.TotalAlloc; grew > 10<<20 {
		t.Fatalf("a helper's output cost %d bytes of memory", grew)
	}
}
