package jobs

import (
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/qwwiwi/agent-jobs/internal/store"
)

// recorded sets the sizes recorded on a job's row.
func recorded(t *testing.T, m *Manager, id string, bytes, tmp int64, partial bool) {
	t.Helper()
	if err := m.Store.SetDirBytes(id, bytes, tmp, partial); err != nil {
		t.Fatal(err)
	}
}

// #240 High (c): the disk status must not wait for the lifecycle lock (a long prune or tick holds it).
func TestDiskDoesNotTakeLifecycleLock(t *testing.T) {
	m := newManager(t)
	tmpJob(t, m, "done", store.StatusSucceeded, 3*time.Hour, nil)
	f, err := os.OpenFile(filepath.Join(m.StateDir, "tick.lock"), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX); err != nil {
		t.Fatal(err)
	}
	got := make(chan DiskStatus, 1)
	go func() { got <- m.Disk() }()
	select {
	case d := <-got:
		if d.Error != "" {
			t.Fatalf("disk error: %s", d.Error)
		}
	case <-time.After(time.Second):
		_ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN) // let the goroutine end
		<-got
		t.Fatal("Disk() waited for the lifecycle lock")
	}
}

// The disk status opens no directory of a finished job: an unreadable temp dir changes nothing, the
// recorded size is reported.
func TestDiskDoesNotWalkFinishedJobs(t *testing.T) {
	m := newManager(t)
	dir := tmpJob(t, m, "done", store.StatusSucceeded, 3*time.Hour, nil)
	recorded(t, m, "done", 6<<30, 5<<30, false)
	tmp := filepath.Join(dir, JobTmpDir)
	if err := os.Chmod(tmp, 0); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(tmp, 0o700) })
	d := m.Disk()
	if d.Error != "" || d.PrunableBytes != 5<<30 || d.JobTmpBytes != 5<<30 {
		t.Fatalf("disk = %+v, want the recorded 5 GiB and no error", d)
	}
}

// Prunable bytes are an estimate from the rows: the recorded temp dir of a job past the temp
// retention, the recorded folder of a job past 48 h that is not archived yet; nothing for a younger
// job, a stopped one or an archived one.
func TestDiskPrunableFromRecordedSizes(t *testing.T) {
	m := newManager(t)
	for _, c := range []struct {
		id, status string
		ago        time.Duration
		bytes, tmp int64
	}{
		{"young", store.StatusSucceeded, time.Hour, 1000, 100},         // nothing
		{"tmp", store.StatusFailed, 3 * time.Hour, 2000, 200},          // 200
		{"stopped", store.StatusStopped, 3 * time.Hour, 4000, 400},     // nothing
		{"full", store.StatusSucceeded, 50 * time.Hour, 8000, 800},     // 8000
		{"archived", store.StatusSucceeded, 50 * time.Hour, 16000, 0},  // nothing
		{"nofinish", store.StatusRunning, 50 * time.Hour, 32000, 3200}, // measured, not prunable
	} {
		fin := m.Now().Add(-c.ago)
		j := insertJob(t, m, c.id, c.status, &fin, nil)
		recorded(t, m, c.id, c.bytes, c.tmp, false)
		if c.id == "archived" {
			dest, _ := m.existingArchiveDir(j)
			writeFile(t, filepath.Join(dest, "report.md"), []byte("r"), 0o600)
		}
	}
	if d := m.Disk(); d.PrunableBytes != 8200 {
		t.Fatalf("prunable %d, want 8200 (200 of the temp dir + 8000 of the folder)", d.PrunableBytes)
	}
}

// A finished job that still records a temp dir well past the retention is evidence that the temp dir
// pass skips or fails for it: counted, and named in prune_error.
func TestDiskReportsOverdueTmp(t *testing.T) {
	m := newManager(t)
	for id, ago := range map[string]time.Duration{
		"due":     2*time.Hour + TmpPruneInterval, // the next pass takes it: not overdue
		"overdue": 2*time.Hour + 4*TmpPruneInterval,
		"oldest":  5 * time.Hour,
		"clean":   6 * time.Hour,
	} {
		fin := m.Now().Add(-ago)
		insertJob(t, m, id, store.StatusFailed, &fin, nil)
		tmp := int64(4096)
		if id == "clean" {
			tmp = 0
		}
		recorded(t, m, id, 8192, tmp, false)
	}
	d := m.Disk()
	if d.PruneOverdue != 2 || !strings.Contains(d.PruneError, "oldest") || strings.Contains(d.PruneError, "clean") {
		t.Fatalf("overdue %d, prune_error %q; want 2 naming the job oldest", d.PruneOverdue, d.PruneError)
	}
}

// #240 Low: a recorded partial size of a finished job makes the totals lower bounds too.
func TestDiskAggregatesRecordedPartial(t *testing.T) {
	m := newManager(t)
	fin := m.Now().Add(-time.Hour)
	insertJob(t, m, "p", store.StatusSucceeded, &fin, nil)
	recorded(t, m, "p", 1000, 100, false)
	if m.Disk().JobDirsPartial {
		t.Fatal("partial without a partial row")
	}
	recorded(t, m, "p", 1000, 100, true)
	if !m.Disk().JobDirsPartial {
		t.Fatal("a finished job's recorded partial size did not reach job_dirs_partial")
	}
}

// The dry run measures with the bounded walk: over a temp dir larger than the bound it returns a
// lower bound and says so.
func TestPruneDryRunBoundedWalk(t *testing.T) {
	m := newManager(t)
	dir := tmpJob(t, m, "big", store.StatusSucceeded, 3*time.Hour, nil)
	for i := range 1000 {
		writeFile(t, filepath.Join(dir, JobTmpDir, "many", "f"+strings.Repeat("x", i%7)+string(rune('a'+i%26))+
			string(rune('a'+i/26%26))+string(rune('a'+i/676))), []byte("x"), 0o600)
	}
	oldN := dirWalkMaxEntries
	dirWalkMaxEntries = 10
	t.Cleanup(func() { dirWalkMaxEntries = oldN })
	rep, err := m.Prune(true)
	if err != nil {
		t.Fatal(err)
	}
	if !rep.Partial || len(rep.Jobs) != 1 || len(rep.Jobs[0].Removed) != 1 || !rep.Jobs[0].Removed[0].Partial {
		t.Fatalf("report = %+v, want a partial measurement", rep)
	}
	dirWalkMaxEntries = oldN
	rep, err = m.Prune(true)
	if err != nil || rep.Partial || rep.Bytes < 1000+4096 {
		t.Fatalf("unbounded: err %v partial %v bytes %d", err, rep.Partial, rep.Bytes)
	}
}
