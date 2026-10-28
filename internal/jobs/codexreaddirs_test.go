package jobs

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/qwwiwi/agent-jobs/internal/store"
)

// A codex job's prompt names the configured read-only dirs as allowed to read, never to write; a
// claude job's prompt and a gate job's do not.
func TestCodexReadDirsInPrompt(t *testing.T) {
	m := newManager(t)
	m.Cfg.CodexReadDirs = []string{"/opt/pnpm-store", "/srv/cache/ms-playwright"}
	codexJob := &store.Job{ID: "j1", Kind: "fix", Runner: store.RunnerCodex, Brief: "b", Title: "t"}
	prompt, err := m.BuildPrompt(codexJob, "rules", nil, MemoryNotes{})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(prompt, "## Read-only directories") || !strings.Contains(prompt, "- `/opt/pnpm-store`\n") ||
		!strings.Contains(prompt, "- `/srv/cache/ms-playwright`\n") || !strings.Contains(prompt, "never write") {
		t.Fatalf("codex prompt lacks the read-only dirs:\n%s", prompt)
	}
	// The exception follows the hard rule it relaxes and comes before the owner rules.
	if strings.Index(prompt, "## Read-only directories") < strings.Index(prompt, "Work only inside your own job directory") ||
		strings.Index(prompt, "## Read-only directories") > strings.Index(prompt, "## Owner rules") {
		t.Fatalf("read-only dirs section misplaced:\n%s", prompt)
	}
	claudeJob := &store.Job{ID: "j2", Kind: "fix", Runner: store.RunnerClaude, Brief: "b", Title: "t"}
	if prompt, err = m.BuildPrompt(claudeJob, "rules", nil, MemoryNotes{}); err != nil || strings.Contains(prompt, "Read-only directories") {
		t.Fatalf("claude prompt names the codex read dirs (err %v):\n%s", err, prompt)
	}
	m.Cfg.CodexReadDirs = nil
	if prompt, err = m.BuildPrompt(codexJob, "rules", nil, MemoryNotes{}); err != nil || strings.Contains(prompt, "Read-only directories") {
		t.Fatalf("codex prompt without codex_read_dirs has the section (err %v):\n%s", err, prompt)
	}
}

func TestCodexReadDirsValidation(t *testing.T) {
	state := filepath.Join(t.TempDir(), "agent")
	for _, bad := range []string{
		`{"codex_read_dirs": ["relative/dir"]}`,
		`{"codex_read_dirs": ["/"]}`,
		`{"codex_read_dirs": ["/opt/x/../y"]}`,
		`{"codex_read_dirs": ["` + filepath.Dir(state) + `"]}`,
		`{"codex_read_dirs": ["` + state + `"]}`,
		`{"codex_read_dirs": ["` + filepath.Join(state, "jobs") + `"]}`,
		`{"codex_read_dirs": ["/tmp/cache"]}`,
	} {
		if _, err := parseConfig(state, []byte(bad)); err == nil || !strings.Contains(err.Error(), "codex_read_dirs") {
			t.Errorf("%s: err = %v, want a codex_read_dirs error", bad, err)
		}
	}
	c, err := parseConfig(state, []byte(`{"codex_read_dirs": ["/home/u/.local/share/pnpm/store", "/home/u/.cache/ms-playwright"]}`))
	if err != nil || len(c.CodexReadDirs) != 2 {
		t.Fatalf("valid codex_read_dirs: %v %v", c.CodexReadDirs, err)
	}
}

// codex_read_dirs changes the prompt only: the codex command line (sandbox, writable roots) is the same.
func TestCodexReadDirsNotInArgv(t *testing.T) {
	j := &store.Job{Model: "gpt-6.1-sol"}
	args := strings.Join(CodexArgs(j, "high", CodexSandboxWrite), " ")
	if strings.Contains(args, "writable_roots") || strings.Contains(args, "--add-dir") {
		t.Fatalf("codex argv grants extra writable roots: %s", args)
	}
}
