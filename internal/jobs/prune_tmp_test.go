package jobs

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/qwwiwi/agent-jobs/internal/store"
)

// tmpJob adds a job that ended `ago` ago with a temp dir, a work dir and a log in its folder.
func tmpJob(t *testing.T, m *Manager, id, status string, ago time.Duration, mod func(*store.Job)) string {
	t.Helper()
	fin := m.Now().Add(-ago)
	insertJob(t, m, id, status, &fin, mod)
	dir := m.JobDir(id)
	writeBytes(t, filepath.Join(dir, JobTmpDir, "go-build1", "big.bin"), 4096)
	writeFile(t, filepath.Join(dir, "work", "out.txt"), []byte("result\n"), 0o600)
	writeFile(t, filepath.Join(dir, "stream.jsonl"), []byte("{}\n"), 0o600)
	writeFile(t, filepath.Join(dir, "report.md"), []byte("# report\n"), 0o600)
	return dir
}

func exists(p string) bool {
	_, err := os.Lstat(p)
	return err == nil
}

// #240: prune removes the temp dir of a job that finished more than job_tmp_retention_hours ago
// (default 2) long before the folder's own 48 h; everything else in the folder stays, nothing is
// archived (the job stays resumable). A younger job, a stopped job and a job with a live process
// group keep their temp dir.
func TestPruneRemovesTmpAfterShortRetention(t *testing.T) {
	m := newManager(t)
	now := time.Now()
	m.Now = func() time.Time { return now }
	const pgid = 1 << 22
	oldExists := groupExists
	groupExists = func(got int) bool { return got == pgid }
	t.Cleanup(func() { groupExists = oldExists })

	done := tmpJob(t, m, "done", store.StatusSucceeded, 3*time.Hour, nil)
	failed := tmpJob(t, m, "failed", store.StatusFailed, 3*time.Hour, nil)
	young := tmpJob(t, m, "young", store.StatusSucceeded, time.Hour, nil)
	stopped := tmpJob(t, m, "stopped", store.StatusStopped, 3*time.Hour, nil)
	alive := tmpJob(t, m, "alive", store.StatusFailed, 3*time.Hour, func(j *store.Job) { j.AcceptancePGID = pgid })
	uncleaned := tmpJob(t, m, "uncleaned", store.StatusSucceeded, 3*time.Hour, func(j *store.Job) { j.RepoPath = t.TempDir() })

	dry, err := m.Prune(true)
	if err != nil {
		t.Fatal(err)
	}
	if !exists(filepath.Join(done, JobTmpDir)) {
		t.Fatal("a dry run removed the temp dir")
	}
	if !slices.ContainsFunc(dry.Jobs, func(j PruneJob) bool { return j.ID == "done" && len(j.Removed) == 1 }) || dry.Bytes < 2*4096 {
		t.Fatalf("dry run does not list the temp dirs: %+v", dry)
	}

	rep, err := m.Prune(false)
	if err != nil {
		t.Fatal(err)
	}
	for _, dir := range []string{done, failed} {
		if exists(filepath.Join(dir, JobTmpDir)) {
			t.Fatalf("%s: temp dir of a job finished 3 h ago kept", dir)
		}
		for _, keep := range []string{"work/out.txt", "stream.jsonl", "report.md"} {
			if !exists(filepath.Join(dir, keep)) {
				t.Fatalf("%s: the short retention removed %s", dir, keep)
			}
		}
	}
	for _, dir := range []string{young, stopped, alive, uncleaned} {
		if !exists(filepath.Join(dir, JobTmpDir, "go-build1", "big.bin")) {
			t.Fatalf("%s: temp dir removed", dir)
		}
	}
	if exists(m.ArchiveDir()) {
		t.Fatal("the short retention archived a job (it would no longer be resumable)")
	}
	byID := map[string]PruneJob{}
	for _, j := range rep.Jobs {
		byID[j.ID] = j
	}
	if pj := byID["done"]; len(pj.Removed) != 1 || pj.Removed[0].Path != filepath.Join(done, JobTmpDir) ||
		pj.Removed[0].Bytes < 4096 || len(pj.Archived) != 0 {
		t.Fatalf("report of the removed temp dir: %+v", pj)
	}
	if !strings.Contains(byID["alive"].Skipped, "acceptance process group") {
		t.Fatalf("live group not reported: %+v", byID["alive"])
	}
	if _, ok := byID["young"]; ok {
		t.Fatalf("young job in the report: %+v", rep.Jobs)
	}
	if _, ok := byID["stopped"]; ok {
		t.Fatalf("stopped job in the report: %+v", rep.Jobs)
	}
	if got := get(t, m, "done"); got.DirTmpBytes != 0 || got.DirBytes == 0 {
		t.Fatalf("recorded size not refreshed: %d / %d", got.DirBytes, got.DirTmpBytes)
	}
	// A second pass finds nothing to do and reports nothing for the cleaned jobs.
	rep, err = m.Prune(false)
	if err != nil || slices.ContainsFunc(rep.Jobs, func(j PruneJob) bool { return j.ID == "done" || j.ID == "failed" }) {
		t.Fatalf("second pass: %+v, %v", rep.Jobs, err)
	}
}

// job_tmp_retention_hours: 0 (or a value not shorter than the folder's 48 h) keeps the old behaviour,
// the temp dir goes with the folder; a negative value is refused.
func TestPruneTmpRetentionConfig(t *testing.T) {
	if got := (Config{}).JobTmpRetention(); got != 2*time.Hour {
		t.Fatalf("default retention %s, want 2h", got)
	}
	for _, hours := range []int{0, 48, 72} {
		m := newManager(t)
		m.Cfg.JobTmpRetentionHours = &hours
		dir := tmpJob(t, m, "done", store.StatusSucceeded, 47*time.Hour, nil)
		rep, err := m.Prune(false)
		if err != nil || len(rep.Jobs) != 0 || !exists(filepath.Join(dir, JobTmpDir, "go-build1", "big.bin")) {
			t.Fatalf("job_tmp_retention_hours %d: temp dir removed before the folder's 48 h: %+v, %v", hours, rep.Jobs, err)
		}
	}
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "config.json"), []byte(`{"job_tmp_retention_hours": 5, "job_dirs_warn_gb": 0}`), 0o600)
	c, err := LoadConfig(dir)
	if err != nil || c.JobTmpRetention() != 5*time.Hour || c.JobDirsWarnBytes() != 0 {
		t.Fatalf("config: %s %d %v", c.JobTmpRetention(), c.JobDirsWarnBytes(), err)
	}
	for _, bad := range []string{`{"job_tmp_retention_hours": -1}`, `{"job_dirs_warn_gb": -1}`} {
		writeFile(t, filepath.Join(dir, "config.json"), []byte(bad), 0o600)
		if _, err := LoadConfig(dir); err == nil {
			t.Fatalf("%s accepted", bad)
		}
	}
}

// The temp dir pass the daemon runs between the daily prunes touches no folder due for the full
// prune (no archive at an odd hour) and is started at most once per TmpPruneInterval.
func TestStartTmpPruneOnlyTempDirsAndThrottled(t *testing.T) {
	m := newManager(t)
	now := time.Now()
	m.Now = func() time.Time { return now }
	old := tmpJob(t, m, "old", store.StatusSucceeded, 49*time.Hour, nil)
	done := tmpJob(t, m, "done", store.StatusSucceeded, 3*time.Hour, nil)
	ctx := t.Context()
	if !m.StartTmpPrune(ctx) {
		t.Fatal("first temp dir pass not started")
	}
	m.WaitDaily()
	if exists(filepath.Join(done, JobTmpDir)) {
		t.Fatal("temp dir of a job finished 3 h ago kept")
	}
	if !exists(filepath.Join(old, JobTmpDir)) || !exists(filepath.Join(old, "stream.jsonl")) || exists(m.ArchiveDir()) {
		t.Fatal("the temp dir pass touched a folder due for the full prune")
	}
	if m.StartTmpPrune(ctx) {
		t.Fatal("second pass started within the interval")
	}
	now = now.Add(TmpPruneInterval)
	if !m.StartTmpPrune(ctx) {
		t.Fatal("pass not started after the interval")
	}
	m.WaitDaily()
	now = now.Add(TmpPruneInterval)
	m.Cfg.DailyOff = true
	if m.StartTmpPrune(ctx) {
		t.Fatal("pass started with daily_off")
	}
}

// #240: the disk status totals all job folders, and measures those of running jobs live (their size
// is recorded only when the attempt ends), so a temp dir filling up right now is seen.
func TestDiskCountsRunningJobDirs(t *testing.T) {
	m := newManager(t)
	insertJob(t, m, "running", store.StatusRunning, nil, nil)
	writeBytes(t, filepath.Join(m.JobDir("running"), JobTmpDir, "big.bin"), 1<<20)
	fin := m.Now().Add(-time.Hour)
	insertJob(t, m, "done", store.StatusSucceeded, &fin, nil)
	if err := m.Store.SetDirBytes("done", 5<<20, 4<<20, false); err != nil {
		t.Fatal(err)
	}
	d := m.Disk()
	if d.JobDirsBytes < 6<<20 || d.JobDirsBytes > 7<<20 || d.JobTmpBytes < 5<<20 || d.JobDirsPartial {
		t.Fatalf("disk status: dirs %d tmp %d partial %v", d.JobDirsBytes, d.JobTmpBytes, d.JobDirsPartial)
	}
	if !slices.ContainsFunc(d.LargestJobDirs, func(u JobDirUsage) bool { return u.ID == "running" && u.TmpBytes >= 1<<20 }) {
		t.Fatalf("running job missing from the largest folders: %+v", d.LargestJobDirs)
	}
	// Out of time: the running job's folder is not walked and the total says it is a lower bound.
	oldBudget := runningDirsBudget
	runningDirsBudget = -time.Second
	t.Cleanup(func() { runningDirsBudget = oldBudget })
	if d := m.Disk(); !d.JobDirsPartial || d.JobDirsBytes != 5<<20 {
		t.Fatalf("over budget: dirs %d partial %v", d.JobDirsBytes, d.JobDirsPartial)
	}
}

// #240 High (a), end to end: a mount point inside a finished job's temp dir is left alone with its
// content, the rest of the temp dir is removed and the job is reported with an error.
func TestPruneTmpKeepsMountedSubtree(t *testing.T) {
	m := newManager(t)
	dir := tmpJob(t, m, "mnt", store.StatusSucceeded, 3*time.Hour, nil)
	mount := filepath.Join(dir, JobTmpDir, "pg", "mnt")
	writeFile(t, filepath.Join(mount, "data"), []byte("d"), 0o600)
	stubOpener(t, mount, syscall.EXDEV)

	rep, err := m.PruneTmpContext(context.Background(), false)
	if err == nil || !strings.Contains(err.Error(), "mount") {
		t.Fatalf("err = %v, want the mount boundary reported", err)
	}
	if !exists(filepath.Join(mount, "data")) {
		t.Fatal("content behind the mount point removed")
	}
	if exists(filepath.Join(dir, JobTmpDir, "go-build1")) {
		t.Fatal("the rest of the temp dir was kept")
	}
	if len(rep.Jobs) != 1 || rep.Jobs[0].ID != "mnt" || !strings.Contains(rep.Jobs[0].Error, mount) || rep.Bytes < 4096 {
		t.Fatalf("report = %+v", rep)
	}
}

// A removal that runs out of its budget is not an error: the job is reported as continuing and a later
// pass finishes it.
func TestPruneTmpBudgetContinuesNextPass(t *testing.T) {
	m := newManager(t)
	dir := tmpJob(t, m, "big", store.StatusSucceeded, 3*time.Hour, nil)
	oldN := removeMaxEntries
	removeMaxEntries = 2
	t.Cleanup(func() { removeMaxEntries = oldN })
	rep, err := m.PruneTmpContext(context.Background(), false)
	if err != nil || len(rep.Jobs) != 1 || rep.Jobs[0].Skipped != pruneSkipBudget || !exists(filepath.Join(dir, JobTmpDir)) {
		t.Fatalf("first pass: err %v, report %+v", err, rep)
	}
	removeMaxEntries = oldN
	if _, err := m.PruneTmpContext(context.Background(), false); err != nil || exists(filepath.Join(dir, JobTmpDir)) {
		t.Fatalf("second pass: err %v, temp dir exists %v", err, exists(filepath.Join(dir, JobTmpDir)))
	}
}

// #240 High (b): a finished job whose cgroup scope stop is still pending may have live processes
// writing its folder. Neither the temp dir pass nor the full prune touches it; once the stop went
// through, the next pass removes the temp dir.
func TestPruneSkipsPendingScopeStop(t *testing.T) {
	m := newManager(t)
	recent := tmpJob(t, m, "recent", store.StatusFailed, 3*time.Hour, func(j *store.Job) { j.ScopePending = true })
	old := tmpJob(t, m, "old", store.StatusFailed, 50*time.Hour, func(j *store.Job) { j.ScopePending = true })

	check := func(rep PruneReport, err error, id string) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
		if len(rep.Jobs) != 1 || rep.Jobs[0].ID != id || !strings.Contains(rep.Jobs[0].Skipped, "scope") || len(rep.Jobs[0].Removed) != 0 {
			t.Fatalf("report = %+v, want job %s skipped for its scope", rep.Jobs, id)
		}
	}
	rep, err := m.PruneTmpContext(context.Background(), false)
	check(rep, err, "recent")
	rep, err = m.Prune(false)
	if err != nil || len(rep.Jobs) != 2 {
		t.Fatalf("full prune: err %v, report %+v", err, rep.Jobs)
	}
	for _, pj := range rep.Jobs {
		if !strings.Contains(pj.Skipped, "scope") || len(pj.Removed)+len(pj.Archived) != 0 {
			t.Fatalf("job %s: %+v, want skipped for its scope", pj.ID, pj)
		}
	}
	for _, dir := range []string{recent, old} {
		for _, keep := range []string{filepath.Join(JobTmpDir, "go-build1", "big.bin"), "work/out.txt", "stream.jsonl"} {
			if !exists(filepath.Join(dir, keep)) {
				t.Fatalf("%s: %s removed while the scope stop is pending", dir, keep)
			}
		}
	}
	if err := m.Store.SetScopePending("recent", false); err != nil {
		t.Fatal(err)
	}
	if _, err := m.PruneTmpContext(context.Background(), false); err != nil || exists(filepath.Join(recent, JobTmpDir)) {
		t.Fatalf("after the stop: err %v, temp dir exists %v", err, exists(filepath.Join(recent, JobTmpDir)))
	}
}

// The one hold rule (#240 High b): each condition alone yields its reason, in the order scope,
// advisor calls, spawned group, acceptance group; a job with none of them is free.
func TestLifecycleHoldTable(t *testing.T) {
	m := newManager(t)
	const accPGID = 1<<22 - 1
	// The spawned group is checked with the kernel (groupAlive): a real group of its own.
	sleep := exec.Command("sleep", "60")
	sleep.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := sleep.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = sleep.Process.Kill(); _ = sleep.Wait() })
	pgid := sleep.Process.Pid
	alive := 0
	oldExists := groupExists
	groupExists = func(got int) bool { return got == alive }
	t.Cleanup(func() { groupExists = oldExists })
	fin := m.Now().Add(-time.Hour)
	cases := []struct {
		id    string
		mod   func(*store.Job)
		alive int
		kind  holdKind
		want  string
	}{
		{"free", nil, 0, holdNone, ""},
		{"scope", func(j *store.Job) { j.ScopePending = true }, 0, holdScope, "cgroup scope"},
		{"advisor", func(j *store.Job) { j.Crew = &store.Crew{AdvisorModel: "opus"} }, 0, holdAdvisor, "advisor calls"},
		{"spawned", func(j *store.Job) { j.PGID, j.PGIDSpawned = pgid, true }, pgid, holdSpawnedGroup, fmt.Sprintf("process group %d of", pgid)},
		{"acceptance", func(j *store.Job) { j.AcceptancePGID = accPGID }, accPGID, holdAcceptanceGroup, "acceptance process group 4194303"},
		{"all", func(j *store.Job) {
			j.ScopePending, j.Crew, j.AcceptancePGID = true, &store.Crew{AdvisorModel: "opus"}, accPGID
		}, accPGID, holdScope, "cgroup scope"},
	}
	for _, c := range cases {
		j := insertJob(t, m, c.id, store.StatusFailed, &fin, c.mod)
		if j.Crew.HasAdvisor() {
			writeFile(t, filepath.Join(m.AdvisorDir(j.ID), "run.1.json"), []byte("{}"), 0o600)
		}
		alive = c.alive
		if got := m.lifecycleHoldKind(j); got != c.kind {
			t.Fatalf("%s: kind = %d, want %d", c.id, got, c.kind)
		}
		got := m.lifecycleHold(j)
		if (c.want == "") != (got == "") || !strings.Contains(got, c.want) {
			t.Fatalf("%s: hold = %q, want it to name %q", c.id, got, c.want)
		}
	}
}
