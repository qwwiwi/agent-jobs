package jobs

import (
	"bytes"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"golang.org/x/sys/unix"

	"github.com/qwwiwi/agent-jobs/internal/store"
)

// insertJob adds a row directly (no launch) with the given status and finish time.
func insertJob(t *testing.T, m *Manager, id, status string, finished *time.Time, mod func(*store.Job)) *store.Job {
	t.Helper()
	now := m.Now()
	started := now.Add(-time.Minute)
	if finished != nil {
		started = finished.Add(-time.Minute)
	}
	j := &store.Job{ID: id, Agent: m.Agent, Title: "t " + id, Kind: "general", Brief: "b", Status: status,
		Model: "sonnet", MaxTurns: 80, MaxBudgetUSD: 10, TimeoutMinutes: 45, CreatedAt: started, StartedAt: &started,
		FinishedAt: finished}
	if mod != nil {
		mod(j)
	}
	if err := m.Store.Insert(j); err != nil {
		t.Fatal(err)
	}
	return j
}

func writeFile(t *testing.T, p string, b []byte, mode os.FileMode) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, b, mode); err != nil {
		t.Fatal(err)
	}
}

// fillJobDir lays out a finished job folder: records, small text files, a large text file, a binary,
// a work dir with caches and a read-only subdir, the read-only no-hooks dir and a symlink out.
func fillJobDir(t *testing.T, dir, outside string) {
	t.Helper()
	big := bytes.Repeat([]byte("report line\n"), (PruneSmallTextBytes/12)+10) // > 1 MiB: a record stays anyway
	writeFile(t, filepath.Join(dir, "report.md"), big, 0o600)
	for _, f := range []string{"base_commit", "commits.patch", "staged.patch", "uncommitted.patch", "diffstat.txt"} {
		writeFile(t, filepath.Join(dir, f), []byte(f+"\n"), 0o600)
	}
	writeFile(t, filepath.Join(dir, "untracked.tar"), []byte("bin\x00ary tar"), 0o600) // a record, though binary
	writeFile(t, filepath.Join(dir, "prompt.md"), []byte("# brief\nпривет\n"), 0o600)
	writeFile(t, filepath.Join(dir, "gitconfig"), []byte("[user]\n"), 0o400)
	writeFile(t, filepath.Join(dir, "stream.jsonl"), bytes.Repeat([]byte(`{"type":"x"}`+"\n"), PruneSmallTextBytes/13+10), 0o600)
	writeFile(t, filepath.Join(dir, "a.out"), []byte("\x7fELF\x00\x00\x01"), 0o700)
	writeFile(t, filepath.Join(dir, "work", "node_modules", "x", "index.js"), []byte("x"), 0o600)
	writeFile(t, filepath.Join(dir, "work", "ro", "file"), []byte("x"), 0o600)
	if err := os.Chmod(filepath.Join(dir, "work", "ro"), 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(filepath.Join(dir, "work", "ro"), 0o700) }) // for t.TempDir's removal
	if err := os.Mkdir(filepath.Join(dir, "no-hooks"), 0o500); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(dir, "link-out")); err != nil {
		t.Fatal(err)
	}
}

func names(dir string) []string {
	ents, _ := os.ReadDir(dir)
	var out []string
	for _, e := range ents {
		out = append(out, e.Name())
	}
	slices.Sort(out)
	return out
}

func TestPruneOneKeepsSessionsWhileAcceptanceGroupAlive(t *testing.T) {
	for _, kind := range []string{"session-only", "cleaned-repo"} {
		t.Run(kind, func(t *testing.T) {
			m := newManager(t)
			cfg := sessionsHome(t, false)
			old := m.Now().Add(-PruneAge - time.Hour)
			const pgid = 1 << 22
			j := insertJob(t, m, "acceptance", store.StatusFailed, &old, func(j *store.Job) {
				j.SessionID = NewSessionID()
				j.AcceptancePGID = pgid
				if kind == "cleaned-repo" {
					j.RepoPath = t.TempDir()
					j.CleanedAt = &old
				}
			})
			writeFile(t, filepath.Join(m.JobDir(j.ID), "prompt.md"), []byte("test"), 0o600)
			transcript := filepath.Join(cfg, "projects", "-test", j.SessionID+".jsonl")
			writeFile(t, transcript, []byte("{}\n"), 0o600)
			live := true
			oldExists := groupExists
			groupExists = func(got int) bool { return got == pgid && live }
			t.Cleanup(func() { groupExists = oldExists })
			cutoff := m.Now().Add(-PruneAge)
			pj, ok, err := m.pruneOne(j.ID, cutoff, false)
			if err != nil || !ok || pj.Error != "" || !strings.Contains(pj.Skipped, "acceptance process group") || len(pj.Removed) != 0 {
				t.Fatalf("prune with live acceptance group: %+v, ok %v, err %v", pj, ok, err)
			}
			if got := readFile(t, transcript); got != "{}\n" {
				t.Fatalf("live acceptance group lost transcript contents: %q", got)
			}
			live = false
			pj, ok, err = m.pruneOne(j.ID, cutoff, false)
			if err != nil || !ok || pj.Error != "" || pj.Skipped != "" {
				t.Fatalf("prune after acceptance group exits: %+v, ok %v, err %v", pj, ok, err)
			}
			if _, err := os.Stat(transcript); !os.IsNotExist(err) {
				t.Fatalf("transcript not pruned after acceptance group exits: %v", err)
			}
			if !slices.ContainsFunc(pj.Removed, func(e PruneEntry) bool { return e.Path == transcript }) {
				t.Fatalf("pruned transcript missing from report: %+v", pj)
			}
		})
	}
}

// Reference, «Как мы убираем»: prune cleans folders of jobs finished more than 48 h ago; records
// (report, patches, base_commit) and small top-level text files stay; binaries, caches, builds go.
func TestPruneKeepsRecordsDropsTheRest(t *testing.T) {
	m := newManager(t)
	now := time.Now()
	m.Now = func() time.Time { return now }
	old, recent := now.Add(-49*time.Hour), now.Add(-47*time.Hour)

	outside := filepath.Join(t.TempDir(), "outside")
	writeFile(t, filepath.Join(outside, "keep-me"), []byte("not the job's"), 0o600)

	insertJob(t, m, "old", store.StatusSucceeded, &old, nil)
	fillJobDir(t, m.JobDir("old"), outside)
	insertJob(t, m, "recent", store.StatusFailed, &recent, nil)
	fillJobDir(t, m.JobDir("recent"), outside)
	insertJob(t, m, "running", store.StatusRunning, nil, nil)
	fillJobDir(t, m.JobDir("running"), outside)
	// A repo job whose cleanup never ran: its patches are not saved, so nothing is touched.
	insertJob(t, m, "uncleaned", store.StatusTimeout, &old, func(j *store.Job) { j.RepoPath = "/repo" })
	fillJobDir(t, m.JobDir("uncleaned"), outside)
	// A repo job whose cleanup kept the worktree for a human (repository drivers): work/ stays.
	insertJob(t, m, "kept", store.StatusCancelled, &old, func(j *store.Job) { j.RepoPath = "/repo"; j.CleanedAt = &old })
	fillJobDir(t, m.JobDir("kept"), outside)
	writeFile(t, filepath.Join(m.JobDir("kept"), "work", ".git"), []byte("gitdir: /repo/.git/worktrees/x\n"), 0o600)

	want := []string{"base_commit", "commits.patch", "diffstat.txt", "gitconfig", "prompt.md", "report.md",
		"staged.patch", "uncommitted.patch", "untracked.tar"}
	before := map[string][]string{}
	for _, id := range []string{"old", "recent", "running", "uncleaned", "kept"} {
		before[id] = names(m.JobDir(id))
	}

	dry, err := m.Prune(true)
	if err != nil {
		t.Fatal(err)
	}
	for id, n := range before {
		if got := names(m.JobDir(id)); !slices.Equal(got, n) {
			t.Fatalf("dry run changed %s: %v -> %v", id, n, got)
		}
	}
	rep, err := m.Prune(false)
	if err != nil {
		t.Fatal(err)
	}
	if rep.Bytes == 0 || rep.Bytes != dry.Bytes {
		t.Fatalf("bytes: dry %d, real %d", dry.Bytes, rep.Bytes)
	}
	if got := names(m.JobDir("old")); !slices.Equal(got, want) {
		t.Fatalf("old job after prune: %v, want %v", got, want)
	}
	for _, id := range []string{"recent", "running", "uncleaned"} {
		if got := names(m.JobDir(id)); !slices.Equal(got, before[id]) {
			t.Fatalf("%s must be untouched: %v -> %v", id, before[id], got)
		}
	}
	if got := names(m.JobDir("kept")); !slices.Contains(got, "work") || slices.Contains(got, "a.out") {
		t.Fatalf("kept: the worktree stays, the rest goes: %v", got)
	}
	if b, err := os.ReadFile(filepath.Join(outside, "keep-me")); err != nil || string(b) != "not the job's" {
		t.Fatalf("symlink target outside the job dir was touched: %q %v", b, err)
	}
	var skippedUncleaned bool
	for _, j := range rep.Jobs {
		if j.ID == "recent" || j.ID == "running" {
			t.Fatalf("job %s must not be in the report", j.ID)
		}
		if j.ID == "uncleaned" && j.Skipped != "" && len(j.Removed) == 0 {
			skippedUncleaned = true
		}
	}
	if !skippedUncleaned {
		t.Fatalf("uncleaned repo job must be reported as skipped: %+v", rep.Jobs)
	}
	// Idempotent: a second pass removes nothing.
	again, err := m.Prune(false)
	if err != nil || again.Bytes != 0 {
		t.Fatalf("second prune: %d bytes, %v", again.Bytes, err)
	}
}

// A job may chmod its own folder read-only; prune still empties it (only below the job dir).
func TestPruneReadOnlyJobDir(t *testing.T) {
	m := newManager(t)
	old := time.Now().Add(-72 * time.Hour)
	insertJob(t, m, "ro", store.StatusSucceeded, &old, nil)
	dir := m.JobDir("ro")
	writeFile(t, filepath.Join(dir, "report.md"), []byte("r"), 0o600)
	writeFile(t, filepath.Join(dir, "work", "build", "out.bin"), []byte("\x00"), 0o600)
	if err := os.Chmod(dir, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(dir, 0o700) })
	if _, err := m.Prune(false); err != nil {
		t.Fatal(err)
	}
	if got := names(dir); !slices.Equal(got, []string{"report.md"}) {
		t.Fatalf("got %v", got)
	}
}

// The retry chmod never follows a symlink: not one named as the tree, not one inside it. Real
// read-only directories inside the tree are opened up.
func TestChmodTreeNoFollow(t *testing.T) {
	base := t.TempDir()
	outside := filepath.Join(base, "outside")
	tree := filepath.Join(base, "tree")
	for _, d := range []string{outside, filepath.Join(tree, "a", "b")} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Symlink(outside, filepath.Join(tree, "a", "link")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(base, "tree-link")); err != nil {
		t.Fatal(err)
	}
	for _, d := range []string{outside, filepath.Join(tree, "a", "b"), filepath.Join(tree, "a"), tree} {
		if err := os.Chmod(d, 0o500); err != nil {
			t.Fatal(err)
		}
	}
	t.Cleanup(func() {
		for _, d := range []string{outside, tree, filepath.Join(tree, "a"), filepath.Join(tree, "a", "b")} {
			_ = os.Chmod(d, 0o700)
		}
	})
	mode := func(p string) os.FileMode {
		fi, err := os.Lstat(p)
		if err != nil {
			t.Fatal(err)
		}
		return fi.Mode().Perm()
	}
	chmodTree(unix.AT_FDCWD, filepath.Join(base, "tree-link"), 0)
	if got := mode(outside); got != 0o500 {
		t.Fatalf("symlink named as the tree was followed: outside is %o", got)
	}
	chmodTree(unix.AT_FDCWD, tree, 0)
	for _, d := range []string{tree, filepath.Join(tree, "a"), filepath.Join(tree, "a", "b")} {
		if got := mode(d); got != 0o700 {
			t.Fatalf("%s is %o, want 700", d, got)
		}
	}
	if got := mode(outside); got != 0o500 {
		t.Fatalf("symlink inside the tree was followed: outside is %o", got)
	}
}

// The prune CLI: --dry-run lists and removes nothing; without it the folder is pruned.
func TestPruneCLI(t *testing.T) {
	m := newManager(t)
	old := time.Now().Add(-50 * time.Hour)
	insertJob(t, m, "cli", store.StatusSucceeded, &old, nil)
	writeFile(t, filepath.Join(m.JobDir("cli"), "report.md"), []byte("r"), 0o600)
	writeFile(t, filepath.Join(m.JobDir("cli"), "work", "x.bin"), []byte("\x00"), 0o600)
	run := func(args ...string) string {
		out, err := execSelf(args...)
		if err != nil {
			t.Fatalf("%v: %v\n%s", args, err, out)
		}
		return out
	}
	if out := run("prune", "--agent", "test", "--dry-run"); !bytes.Contains([]byte(out), []byte("would remove")) {
		t.Fatalf("dry run output: %s", out)
	}
	if got := names(m.JobDir("cli")); !slices.Contains(got, "work") {
		t.Fatalf("dry run removed: %v", got)
	}
	run("prune", "--agent", "test")
	if got := names(m.JobDir("cli")); !slices.Equal(got, []string{"report.md"}) {
		t.Fatalf("after prune: %v", got)
	}
}
