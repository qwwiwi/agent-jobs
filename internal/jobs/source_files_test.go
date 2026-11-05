package jobs

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/qwwiwi/agent-jobs/internal/guard"
	"github.com/qwwiwi/agent-jobs/internal/store"
)

func sourceFixture(t *testing.T, dir, name, content string) string {
	t.Helper()
	// macOS temp roots may be reached through /var or /tmp symlinks.
	dir, err := filepath.EvalSymlinks(dir)
	if err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(dir, name)
	if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestSourceFilesCopyAndPrompt(t *testing.T) {
	m := newManager(t)
	first := sourceFixture(t, t.TempDir(), "report.md", "first report")
	second := sourceFixture(t, t.TempDir(), "report.md", "second report")
	punctuation := sourceFixture(t, t.TempDir(), "notes, final.md", "third report")
	prior := enqueue(t, m, "previous job", nil)
	if err := os.MkdirAll(m.JobDir(prior.ID), 0o700); err != nil {
		t.Fatal(err)
	}
	sourceFixture(t, m.JobDir(prior.ID), "report.md", "prior report")
	origins := []string{first, second, punctuation, "job:" + prior.ID}
	j, err := m.Enqueue(StartRequest{Title: "review", Kind: "review", Brief: "Read the supplied reports", Model: "sonnet", Sources: origins})
	if err != nil {
		t.Fatalf("local sources must be accepted: %v", err)
	}
	// Enqueue has returned: all copies must already exist, even before any daemon tick.
	names := []string{"report.md", "report-2.md", "notes, final.md", prior.ID + "-report.md"}
	contents := []string{"first report", "second report", "third report", "prior report"}
	for i, name := range names {
		p := filepath.Join(m.JobDir(j.ID), "sources", name)
		b, err := os.ReadFile(p)
		if err != nil || string(b) != contents[i] {
			t.Fatalf("copy %s: %q, %v", name, b, err)
		}
		fi, err := os.Stat(p)
		if err != nil || fi.Mode().Perm()&0o222 != 0 {
			t.Fatalf("copy is not read-only: %s (%v)", name, err)
		}
	}
	// A fresh DB read must preserve origins with commas and spaces for later launch/resume.
	j = get(t, m, j.ID)
	prompt, err := m.BuildPrompt(j, "", nil, MemoryNotes{})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(prompt, "Sources (read-only copies):") {
		t.Fatal("prompt lacks the sources heading")
	}
	for i, name := range names {
		want := "sources/" + name + " – from " + origins[i]
		if !strings.Contains(prompt, want) {
			t.Errorf("prompt lacks %q", want)
		}
	}
	if needsClone(j) {
		t.Fatal("file copies must never go through clone prefetch")
	}
	if _, err := m.clonedSources(j); err != nil {
		t.Fatalf("local copies must not need a clone manifest: %v", err)
	}
	// Changing an origin after start cannot change its snapshot.
	sourceFixture(t, filepath.Dir(first), filepath.Base(first), "changed")
	b, err := os.ReadFile(filepath.Join(m.JobDir(j.ID), "sources", names[0]))
	if err != nil || string(b) != contents[0] {
		t.Fatalf("copy changed with its origin: %q, %v", b, err)
	}
}

func TestSourceFilesValidation(t *testing.T) {
	m := newManager(t)
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	good := sourceFixture(t, dir, "good.md", "report")
	big := sourceFixture(t, dir, "large.md", strings.Repeat("x", (2<<20)+1))
	linked := filepath.Join(dir, "link.md")
	if err := os.Symlink(good, linked); err != nil {
		t.Fatal(err)
	}
	linkedDir := filepath.Join(t.TempDir(), "linked")
	if err := os.Symlink(dir, linkedDir); err != nil {
		t.Fatal(err)
	}
	foreign := enqueue(t, m, "foreign job", nil)
	// Agent identity is checked from the stored row, not inferred from a supplied path.
	foreign.ID += "-foreign"
	foreign.Agent = "another-agent"
	if err := m.Store.Insert(foreign); err != nil {
		t.Fatal(err)
	}
	withoutReport := enqueue(t, m, "no report yet", nil)
	tooMany := make([]string, 11)
	for i := range tooMany {
		tooMany[i] = good
	}
	for _, tc := range []struct {
		name string
		in   []string
		want string
	}{
		{"missing", []string{filepath.Join(dir, "missing.md")}, "no such file"},
		{"too big", []string{big}, "2097152"},
		{"unknown job", []string{"job:unknown"}, "unknown"},
		{"foreign job", []string{"job:" + foreign.ID}, "same agent"},
		{"missing report", []string{"job:" + withoutReport.ID}, "report.md"},
		{"too many", tooMany, "at most 10"},
		{"relative", []string{"report.md"}, "absolute"},
		{"directory", []string{dir}, "regular file"},
		{"symlink", []string{linked}, "symlink"},
		{"symlink parent", []string{filepath.Join(linkedDir, "good.md")}, "symlink"},
		{"bad job id", []string{"job:../other"}, "job id"},
		{"empty", []string{""}, "absolute"},
		{"control character", []string{good + "\n"}, "control characters"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			before := countJobs(t, m)
			_, err := m.Enqueue(StartRequest{Title: "review", Kind: "review", Brief: "read", Model: "sonnet", Sources: tc.in})
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("want %q validation error, got %v", tc.want, err)
			}
			if n := countJobs(t, m); n != before {
				t.Fatalf("invalid sources queued a job: %d -> %d", before, n)
			}
		})
	}
}

func TestSourceFilesBoundariesAndNames(t *testing.T) {
	m := newManager(t)
	empty := sourceFixture(t, t.TempDir(), "report.md", "")
	full := sourceFixture(t, t.TempDir(), "report.md", strings.Repeat("x", 2<<20))
	collision := sourceFixture(t, t.TempDir(), "report-2.md", "suffix collision")
	caseCollision := sourceFixture(t, t.TempDir(), "REPORT.md", "case collision")
	origins := []string{empty, full, collision, caseCollision, empty, empty, empty, empty, empty, empty}
	j := enqueue(t, m, "read", func(r *StartRequest) { r.Kind, r.Sources = "review", origins })
	entries, err := os.ReadDir(filepath.Join(m.JobDir(j.ID), "sources"))
	if err != nil || len(entries) != 10 {
		t.Fatalf("exactly ten sources must be accepted: %d, %v", len(entries), err)
	}
	for name, size := range map[string]int64{"report.md": 0, "report-2.md": 2 << 20, "report-2-2.md": 16, "REPORT-3.md": 14} {
		fi, err := os.Stat(filepath.Join(m.JobDir(j.ID), "sources", name))
		if err != nil || fi.Size() != size {
			t.Errorf("boundary or collision copy %s (want %d bytes): %v", name, size, err)
		}
	}
}

func TestSourceFilesInsertFailureCleanup(t *testing.T) {
	m := newManager(t)
	one := 1
	m.Cfg.CodexJobsPerDay = &one
	enqueue(t, m, "first", codexStart("gpt-5.5"))
	origin := sourceFixture(t, t.TempDir(), "report.md", "report")
	_, err := m.Enqueue(StartRequest{Title: "over cap", Kind: "review", Brief: "read", Model: "gpt-5.5", Sources: []string{origin}})
	if err == nil || !strings.Contains(err.Error(), "daily cap") {
		t.Fatalf("expected capped insertion failure: %v", err)
	}
	entries, err := os.ReadDir(filepath.Join(m.StateDir, "jobs"))
	if err != nil || len(entries) != 0 {
		t.Fatalf("failed insertion left a source folder: %v, %v", entries, err)
	}
	if n := countJobs(t, m); n != 1 {
		t.Fatalf("failed insertion queued a second job: %d", n)
	}
}

func TestSourceFilesGuard(t *testing.T) {
	m := newManager(t)
	prior := enqueue(t, m, "prior", nil)
	if err := os.MkdirAll(m.JobDir(prior.ID), 0o700); err != nil {
		t.Fatal(err)
	}
	origin := sourceFixture(t, m.JobDir(prior.ID), "report.md", "prior report")
	j := enqueue(t, m, "review", func(r *StartRequest) { r.Kind, r.Sources = "review", []string{"job:" + prior.ID} })
	src := filepath.Join(m.JobDir(j.ID), "sources")
	copyPath := filepath.Join(src, prior.ID+"-report.md")
	settings, err := json.Marshal(m.JobSettings(j))
	if err != nil || !strings.Contains(string(settings), "--protect "+shellQuote(src)) {
		t.Fatalf("source copies missing from protected paths: %v", err)
	}
	cfg := guard.Config{StateDir: m.StateDir, OwnDir: m.JobDir(j.ID), Dir: filepath.Join(m.JobDir(j.ID), "work"), Protected: []string{src}}
	for _, tc := range []struct {
		tool, path string
		blocked    bool
	}{{"Read", copyPath, false}, {"Read", origin, true}, {"Write", copyPath, true}, {"Edit", copyPath, true}} {
		in, _ := json.Marshal(map[string]any{"tool_name": tc.tool, "tool_input": map[string]string{"file_path": tc.path}})
		reason, err := guard.Decide(bytes.NewReader(in), cfg)
		if err != nil || (reason != "") != tc.blocked {
			t.Errorf("%s %s: blocked=%v, reason=%q, err=%v", tc.tool, tc.path, tc.blocked, reason, err)
		}
	}
}

func TestSourceFilesCodex(t *testing.T) {
	m := newManager(t)
	p := sourceFixture(t, t.TempDir(), "input.md", "review input")
	j, err := m.Enqueue(StartRequest{Title: "review", Kind: "review", Brief: "read", Model: "gpt-5.5", Sources: []string{p}})
	if err != nil {
		t.Fatalf("codex must accept local source copies: %v", err)
	}
	if j.Runner != store.RunnerCodex {
		t.Fatalf("runner = %s", j.Runner)
	}
}
