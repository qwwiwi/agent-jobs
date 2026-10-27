package jobs

import (
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"
)

// noStaging fails when a staging dir is left in the job dir.
func noStaging(t *testing.T, jobDir string) {
	t.Helper()
	if left, _ := filepath.Glob(filepath.Join(jobDir, tmpStagePrefix+"*")); len(left) > 0 {
		t.Fatalf("staging dir left: %v", left)
	}
}

// #105: a process that takes the temp dir after the first /proc check (before the rename, or under
// its staging name before the second check) keeps it, back under its own name; once it lets go the
// dir is removed. A staging dir an interrupted removal left is removed by the tick.
func TestJobTmpHolderBetweenScanAndDelete(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("the temp dir is removed only where /proc proves it unused")
	}
	for _, stage := range []string{"scanned", "renamed"} {
		t.Run(stage, func(t *testing.T) {
			m := newManager(t)
			id, tmp := finishedWithTmp(t, m)
			var held *os.File
			old := tmpRemovalHook
			t.Cleanup(func() { tmpRemovalHook = old })
			tmpRemovalHook = func(s, dir string) {
				if s == stage && held == nil {
					f, err := os.Open(filepath.Join(dir, "big.bin"))
					if err != nil {
						t.Errorf("open at stage %s: %v", s, err)
						return
					}
					held = f
				}
			}
			err := m.removeJobTmp(get(t, m, id))
			if held == nil {
				t.Fatalf("hook not reached at %s: %v", stage, err)
			}
			if err == nil || !strings.Contains(err.Error(), "still used by pids "+strconv.Itoa(os.Getpid())) {
				t.Fatalf("a holder that came at %s did not keep the dir: %v", stage, err)
			}
			if _, err := os.Stat(filepath.Join(tmp, "big.bin")); err != nil {
				t.Fatalf("temp dir not back under its name: %v", err)
			}
			noStaging(t, m.JobDir(id))
			_ = held.Close()
			tmpRemovalHook = old
			if err := m.removeJobTmp(get(t, m, id)); err != nil {
				t.Fatal(err)
			}
			if _, err := os.Lstat(tmp); !os.IsNotExist(err) {
				t.Fatalf("temp dir kept after its holder let go: %v", err)
			}
			noStaging(t, m.JobDir(id))
		})
	}
	t.Run("leftover staging dir", func(t *testing.T) {
		m := newManager(t)
		id, tmp := finishedWithTmp(t, m)
		if err := os.RemoveAll(tmp); err != nil {
			t.Fatal(err)
		}
		ro := filepath.Join(m.JobDir(id), tmpStagePrefix+"1", "go-build1")
		writeFile(t, filepath.Join(ro, "x"), []byte("x"), 0o600)
		if err := os.Chmod(ro, 0o555); err != nil { // read-only, as a go module cache
			t.Fatal(err)
		}
		if err := m.Tick(); err != nil {
			t.Fatal(err)
		}
		noStaging(t, m.JobDir(id))
		if _, err := os.Stat(filepath.Join(m.JobDir(id), "report.md")); err != nil {
			t.Fatal(err)
		}
	})
}

// #105: the temp dir is kept while a process whose /proc entry cannot be read fully may use it: one of
// this user (non-dumpable) or of another user while the dir is open to others. One of another user
// cannot enter a 0700 dir, and one that started before the job was created did not get the dir from
// the job: neither keeps it.
func TestJobTmpUnreadableProcessDefers(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("the temp dir is removed only where /proc proves it unused")
	}
	cases := []struct {
		name     string
		otherUID bool
		open     bool // the temp dir 0755
		predates bool // the process started before the job
		wantKept bool
	}{
		{name: "same uid", wantKept: true},
		{name: "other uid, 0700", otherUID: true},
		{name: "other uid, open dir", otherUID: true, open: true, wantKept: true},
		{name: "same uid, predates the job", predates: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			m := newManager(t)
			var proc *exec.Cmd
			start := func() {
				proc = exec.Command("sleep", "300")
				startOwned(t, proc)
			}
			if tc.predates {
				start()
				time.Sleep(3 * time.Second) // past attrSlack, a created_at kept in whole seconds and btime
			}
			id, tmp := finishedWithTmp(t, m)
			if !tc.predates {
				start()
			}
			if tc.open {
				if err := os.Chmod(tmp, 0o755); err != nil {
					t.Fatal(err)
				}
			}
			pid := strconv.Itoa(proc.Process.Pid)
			base := filepath.Join(procRoot, pid)
			old := tmpProc
			t.Cleanup(func() { tmpProc = old })
			tmpProc.readlink = func(p string) (string, error) {
				if p == filepath.Join(base, "cwd") {
					return "", &fs.PathError{Op: "readlink", Path: p, Err: fs.ErrPermission}
				}
				return old.readlink(p)
			}
			tmpProc.readFile = func(p string) ([]byte, error) {
				switch {
				case p == filepath.Join(base, "environ"):
					return nil, &fs.PathError{Op: "open", Path: p, Err: fs.ErrPermission}
				case tc.otherUID && p == filepath.Join(base, "status"):
					u := strconv.Itoa(os.Geteuid() + 1)
					return []byte("Name:\tsleep\nUid:\t" + u + "\t" + u + "\t" + u + "\t" + u + "\n"), nil
				}
				return old.readFile(p)
			}
			err := m.removeJobTmp(get(t, m, id))
			_, statErr := os.Stat(filepath.Join(tmp, "big.bin"))
			if tc.wantKept {
				if statErr != nil || err == nil || !strings.Contains(err.Error(), "cannot tell whether pids ") ||
					!strings.Contains(err.Error(), pid) {
					t.Fatalf("not kept for the unreadable pid %s: stat %v, err %v", pid, statErr, err)
				}
				return
			}
			if err != nil || !os.IsNotExist(statErr) {
				t.Fatalf("kept for pid %s: stat %v, err %v", pid, statErr, err)
			}
		})
	}
}

// #105: a job dir (or jobs root) swapped for a symlink to another folder is refused: nothing behind
// it is removed, by removeJobTmp or the tick.
func TestJobTmpSymlinkedJobDirRefused(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("the temp dir is removed only where /proc proves it unused")
	}
	for _, which := range []string{"job dir", "jobs root"} {
		t.Run(which, func(t *testing.T) {
			m := newManager(t)
			id, _ := finishedWithTmp(t, m)
			link := m.JobDir(id)
			kept := func(outside string) string { return filepath.Join(outside, JobTmpDir, "big.bin") }
			if which == "jobs root" {
				link = filepath.Dir(link)
				kept = func(outside string) string { return filepath.Join(outside, id, JobTmpDir, "big.bin") }
			}
			outside := filepath.Join(t.TempDir(), "elsewhere")
			if err := os.Rename(link, outside); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(outside, link); err != nil {
				t.Fatal(err)
			}
			if err := m.removeJobTmp(get(t, m, id)); err == nil {
				t.Fatalf("a symlinked %s was not refused", which)
			}
			_ = m.tmpCleanupDue()
			if _, err := os.Stat(kept(outside)); err != nil {
				t.Fatalf("removal followed a symlinked %s: %v", which, err)
			}
		})
	}
}
