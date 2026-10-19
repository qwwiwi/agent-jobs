package jobs

import (
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
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
