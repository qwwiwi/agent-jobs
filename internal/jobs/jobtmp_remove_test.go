package jobs

import (
	"bytes"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

// finishedWithTmp runs a job that fills its temp dir to its end (notified), then returns its id and
// temp dir; m.Now is moved past CleanupDelay, so the next tick runs the cleanup.
func finishedWithTmp(t *testing.T, m *Manager) (string, string) {
	t.Helper()
	notifyTo(t, m)
	j := runToEnd(t, m, enqueue(t, m, "FAKE_BIGTMP", nil).ID)
	tmp := filepath.Join(m.JobDir(j.ID), JobTmpDir)
	if _, err := os.Stat(filepath.Join(tmp, "big.bin")); err != nil {
		t.Fatalf("the job left nothing in its temp dir: %v", err)
	}
	if got := get(t, m, j.ID); got.NotifiedAt == nil {
		t.Fatal("job not notified")
	}
	real := time.Now
	m.Now = func() time.Time { return real().Add(CleanupDelay + time.Minute) }
	return j.ID, tmp
}

// #105: at the job's cleanup, once no process uses it, the temp dir is removed; the report, the logs
// and the stream stay, and the recorded folder size drops.
func TestFinishedJobTmpRemovedAtCleanup(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("the temp dir is removed only where /proc proves it unused")
	}
	m := newManager(t)
	id, tmp := finishedWithTmp(t, m)
	if err := m.Tick(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(tmp); !os.IsNotExist(err) {
		t.Fatalf("temp dir of the finished job kept: %v", err)
	}
	for _, f := range []string{"report.md", "stream.jsonl", "stderr.log"} {
		if _, err := os.Stat(filepath.Join(m.JobDir(id), f)); err != nil {
			t.Fatalf("%s removed with the temp dir: %v", f, err)
		}
	}
	j := get(t, m, id)
	// FAKE_BIGTMP writes 3 MiB to the temp dir and 3 MiB to the work dir (no repo: no worktree removal).
	if j.CleanedAt == nil || j.DirTmpBytes != 0 || j.DirBytes >= 5<<20 {
		t.Fatalf("after cleanup: cleaned %v, dir %d bytes, tmp %d", j.CleanedAt, j.DirBytes, j.DirTmpBytes)
	}
}

// #105: while a process still uses the temp dir (works in it, or runs a binary from it as a test
// daemon outside the job's session), it is kept and retried, without holding up the cleanup mark
// (prune and the log archive go by it); once the process is gone the next tick removes it.
func TestJobTmpKeptWhileProcessHoldsIt(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("the temp dir is removed only where /proc proves it unused")
	}
	for _, how := range []string{"cwd", "binary"} {
		t.Run(how, func(t *testing.T) {
			m := newManager(t)
			var logBuf bytes.Buffer
			m.Log = log.New(&logBuf, "", 0)
			id, tmp := finishedWithTmp(t, m)
			var holder *exec.Cmd
			if how == "cwd" {
				holder = exec.Command("sleep", "300")
				holder.Dir = tmp
			} else {
				holder = exec.Command(plantTmpBinary(t, m.JobDir(id)), "300")
			}
			holder.SysProcAttr = &syscall.SysProcAttr{Setsid: true} // outside the job's session
			stop := startOwned(t, holder)
			if err := m.Tick(); err != nil {
				t.Fatal(err)
			}
			if _, err := os.Stat(filepath.Join(tmp, "big.bin")); err != nil {
				t.Fatalf("temp dir removed under a live process: %v", err)
			}
			if get(t, m, id).CleanedAt == nil {
				t.Fatalf("a kept temp dir held up the cleanup mark: %s", logBuf.String())
			}
			if !strings.Contains(logBuf.String(), "still used by pids "+strconv.Itoa(holder.Process.Pid)) {
				t.Fatalf("log does not name the holder: %s", logBuf.String())
			}
			_ = stop()
			if err := m.Tick(); err != nil {
				t.Fatal(err)
			}
			if _, err := os.Lstat(tmp); !os.IsNotExist(err) {
				t.Fatalf("temp dir kept after its last user ended: %v\n%s", err, logBuf.String())
			}
			if m.tmpFails[id].n != 0 {
				t.Fatalf("failure count not reset: %+v", m.tmpFails[id])
			}
		})
	}
}

// #105: where /proc cannot prove the temp dir unused (not Linux) the cleanup completes and keeps it
// for prune; a symlink planted in its place is not followed.
func TestJobTmpKeptWithoutProc(t *testing.T) {
	m := newManager(t)
	id, tmp := finishedWithTmp(t, m)
	old := tmpRemovalGOOS
	tmpRemovalGOOS = "darwin"
	t.Cleanup(func() { tmpRemovalGOOS = old })
	if err := m.Tick(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(tmp, "big.bin")); err != nil {
		t.Fatalf("temp dir removed without /proc: %v", err)
	}
	if get(t, m, id).CleanedAt == nil {
		t.Fatal("cleanup not done")
	}

	tmpRemovalGOOS = old
	outside := t.TempDir()
	writeFile(t, filepath.Join(outside, "keep"), []byte("x"), 0o600)
	if err := os.RemoveAll(tmp); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, tmp); err != nil {
		t.Fatal(err)
	}
	if err := m.removeJobTmp(get(t, m, id)); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(outside, "keep")); err != nil {
		t.Fatalf("removal followed a symlinked temp dir: %v", err)
	}
}
