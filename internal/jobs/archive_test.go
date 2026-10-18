package jobs

import (
	"bytes"
	"compress/gzip"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/qwwiwi/agent-jobs/internal/store"
)

func gunzipFile(t *testing.T, p string) []byte {
	t.Helper()
	f, err := os.Open(p)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	zr, err := gzip.NewReader(f)
	if err != nil {
		t.Fatal(err)
	}
	b, err := io.ReadAll(zr)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// Prune moves a job's records into <archive>/<YYYY-MM>/<id>/ instead of deleting them: the stream
// and stderr gzipped (and gone from the job folder), prompt, report and diffstat copied, meta.json
// the job row. The dry run changes nothing; a second pass archives nothing again.
func TestPruneMovesRecordsIntoArchive(t *testing.T) {
	m := newManager(t)
	now := time.Now()
	m.Now = func() time.Time { return now }
	old := now.Add(-49 * time.Hour)
	outside := filepath.Join(t.TempDir(), "outside")
	writeFile(t, filepath.Join(outside, "keep-me"), []byte("x"), 0o600)
	insertJob(t, m, "old", store.StatusSucceeded, &old, func(j *store.Job) { j.Turns, j.CostUSD = 7, 0.5 })
	dir := m.JobDir("old")
	fillJobDir(t, dir, outside)
	stream, _ := os.ReadFile(filepath.Join(dir, "stream.jsonl"))
	writeFile(t, filepath.Join(dir, "stderr.log"), []byte("warn: x\n"), 0o600)
	report, _ := os.ReadFile(filepath.Join(dir, "report.md"))

	dest := filepath.Join(m.StateDir, "archive", old.UTC().Format("2006-01"), "old")
	dry, err := m.Prune(true)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(filepath.Join(m.StateDir, "archive")); !os.IsNotExist(err) {
		t.Fatalf("dry run created the archive: %v", err)
	}
	if len(dry.Jobs) != 1 || len(dry.Jobs[0].Archived) != 2 || dry.ArchivedBytes != int64(len(stream))+8 {
		t.Fatalf("dry run report: %+v", dry)
	}
	for _, e := range dry.Jobs[0].Removed {
		if strings.HasSuffix(e.Path, "stream.jsonl") {
			t.Fatalf("dry run lists the stream as removed, not archived: %+v", e)
		}
	}

	rep, err := m.Prune(false)
	if err != nil {
		t.Fatal(err)
	}
	if rep.ArchivedBytes != dry.ArchivedBytes || rep.Jobs[0].ArchiveDest != dest {
		t.Fatalf("report: %+v", rep)
	}
	if got := names(dest); !slices.Equal(got, []string{"diffstat.txt", "meta.json", "prompt.md", "report.md",
		"stderr.log.gz", "stream.jsonl.gz"}) {
		t.Fatalf("archive holds %v", got)
	}
	if got := gunzipFile(t, filepath.Join(dest, "stream.jsonl.gz")); !bytes.Equal(got, stream) {
		t.Fatalf("archived stream differs (%d vs %d bytes)", len(got), len(stream))
	}
	if got := gunzipFile(t, filepath.Join(dest, "stderr.log.gz")); string(got) != "warn: x\n" {
		t.Fatalf("archived stderr: %q", got)
	}
	if got, _ := os.ReadFile(filepath.Join(dest, "report.md")); !bytes.Equal(got, report) {
		t.Fatal("archived report differs")
	}
	var meta map[string]any
	b, _ := os.ReadFile(filepath.Join(dest, "meta.json"))
	if err := json.Unmarshal(b, &meta); err != nil || meta["id"] != "old" || meta["turns"] != float64(7) {
		t.Fatalf("meta.json: %s %v", b, err)
	}
	if _, ok := meta["launch_token"]; ok {
		t.Fatal("meta.json carries the launch token")
	}
	for _, p := range []string{filepath.Join(m.StateDir, "archive"), filepath.Dir(dest), dest} {
		if fi, err := os.Lstat(p); err != nil || fi.Mode().Perm() != 0o700 {
			t.Fatalf("%s must be 0700: %v %v", p, fi.Mode(), err)
		}
	}
	got := names(dir)
	if slices.Contains(got, "stream.jsonl") || slices.Contains(got, "stderr.log") || !slices.Contains(got, "report.md") {
		t.Fatalf("job dir after prune: %v", got)
	}
	// The summary row was taken while the stream was still in the job folder.
	if st, err := m.Store.GetStats("old"); err != nil || st.Source != "job_dir" {
		t.Fatalf("stats row before archiving: %+v %v", st, err)
	}
	again, err := m.Prune(false)
	if err != nil || len(again.Jobs) != 0 || again.ArchivedBytes != 0 {
		t.Fatalf("second prune: %+v %v", again, err)
	}
	if ents, _ := os.ReadDir(filepath.Dir(dest)); len(ents) != 1 {
		t.Fatalf("month dir after two passes: %v", ents)
	}
}

// The archive root may be configured elsewhere (another disk, a synced folder); `Archive` alone
// moves the same records prune would.
func TestArchiveConfiguredDir(t *testing.T) {
	m := newManager(t)
	m.Cfg.LogArchiveDir = filepath.Join(t.TempDir(), "elsewhere", "logs")
	old := time.Now().Add(-72 * time.Hour)
	insertJob(t, m, "a", store.StatusFailed, &old, nil)
	writeFile(t, filepath.Join(m.JobDir("a"), "stream.jsonl"), []byte(`{"type":"result","subtype":"success"}`+"\n"), 0o600)
	dry, err := m.Archive(true)
	if err != nil || len(dry.Jobs) != 1 || len(dry.Jobs[0].Moved) != 1 {
		t.Fatalf("dry: %+v %v", dry, err)
	}
	if _, err := os.Lstat(m.Cfg.LogArchiveDir); !os.IsNotExist(err) {
		t.Fatal("dry run created the archive")
	}
	if _, err := m.Archive(false); err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(m.Cfg.LogArchiveDir, old.UTC().Format("2006-01"), "a", "stream.jsonl.gz")
	if got := gunzipFile(t, p); !strings.Contains(string(got), "success") {
		t.Fatalf("archived: %q", got)
	}
	if _, err := os.Lstat(filepath.Join(m.JobDir("a"), "stream.jsonl")); !os.IsNotExist(err) {
		t.Fatal("stream still in the job dir")
	}
	if _, err := os.Lstat(filepath.Join(m.StateDir, "archive")); !os.IsNotExist(err) {
		t.Fatal("the default archive was used")
	}
}

// A symlinked archive root (or month dir) is refused: nothing is written through it and the stream
// stays in the job folder (the rest of the folder is still pruned).
func TestArchiveRefusesSymlink(t *testing.T) {
	for _, where := range []string{"root", "month"} {
		t.Run(where, func(t *testing.T) {
			m := newManager(t)
			old := time.Now().Add(-72 * time.Hour)
			insertJob(t, m, "s", store.StatusSucceeded, &old, nil)
			writeFile(t, filepath.Join(m.JobDir("s"), "stream.jsonl"), []byte("{}\n"), 0o600)
			writeFile(t, filepath.Join(m.JobDir("s"), "work", "x.bin"), []byte("\x00"), 0o600)
			target := t.TempDir()
			link := filepath.Join(m.StateDir, "archive")
			if where == "month" {
				if err := os.MkdirAll(link, 0o700); err != nil {
					t.Fatal(err)
				}
				link = filepath.Join(link, old.UTC().Format("2006-01"))
			}
			if err := os.Symlink(target, link); err != nil {
				t.Fatal(err)
			}
			rep, err := m.Prune(false)
			if err == nil || !strings.Contains(err.Error(), "symlink") {
				t.Fatalf("prune through a symlinked archive: %v", err)
			}
			if len(rep.Jobs) != 1 || rep.Jobs[0].Error == "" || len(rep.Jobs[0].Removed) != 1 {
				t.Fatalf("report: %+v", rep.Jobs)
			}
			if got := names(m.JobDir("s")); !slices.Equal(got, []string{"stream.jsonl"}) {
				t.Fatalf("job dir touched: %v", got)
			}
			if ents, _ := os.ReadDir(target); len(ents) != 0 {
				t.Fatalf("written through the symlink: %v", ents)
			}
		})
	}
}

// Archive retention: in a root with the marker, months whose last day is older than log_archive_days
// are deleted by prune; newer months, non-month entries and everything with 0 (forever) stay.
func TestArchiveRetention(t *testing.T) {
	m := newManager(t)
	now := time.Date(2026, 10, 17, 12, 0, 0, 0, time.UTC)
	m.Now = func() time.Time { return now }
	root := m.ArchiveDir()
	writeFile(t, filepath.Join(root, archiveMarker), nil, 0o600)
	for _, d := range []string{"2025-09/j1", "2025-10/j2", "2025-11/j3", "2026-10/j4", "notes"} {
		writeFile(t, filepath.Join(root, d, "meta.json"), []byte("{}"), 0o600)
	}
	outside := t.TempDir()
	writeFile(t, filepath.Join(outside, "keep"), []byte("x"), 0o600)
	if err := os.Symlink(outside, filepath.Join(root, "2024-01")); err != nil {
		t.Fatal(err)
	}
	zero := 0
	m.Cfg.LogArchiveDays = &zero
	if rep, err := m.Prune(false); err != nil || len(rep.ArchiveExpired) != 0 {
		t.Fatalf("forever: %+v %v", rep.ArchiveExpired, err)
	}
	m.Cfg.LogArchiveDays = nil // default 365: cutoff 2025-10-17
	dry, err := m.Prune(true)
	if err != nil || len(dry.ArchiveExpired) != 2 || len(names(root)) != 7 {
		t.Fatalf("dry: %+v %v %v", dry.ArchiveExpired, err, names(root))
	}
	if _, err := m.Prune(false); err != nil {
		t.Fatal(err)
	}
	if got := names(root); !slices.Equal(got, []string{archiveMarker, "2025-10", "2025-11", "2026-10", "notes"}) {
		t.Fatalf("after retention: %v", got)
	}
	if _, err := os.Stat(filepath.Join(outside, "keep")); err != nil {
		t.Fatalf("symlinked month was followed: %v", err)
	}
}

func TestConfigArchiveKeys(t *testing.T) {
	dir := t.TempDir()
	c, err := LoadConfig(dir)
	if err != nil || c.LogArchiveDir != filepath.Join(dir, "archive") || c.ArchiveDays() != 365 {
		t.Fatalf("defaults: %q %d %v", c.LogArchiveDir, c.ArchiveDays(), err)
	}
	for body, want := range map[string]string{
		`{"log_archive_dir":"rel/dir"}`: "absolute",
		`{"log_archive_days":-1}`:       "0 (forever)",
	} {
		writeFile(t, filepath.Join(dir, "config.json"), []byte(body), 0o600)
		if _, err := LoadConfig(dir); err == nil || !strings.Contains(err.Error(), want) {
			t.Fatalf("%s: %v", body, err)
		}
	}
	writeFile(t, filepath.Join(dir, "config.json"), []byte(`{"log_archive_dir":"/srv/logs","log_archive_days":0}`), 0o600)
	if c, err := LoadConfig(dir); err != nil || c.LogArchiveDir != "/srv/logs" || c.ArchiveDays() != 0 {
		t.Fatalf("set: %+v %v", c, err)
	}
}

// The archive and stats CLI: archive --dry-run lists and moves nothing; archive moves; stats
// --backfill --json reports the job. Spawns the binary, so it is refused when run inside a job.
func TestArchiveAndStatsCLI(t *testing.T) {
	m := newManager(t)
	old := time.Now().Add(-50 * time.Hour)
	insertJob(t, m, "cli", store.StatusSucceeded, &old, nil)
	writeFile(t, filepath.Join(m.JobDir("cli"), "stream.jsonl"), []byte(`{"type":"result","subtype":"success"}`+"\n"), 0o600)
	run := func(args ...string) string {
		out, err := execSelf(args...)
		if err != nil {
			t.Fatalf("%v: %v\n%s", args, err, out)
		}
		return out
	}
	if out := run("archive", "--agent", "test", "--dry-run"); !strings.Contains(out, "would move") {
		t.Fatalf("dry run output: %s", out)
	}
	if got := names(m.JobDir("cli")); !slices.Contains(got, "stream.jsonl") {
		t.Fatalf("dry run moved: %v", got)
	}
	run("archive", "--agent", "test")
	if got := names(m.JobDir("cli")); slices.Contains(got, "stream.jsonl") {
		t.Fatalf("after archive: %v", got)
	}
	var rep StatsReport
	if err := json.Unmarshal([]byte(run("stats", "--agent", "test", "--json")), &rep); err != nil || rep.Totals.Jobs != 1 {
		t.Fatalf("stats --json: %+v %v", rep, err)
	}
}

// An archive root agent-jobs did not create (no marker) is used for archiving but never expired or
// chmodded: an owner's folder with month-named dirs keeps them. A root the archive creates gets the
// marker.
func TestArchiveRootWithoutMarker(t *testing.T) {
	m := newManager(t)
	now := time.Date(2026, 10, 17, 12, 0, 0, 0, time.UTC)
	m.Now = func() time.Time { return now }
	root := filepath.Join(t.TempDir(), "Dropbox")
	m.Cfg.LogArchiveDir = root
	writeFile(t, filepath.Join(root, "2024-03", "photo.jpg"), []byte("x"), 0o600)
	if err := os.Chmod(root, 0o755); err != nil {
		t.Fatal(err)
	}
	old := now.Add(-72 * time.Hour)
	insertJob(t, m, "a", store.StatusSucceeded, &old, nil)
	writeFile(t, filepath.Join(m.JobDir("a"), "stream.jsonl"), []byte("{}\n"), 0o600)
	rep, err := m.Prune(false)
	if err != nil || len(rep.ArchiveExpired) != 0 {
		t.Fatalf("prune: %+v %v", rep.ArchiveExpired, err)
	}
	if _, err := os.Stat(filepath.Join(root, "2024-03", "photo.jpg")); err != nil {
		t.Fatalf("owner's month folder removed: %v", err)
	}
	if _, err := os.Stat(filepath.Join(root, old.UTC().Format("2006-01"), "a", "stream.jsonl.gz")); err != nil {
		t.Fatalf("not archived into the existing root: %v", err)
	}
	if fi, _ := os.Lstat(root); fi.Mode().Perm() != 0o755 {
		t.Fatalf("existing root chmodded to %v", fi.Mode().Perm())
	}
	if _, err := os.Lstat(filepath.Join(root, archiveMarker)); !os.IsNotExist(err) {
		t.Fatalf("marker written into an existing root: %v", err)
	}

	m.Cfg.LogArchiveDir = filepath.Join(t.TempDir(), "fresh")
	insertJob(t, m, "b", store.StatusSucceeded, &old, nil)
	writeFile(t, filepath.Join(m.JobDir("b"), "stream.jsonl"), []byte("{}\n"), 0o600)
	if _, err := m.Archive(false); err != nil {
		t.Fatal(err)
	}
	if fi, err := os.Lstat(filepath.Join(m.Cfg.LogArchiveDir, archiveMarker)); err != nil || !fi.Mode().IsRegular() {
		t.Fatalf("created root has no marker: %v", err)
	}
}

// A failing write (an unreadable stream) keeps the originals and leaves no temporary dir.
func TestArchiveFailedWriteKeepsOriginals(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root reads a 0000 file")
	}
	m := newManager(t)
	old := time.Now().Add(-72 * time.Hour)
	insertJob(t, m, "f", store.StatusSucceeded, &old, nil)
	stream := filepath.Join(m.JobDir("f"), "stream.jsonl")
	writeFile(t, stream, []byte("{}\n"), 0o600)
	writeFile(t, filepath.Join(m.JobDir("f"), "stderr.log"), []byte("e\n"), 0o600)
	if err := os.Chmod(stream, 0); err != nil {
		t.Fatal(err)
	}
	defer os.Chmod(stream, 0o600)
	if _, err := m.Archive(false); err == nil {
		t.Fatal("archive of an unreadable stream succeeded")
	}
	if got := names(m.JobDir("f")); !slices.Equal(got, []string{"stderr.log", "stream.jsonl"}) {
		t.Fatalf("originals: %v", got)
	}
	month := filepath.Join(m.ArchiveDir(), old.UTC().Format("2006-01"))
	if got := names(month); len(got) != 0 {
		t.Fatalf("month dir after a failed write: %v", got)
	}
}

// "Already archived" counts only when the destination holds meta.json and the gzipped copy: an empty
// destination keeps the original and reports an error; a complete one lets the leftover go.
func TestArchiveAlreadyArchivedNeedsCopy(t *testing.T) {
	m := newManager(t)
	old := time.Now().Add(-72 * time.Hour)
	j := insertJob(t, m, "d", store.StatusSucceeded, &old, nil)
	stream := filepath.Join(m.JobDir("d"), "stream.jsonl")
	writeFile(t, stream, []byte("{}\n"), 0o600)
	dest := m.archiveJobDir(j)
	if err := os.MkdirAll(dest, 0o700); err != nil {
		t.Fatal(err)
	}
	if _, err := m.Archive(false); err == nil || !strings.Contains(err.Error(), "originals kept") {
		t.Fatalf("empty destination: %v", err)
	}
	if _, err := os.Lstat(stream); err != nil {
		t.Fatalf("original removed with an empty destination: %v", err)
	}
	writeFile(t, filepath.Join(dest, "meta.json"), []byte("{}"), 0o600)
	writeFile(t, filepath.Join(dest, "stream.jsonl.gz"), []byte("gz"), 0o600)
	rep, err := m.Archive(false)
	if err != nil || len(rep.Jobs) != 1 || !rep.Jobs[0].Already || len(rep.Jobs[0].Moved) != 1 {
		t.Fatalf("complete destination: %+v %v", rep.Jobs, err)
	}
	if _, err := os.Lstat(stream); !os.IsNotExist(err) {
		t.Fatalf("leftover kept: %v", err)
	}
}

// When the archive cannot be written, prune still frees the rest of the job folder; only the files
// that would have been archived stay.
func TestPruneWhenArchiveFails(t *testing.T) {
	m := newManager(t)
	writeFile(t, m.ArchiveDir(), []byte("not a dir"), 0o600)
	old := time.Now().Add(-72 * time.Hour)
	insertJob(t, m, "p", store.StatusSucceeded, &old, nil)
	writeFile(t, filepath.Join(m.JobDir("p"), "stream.jsonl"), []byte("{}\n"), 0o600)
	writeFile(t, filepath.Join(m.JobDir("p"), "stderr.log"), []byte("e\n"), 0o600)
	writeFile(t, filepath.Join(m.JobDir("p"), "work", "big.bin"), []byte("\x00"), 0o600)
	rep, err := m.Prune(false)
	if err == nil || !strings.Contains(err.Error(), "log archive") {
		t.Fatalf("prune: %v", err)
	}
	if len(rep.Jobs) != 1 || rep.Jobs[0].Error == "" || len(rep.Jobs[0].Removed) != 1 || len(rep.Jobs[0].Archived) != 0 {
		t.Fatalf("report: %+v", rep.Jobs)
	}
	if got := names(m.JobDir("p")); !slices.Equal(got, []string{"stderr.log", "stream.jsonl"}) {
		t.Fatalf("job dir: %v", got)
	}
}

// Temporary dirs of an interrupted pass are removed before the next write.
func TestArchiveRemovesStaleTemp(t *testing.T) {
	m := newManager(t)
	old := time.Now().Add(-72 * time.Hour)
	insertJob(t, m, "t", store.StatusSucceeded, &old, nil)
	writeFile(t, filepath.Join(m.JobDir("t"), "stream.jsonl"), []byte("{}\n"), 0o600)
	month := filepath.Join(m.ArchiveDir(), old.UTC().Format("2006-01"))
	writeFile(t, filepath.Join(month, ".t.tmp-123", "stream.jsonl.gz"), []byte("partial"), 0o600)
	writeFile(t, filepath.Join(month, ".tx.tmp-1", "keep"), []byte("other job"), 0o600)
	if _, err := m.Archive(false); err != nil {
		t.Fatal(err)
	}
	if got := names(month); !slices.Equal(got, []string{".tx.tmp-1", "t"}) {
		t.Fatalf("month dir: %v", got)
	}
}

// `agent-jobs archive` works on a job folder the job made read-only.
func TestArchiveReadOnlyJobDir(t *testing.T) {
	m := newManager(t)
	old := time.Now().Add(-72 * time.Hour)
	insertJob(t, m, "r", store.StatusSucceeded, &old, nil)
	writeFile(t, filepath.Join(m.JobDir("r"), "stream.jsonl"), []byte("{}\n"), 0o600)
	if err := os.Chmod(m.JobDir("r"), 0o500); err != nil {
		t.Fatal(err)
	}
	defer os.Chmod(m.JobDir("r"), 0o700)
	if _, err := m.Archive(false); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(filepath.Join(m.JobDir("r"), "stream.jsonl")); !os.IsNotExist(err) {
		t.Fatalf("stream still in the job dir: %v", err)
	}
}

// N1: the default root <state>/archive belongs to agent-jobs. One that exists without the marker (an
// earlier build, a marker write that failed after the mkdir) gets it, so retention works; the dry run
// writes nothing and says retention is off. A configured root without the marker is reported, never
// adopted.
func TestArchiveDefaultRootAdoptsMarker(t *testing.T) {
	m := newManager(t)
	now := time.Date(2026, 10, 17, 12, 0, 0, 0, time.UTC)
	m.Now = func() time.Time { return now }
	root := m.ArchiveDir()
	writeFile(t, filepath.Join(root, "2024-01", "j", "meta.json"), []byte("{}"), 0o600)
	if err := os.Chmod(root, 0o755); err != nil {
		t.Fatal(err)
	}
	dry, err := m.Prune(true)
	if err != nil || len(dry.ArchiveExpired) != 0 || dry.RetentionOff != "retention off: no marker in "+root {
		t.Fatalf("dry run: %+v %q %v", dry.ArchiveExpired, dry.RetentionOff, err)
	}
	if _, err := os.Lstat(filepath.Join(root, archiveMarker)); !os.IsNotExist(err) {
		t.Fatalf("dry run wrote the marker: %v", err)
	}
	rep, err := m.Prune(false)
	if err != nil || len(rep.ArchiveExpired) != 1 || rep.RetentionOff != "" {
		t.Fatalf("prune: %+v %q %v", rep.ArchiveExpired, rep.RetentionOff, err)
	}
	if fi, err := os.Lstat(filepath.Join(root, archiveMarker)); err != nil || !fi.Mode().IsRegular() {
		t.Fatalf("marker not written: %v", err)
	}
	if fi, _ := os.Lstat(root); fi.Mode().Perm() != 0o700 {
		t.Fatalf("default root left at %v", fi.Mode().Perm())
	}
	if got := names(root); !slices.Equal(got, []string{archiveMarker}) {
		t.Fatalf("after retention: %v", got)
	}

	other := filepath.Join(t.TempDir(), "synced")
	writeFile(t, filepath.Join(other, "2024-01", "x"), []byte("x"), 0o600)
	m.Cfg.LogArchiveDir = other
	arep, err := m.Archive(false)
	if err != nil || len(arep.Expired) != 0 || arep.RetentionOff != "retention off: no marker in "+other {
		t.Fatalf("configured root: %+v %q %v", arep.Expired, arep.RetentionOff, err)
	}
	if _, err := os.Lstat(filepath.Join(other, archiveMarker)); !os.IsNotExist(err) {
		t.Fatalf("marker written into a configured root: %v", err)
	}
	zero := 0
	m.Cfg.LogArchiveDays = &zero
	if arep, _ := m.Archive(false); arep.RetentionOff != "" {
		t.Fatalf("forever still warns: %q", arep.RetentionOff)
	}
}

// N2: a failed archive keeps the copied records too: a prompt.md over the small-text limit is not
// pruned while it has no archive copy.
func TestPruneWhenArchiveFailsKeepsPrompt(t *testing.T) {
	m := newManager(t)
	writeFile(t, m.ArchiveDir(), []byte("not a dir"), 0o600)
	old := time.Now().Add(-72 * time.Hour)
	insertJob(t, m, "q", store.StatusSucceeded, &old, nil)
	writeFile(t, filepath.Join(m.JobDir("q"), "stream.jsonl"), []byte("{}\n"), 0o600)
	writeFile(t, filepath.Join(m.JobDir("q"), "prompt.md"), bytes.Repeat([]byte("p"), PruneSmallTextBytes+1), 0o600)
	writeFile(t, filepath.Join(m.JobDir("q"), "big.log"), bytes.Repeat([]byte("l"), PruneSmallTextBytes+1), 0o600)
	if _, err := m.Prune(false); err == nil {
		t.Fatal("prune with a broken archive succeeded")
	}
	if got := names(m.JobDir("q")); !slices.Equal(got, []string{"prompt.md", "stream.jsonl"}) {
		t.Fatalf("job dir: %v", got)
	}
}

// N3: meta.json goes through the same synced O_EXCL write as the archived streams.
func TestArchiveSyncsMeta(t *testing.T) {
	m := newManager(t)
	old := time.Now().Add(-72 * time.Hour)
	insertJob(t, m, "y", store.StatusSucceeded, &old, nil)
	writeFile(t, filepath.Join(m.JobDir("y"), "stream.jsonl"), []byte("{}\n"), 0o600)
	var synced []string
	orig := syncFile
	syncFile = func(f *os.File) error {
		if fi, err := f.Stat(); err == nil && fi.Mode().IsRegular() {
			synced = append(synced, filepath.Base(f.Name()))
		}
		return orig(f)
	}
	defer func() { syncFile = orig }()
	if _, err := m.Archive(false); err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(synced, "meta.json") || !slices.Contains(synced, "stream.jsonl.gz") {
		t.Fatalf("synced files: %v", synced)
	}
}
