package jobs

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/qwwiwi/agent-jobs/internal/store"
)

func TestTempEnvReplacesEveryTempVariable(t *testing.T) {
	env := TempEnv([]string{"PATH=/bin", "TMPDIR=/tmp", "TMP=/x", "TEMP=/y", "GOTMPDIR=/z", "TMPDIRX=keep"}, "/job/tmp")
	want := []string{"PATH=/bin", "TMPDIRX=keep", "TMPDIR=/job/tmp", "GOTMPDIR=/job/tmp", "TMP=/job/tmp", "TEMP=/job/tmp"}
	if !slices.Equal(env, want) {
		t.Fatalf("TempEnv = %q, want %q", env, want)
	}
	dir := t.TempDir()
	acc, err := acceptanceEnv(dir, []string{"PATH=/bin", "TMPDIR=/tmp", "GOTMPDIR=/tmp"})
	if err != nil {
		t.Fatal(err)
	}
	tmp := filepath.Join(dir, acceptanceHome, "tmp")
	for _, k := range tempEnvKeys {
		if !slices.Contains(acc, k+"="+tmp) || slices.Contains(acc, k+"=/tmp") {
			t.Fatalf("acceptance %s is not %s: %q", k, tmp, acc)
		}
	}
}

// #43: claude runs with TMPDIR, GOTMPDIR, TMP and TEMP set to <jobdir>/tmp (0700, made by prepare).
func TestJobRunsWithItsOwnTempDir(t *testing.T) {
	m := newManager(t)
	j := runToEnd(t, m, enqueue(t, m, "FAKE_TMPENV", nil).ID)
	tmp := filepath.Join(m.JobDir(j.ID), JobTmpDir)
	fi, err := os.Lstat(tmp)
	if err != nil || !fi.IsDir() || fi.Mode().Perm() != 0o700 {
		t.Fatalf("job temp dir %s: %v %v", tmp, fi, err)
	}
	b, err := os.ReadFile(filepath.Join(m.JobDir(j.ID), "tmpenv.txt"))
	if err != nil {
		t.Fatal(err)
	}
	var want []string
	for _, k := range []string{"GOTMPDIR", "TEMP", "TMP", "TMPDIR"} {
		want = append(want, k+"="+tmp)
	}
	got := strings.Fields(string(b))
	slices.Sort(got)
	if !slices.Equal(got, want) {
		t.Fatalf("claude's temp env %q, want %q", got, want)
	}
	// A planted symlink in its place is refused, not followed.
	other := t.TempDir()
	if err := os.Remove(tmp); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(other, tmp); err != nil {
		t.Fatal(err)
	}
	if _, err := ensureJobTmp(m.JobDir(j.ID)); err == nil {
		t.Fatal("symlinked job temp dir accepted")
	}
}

// goBuildDirs lists the go-build* work dirs directly under dir.
func goBuildDirs(t *testing.T, dir string) []string {
	t.Helper()
	m, err := filepath.Glob(filepath.Join(dir, "go-build*"))
	if err != nil {
		t.Fatal(err)
	}
	return m
}

// #43: a job killed while `go test` runs leaves go's work dir in its own temp dir, never in the
// system's.
func TestKilledGoTestLeavesNothingInSystemTemp(t *testing.T) {
	if _, err := exec.LookPath("go"); err != nil {
		t.Skip("no go on PATH")
	}
	sys := t.TempDir()
	t.Setenv("TMPDIR", sys)
	t.Setenv("GOTMPDIR", "")
	m := newManager(t)
	j := enqueue(t, m, "FAKE_GOTEST FAKE_SLEEP=120", nil)
	if err := m.Tick(); err != nil {
		t.Fatal(err)
	}
	dir := m.JobDir(j.ID)
	waitFor(t, "the sleeping go test", 90*time.Second, func() bool {
		_, err := os.Stat(filepath.Join(dir, "gotest.running"))
		return err == nil
	})
	if len(goBuildDirs(t, filepath.Join(dir, JobTmpDir))) == 0 {
		t.Fatal("go test runs without a go-build dir in the job's temp dir")
	}
	if err := m.Cancel(j.ID); err != nil {
		t.Fatal(err)
	}
	if got := goBuildDirs(t, sys); len(got) != 0 {
		t.Fatalf("go-build dirs in the system temp dir: %v", got)
	}
}

// #43: the job folder's size (the worktree of a repo job left out) lands on the row and the stats
// row, the job-done line shows it from the threshold on, prune removes the temp dir, and the disk
// status lists the largest folders.
func TestJobDirSizeReported(t *testing.T) {
	m := newManager(t)
	plain := runToEnd(t, m, enqueue(t, m, "FAKE_BIGTMP", nil).ID)
	repo := runToEnd(t, m, enqueue(t, m, "FAKE_BIGTMP", func(r *StartRequest) { r.RepoPath = newRepo(t) }).ID)
	const mb = 1 << 20
	if plain.DirBytes < 6*mb {
		t.Fatalf("plain job dir %d bytes, want temp dir and work dir counted", plain.DirBytes)
	}
	if repo.DirBytes < 3*mb || repo.DirBytes >= 6*mb {
		t.Fatalf("repo job dir %d bytes, want the temp dir without the worktree", repo.DirBytes)
	}
	st, err := m.Store.GetStats(plain.ID)
	if err != nil || st.DirBytes != plain.DirBytes {
		t.Fatalf("stats dir bytes %v %v, want %d", st, err, plain.DirBytes)
	}
	if l := DoneLine(plain, 5*mb); !strings.Contains(l, " · job dir 6.") {
		t.Fatalf("done line above the threshold: %s", l)
	}
	if l := DoneLine(repo, 5*mb); strings.Contains(l, "job dir") {
		t.Fatalf("done line below the threshold: %s", l)
	}
	if l := DoneLine(plain, 0); strings.Contains(l, "job dir") {
		t.Fatalf("done line with the note off: %s", l)
	}
	if got := (Config{}).JobDirNoteBytes(); got != DefaultJobDirNoteMB*mb {
		t.Fatalf("default threshold %d", got)
	}

	d := m.Disk()
	if len(d.LargestJobDirs) != 2 || d.LargestJobDirs[0].ID != plain.ID || d.LargestJobDirs[0].TmpBytes < 3*mb {
		t.Fatalf("largest job dirs %+v", d.LargestJobDirs)
	}
	if d.JobTmpBytes < 6*mb {
		t.Fatalf("job temp bytes %d", d.JobTmpBytes)
	}

	real := m.Now
	m.Now = func() time.Time { return real().Add(PruneAge + time.Hour) }
	if _, err := m.Prune(false); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(filepath.Join(m.JobDir(plain.ID), JobTmpDir)); !os.IsNotExist(err) {
		t.Fatalf("prune kept the job temp dir: %v", err)
	}
}

func TestJobDirNoteConfig(t *testing.T) {
	for v, want := range map[string]int64{"0": 0, "7": 7 << 20, "-1": -1} {
		dir := t.TempDir()
		if err := os.WriteFile(filepath.Join(dir, "config.json"), []byte(`{"job_dir_note_mb": `+v+`}`), 0o600); err != nil {
			t.Fatal(err)
		}
		c, err := LoadConfig(dir)
		if want < 0 {
			if err == nil {
				t.Fatal("negative job_dir_note_mb accepted")
			}
			continue
		}
		if err != nil || c.JobDirNoteBytes() != want {
			t.Fatalf("job_dir_note_mb %s: %d %v, want %d", v, c.JobDirNoteBytes(), err, want)
		}
	}
}

// writeBytes writes a file of n bytes at p.
func writeBytes(t *testing.T, p string, n int) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, make([]byte, n), 0o644); err != nil {
		t.Fatal(err)
	}
}

// #43 review: the job can write its folder; the size walk follows no symlink, neither one put in place
// of the folder itself nor one inside it (to a directory or a file).
func TestJobDirSizeFollowsNoSymlink(t *testing.T) {
	const mb = 1 << 20
	outside := t.TempDir()
	writeBytes(t, filepath.Join(outside, "big"), 4*mb)
	base := t.TempDir()
	dir := filepath.Join(base, "job")
	writeBytes(t, filepath.Join(dir, "small"), 100)
	for name, target := range map[string]string{"dirlink": outside, "filelink": filepath.Join(outside, "big"),
		JobTmpDir: outside} {
		if err := os.Symlink(target, filepath.Join(dir, name)); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Symlink(outside, filepath.Join(dir, "sub")); err != nil {
		t.Fatal(err)
	}
	s, err := jobDirSize(&store.Job{}, dir)
	if err != nil || s.Bytes >= mb || s.TmpBytes != 0 || s.Partial {
		t.Fatalf("job dir with symlinks inside: %+v %v, want only the links and the small file counted", s, err)
	}
	root := filepath.Join(base, "rootlink")
	if err := os.Symlink(outside, root); err != nil {
		t.Fatal(err)
	}
	if s, err := jobDirSize(&store.Job{}, root); err == nil {
		t.Fatalf("a symlinked job folder was measured: %+v", s)
	}
}

// #43 review: the walk that runs when a job settles is bounded by entries and by time; a size it
// could not finish is recorded as partial, and the disk status reads the recorded sizes instead of
// walking every job folder again.
func TestJobDirSizeBoundedAndCached(t *testing.T) {
	oldMax, oldBudget := dirWalkMaxEntries, dirWalkBudget
	t.Cleanup(func() { dirWalkMaxEntries, dirWalkBudget = oldMax, oldBudget })
	dir := t.TempDir()
	for i := 0; i < 50; i++ {
		writeBytes(t, filepath.Join(dir, "d", fmt.Sprintf("f%02d", i)), 10)
	}
	full, err := jobDirSize(&store.Job{}, dir)
	if err != nil || full.Partial || full.Bytes < 500 {
		t.Fatalf("unbounded: %+v %v", full, err)
	}
	dirWalkMaxEntries = 10
	if s, err := jobDirSize(&store.Job{}, dir); err != nil || !s.Partial || s.Bytes >= full.Bytes {
		t.Fatalf("entry cap: %+v %v, want partial", s, err)
	}
	dirWalkMaxEntries, dirWalkBudget = oldMax, -time.Second
	if s, err := jobDirSize(&store.Job{}, dir); err != nil || !s.Partial {
		t.Fatalf("time budget: %+v %v, want partial", s, err)
	}
	dirWalkBudget = oldBudget

	m := newManager(t)
	j := enqueue(t, m, "x", nil)
	jd := m.JobDir(j.ID)
	writeBytes(t, filepath.Join(jd, JobTmpDir, "a"), 3000)
	for i := 0; i < 20; i++ {
		writeBytes(t, filepath.Join(jd, "many", fmt.Sprint(i)), 1)
	}
	dirWalkMaxEntries = 5
	m.recordDirSize(j.ID)
	got := get(t, m, j.ID)
	if !got.DirPartial || got.DirBytes == 0 {
		t.Fatalf("recorded size %d partial %v", got.DirBytes, got.DirPartial)
	}
	if l := DoneLine(got, 1<<30); !strings.Contains(l, "not fully measured") {
		t.Fatalf("done line of a partial size: %s", l)
	}
	dirWalkMaxEntries = oldMax
	m.recordDirSize(j.ID)
	got = get(t, m, j.ID)
	if got.DirPartial || got.DirTmpBytes < 3000 {
		t.Fatalf("recorded size %d tmp %d partial %v", got.DirBytes, got.DirTmpBytes, got.DirPartial)
	}

	writeBytes(t, filepath.Join(jd, "later"), 1<<20) // not walked by the disk status
	d := m.Disk()
	if len(d.LargestJobDirs) != 1 || d.LargestJobDirs[0].Bytes != got.DirBytes || d.JobTmpBytes != got.DirTmpBytes {
		t.Fatalf("disk status %+v tmp %d, want the recorded %d / %d", d.LargestJobDirs, d.JobTmpBytes, got.DirBytes, got.DirTmpBytes)
	}
}

// #105: the job's cleanup leaves its temp dir alone (no early removal: proving it unused safely needs
// OS-level isolation); the disk status still shows its size, and prune removes it with the folder.
func TestFinishedJobTmpKeptUntilPrune(t *testing.T) {
	m := newManager(t)
	notifyTo(t, m)
	j := runToEnd(t, m, enqueue(t, m, "FAKE_BIGTMP", nil).ID)
	big := filepath.Join(m.JobDir(j.ID), JobTmpDir, "big.bin")
	if _, err := os.Stat(big); err != nil {
		t.Fatalf("the job left nothing in its temp dir: %v", err)
	}
	if got := get(t, m, j.ID); got.NotifiedAt == nil {
		t.Fatal("job not notified")
	}
	real := m.Now
	m.Now = func() time.Time { return real().Add(CleanupDelay + time.Minute) }
	if err := m.Tick(); err != nil {
		t.Fatal(err)
	}
	if got := get(t, m, j.ID); got.CleanedAt == nil {
		t.Fatal("cleanup did not run")
	}
	if _, err := os.Stat(big); err != nil {
		t.Fatalf("cleanup removed the job's temp dir: %v", err)
	}
	if d := m.Disk(); d.JobTmpBytes < 3<<20 {
		t.Fatalf("disk status lost the kept temp dir: %d bytes", d.JobTmpBytes)
	}

	m.Now = func() time.Time { return real().Add(PruneAge + time.Hour) }
	if _, err := m.Prune(false); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(filepath.Join(m.JobDir(j.ID), JobTmpDir)); !os.IsNotExist(err) {
		t.Fatalf("prune kept the job temp dir: %v", err)
	}
}
