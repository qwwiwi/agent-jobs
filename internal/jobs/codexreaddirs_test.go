package jobs

import (
	"os"
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

// withCodexTmp points the codex temp-dir rule at a fresh dir of its own, so a test's temp dirs (in /tmp
// on most hosts) do not trip it, and returns that dir.
func withCodexTmp(t *testing.T) string {
	t.Helper()
	tmp := filepath.Join(t.TempDir(), "codex-tmp")
	old := codexWritableTmp
	codexWritableTmp = tmp
	t.Cleanup(func() { codexWritableTmp = old })
	return tmp
}

// Each refusal names its own reason, whatever TMPDIR the tests run with.
func TestCodexReadDirsValidation(t *testing.T) {
	tmp := withCodexTmp(t)
	state := filepath.Join(t.TempDir(), "agent")
	for _, c := range []struct{ dir, want string }{
		{"relative/dir", "absolute clean path"},
		{"/", "whole file system"},
		{"/opt/x/../y", "absolute clean path"},
		{filepath.Dir(state), "overlaps the agent-jobs state dir"},
		{state, "overlaps the agent-jobs state dir"},
		{filepath.Join(state, "jobs"), "overlaps the agent-jobs state dir"},
		{filepath.Join(tmp, "cache"), "which a workspace-write codex job can write"},
	} {
		_, err := parseConfig(state, []byte(`{"codex_read_dirs": ["`+c.dir+`"]}`))
		if err == nil || !strings.Contains(err.Error(), "codex_read_dirs") || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: err = %v, want a codex_read_dirs error with %q", c.dir, err, c.want)
		}
	}
	// Outside tests the rule is /tmp.
	codexWritableTmp = "/tmp"
	if _, err := parseConfig(state, []byte(`{"codex_read_dirs": ["/tmp/cache"]}`)); err == nil || !strings.Contains(err.Error(), "is in /tmp") {
		t.Errorf("/tmp/cache: err = %v", err)
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

// An entry that reaches the state dir through a link is refused like the state dir itself.
func TestCodexReadDirsLinkToStateRefused(t *testing.T) {
	withCodexTmp(t)
	base := t.TempDir()
	state := filepath.Join(base, "home", "agent")
	if err := os.MkdirAll(state, 0o700); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(base, "cache")
	if err := os.Symlink(filepath.Join(base, "home"), link); err != nil {
		t.Fatal(err)
	}
	if _, err := parseConfig(state, []byte(`{"codex_read_dirs": ["`+link+`"]}`)); err == nil || !strings.Contains(err.Error(), "resolves to") {
		t.Fatalf("link to the state dir's parent: err = %v", err)
	}
}
