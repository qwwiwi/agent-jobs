package jobs

import (
	"errors"
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
// The one extra writable root is the job's progress dir (#92).
func TestCodexReadDirsNotInArgv(t *testing.T) {
	j := &store.Job{Model: "gpt-6.1-sol"}
	jobDir := t.TempDir()
	argv := CodexArgs(j, "high", CodexSandboxWrite, jobDir, false)
	if args := strings.Join(argv, " "); strings.Contains(args, "writable_roots") {
		t.Fatalf("codex argv grants extra writable roots: %s", args)
	}
	for i, a := range argv {
		if a == "--add-dir" && (i+1 >= len(argv) || argv[i+1] != codexProgressRoot(CodexSandboxWrite, jobDir)) {
			t.Fatalf("codex argv adds a writable root other than the progress dir: %v", argv)
		}
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

// withCodexHome points the secret-dir rule at a fresh home dir (real path, alone in its parent) and
// returns it.
func withCodexHome(t *testing.T) string {
	t.Helper()
	parent, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	home := filepath.Join(parent, "home")
	if err := os.Mkdir(home, 0o700); err != nil {
		t.Fatal(err)
	}
	old := codexReadDirsHome
	codexReadDirsHome = func() (string, error) { return home, nil }
	t.Cleanup(func() { codexReadDirsHome = old })
	return home
}

func mkdirs(t *testing.T, dirs ...string) {
	t.Helper()
	for _, d := range dirs {
		if err := os.MkdirAll(d, 0o700); err != nil {
			t.Fatal(err)
		}
	}
}

func symlink(t *testing.T, target, link string) {
	t.Helper()
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
}

// Secret dirs (credentials, keys, agent config), a dir holding one (the home itself, its parent) and a
// dir inside one are refused, lexically and through links; package caches in the home are not.
func TestCodexReadDirsSecretDirsRefused(t *testing.T) {
	withCodexTmp(t)
	home := withCodexHome(t)
	state := filepath.Join(t.TempDir(), "aj", "agent")
	base := t.TempDir()
	mkdirs(t, state, filepath.Join(home, ".ssh"), filepath.Join(home, ".aws"), filepath.Join(home, ".config", "gh"),
		filepath.Join(home, ".claude-lab", "thrall", "secrets"), filepath.Join(home, ".local", "share", "pnpm", "store"),
		filepath.Join(home, ".cache", "ms-playwright"), filepath.Join(base, "real-gnupg", "keys"))
	if err := os.WriteFile(filepath.Join(home, ".claude.json"), []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
	// ~/.gnupg is a link elsewhere; links outside the home lead to a secret dir and to the home.
	symlink(t, filepath.Join(base, "real-gnupg"), filepath.Join(home, ".gnupg"))
	symlink(t, filepath.Join(home, ".ssh"), filepath.Join(base, "ssh-alias"))
	symlink(t, home, filepath.Join(base, "home-alias"))
	symlink(t, filepath.Join(home, ".aws"), filepath.Join(home, ".cache", "aws-alias"))
	symlink(t, filepath.Join(home, ".claude-lab"), filepath.Join(base, "lab-alias"))
	for _, d := range []string{
		home,
		filepath.Dir(home),
		filepath.Join(home, ".ssh"),
		filepath.Join(home, ".ssh", "keys"),
		filepath.Join(home, ".secrets"), // missing: refused all the same
		filepath.Join(home, ".gnupg"),
		filepath.Join(home, ".aws"),
		filepath.Join(home, ".config"), // holds ~/.config/gh
		filepath.Join(home, ".config", "gh"),
		filepath.Join(home, ".claude"),
		filepath.Join(home, ".claude.json"),
		filepath.Join(home, ".claude-lab"),
		filepath.Join(home, ".claude-lab", "thrall", "secrets"),
		filepath.Join(home, ".claude-other", "cache"),     // any ~/.claude*, existing or not
		filepath.Join(filepath.Dir(state), "other-agent"), // the agent-jobs home: other agents' state
		filepath.Join(base, "real-gnupg"),                 // the real path of the ~/.gnupg link
		filepath.Join(base, "real-gnupg", "keys"),
		filepath.Join(base, "ssh-alias"),
		filepath.Join(base, "home-alias"),
		filepath.Join(home, ".cache", "aws-alias"),
		filepath.Join(base, "lab-alias"),
	} {
		_, err := parseConfig(state, []byte(`{"codex_read_dirs": ["`+d+`"]}`))
		if err == nil || !strings.Contains(err.Error(), "codex_read_dirs") || !strings.Contains(err.Error(), "holds credentials, keys or agent configuration") {
			t.Errorf("%s: err = %v, want a secret-dir refusal", d, err)
		}
	}
	ok := []string{filepath.Join(home, ".local", "share", "pnpm", "store"), filepath.Join(home, ".cache", "ms-playwright"), "/opt/pnpm-store"}
	if _, err := parseConfig(state, []byte(`{"codex_read_dirs": ["`+strings.Join(ok, `", "`)+`"]}`)); err != nil {
		t.Fatalf("package caches refused: %v", err)
	}
}

// A home reached through a link is checked with the link resolved too: its real path's secret dirs
// are refused.
func TestCodexReadDirsLinkedHome(t *testing.T) {
	withCodexTmp(t)
	real := withCodexHome(t)
	link := filepath.Join(t.TempDir(), "home-link")
	symlink(t, real, link)
	codexReadDirsHome = func() (string, error) { return link, nil }
	state := filepath.Join(t.TempDir(), "aj", "agent")
	for _, d := range []string{filepath.Join(real, ".ssh"), filepath.Join(link, ".ssh"), real, filepath.Join(real, ".claude")} {
		if _, err := parseConfig(state, []byte(`{"codex_read_dirs": ["`+d+`"]}`)); err == nil || !strings.Contains(err.Error(), "holds credentials") {
			t.Errorf("%s: err = %v, want a secret-dir refusal", d, err)
		}
	}
}

// Without a known home dir no entry can be checked: codex_read_dirs is refused, an empty one is not.
func TestCodexReadDirsUnknownHome(t *testing.T) {
	withCodexTmp(t)
	old := codexReadDirsHome
	codexReadDirsHome = func() (string, error) { return "", errors.New("$HOME is not defined") }
	t.Cleanup(func() { codexReadDirsHome = old })
	state := filepath.Join(t.TempDir(), "agent")
	if _, err := parseConfig(state, []byte(`{"codex_read_dirs": ["/opt/pnpm-store"]}`)); err == nil || !strings.Contains(err.Error(), "home dir") {
		t.Fatalf("unknown home: err = %v", err)
	}
	if _, err := parseConfig(state, []byte(`{}`)); err != nil {
		t.Fatalf("no codex_read_dirs, unknown home: %v", err)
	}
}

// An entry whose tail does not exist yet is checked with its existing part's links resolved: a link to
// the home with a missing ~/.claude* or secret dir below it, and a dangling link into one, are refused;
// a path that cannot be resolved for another reason (a file on the way, a link loop) is refused too.
func TestCodexReadDirsMissingTailThroughLink(t *testing.T) {
	withCodexTmp(t)
	home := withCodexHome(t)
	state := filepath.Join(t.TempDir(), "aj", "agent")
	base := t.TempDir()
	mkdirs(t, state, filepath.Join(home, ".cache"))
	symlink(t, home, filepath.Join(base, "home-link"))
	symlink(t, filepath.Join(home, ".claude-new"), filepath.Join(base, "dangling"))
	symlink(t, filepath.Join("..", filepath.Base(base), "home-link", ".ssh"), filepath.Join(base, "dotdot-link"))
	// A dangling link whose target has ".." after a name: the name may be a link, so ".." is not lexical.
	symlink(t, "home-link/x/../.ssh", filepath.Join(base, "inner-dotdot"))
	symlink(t, filepath.Join(base, "loop-b"), filepath.Join(base, "loop-a"))
	symlink(t, filepath.Join(base, "loop-a"), filepath.Join(base, "loop-b"))
	if err := os.WriteFile(filepath.Join(base, "file"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	for d, want := range map[string]string{
		filepath.Join(base, "home-link", ".claude-new"):        "holds credentials",
		filepath.Join(base, "home-link", ".ssh", "keys"):       "holds credentials",
		filepath.Join(base, "home-link", ".config", "gh", "x"): "holds credentials",
		filepath.Join(base, "dangling"):                        "holds credentials",
		filepath.Join(base, "dangling", "sub"):                 "holds credentials",
		filepath.Join(base, "dotdot-link", "missing"):          "holds credentials",
		filepath.Join(base, "inner-dotdot"):                    "cannot be resolved",
		filepath.Join(base, "file", "sub"):                     "cannot be resolved",
		filepath.Join(base, "loop-a", "sub"):                   "cannot be resolved",
	} {
		if _, err := parseConfig(state, []byte(`{"codex_read_dirs": ["`+d+`"]}`)); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%s: err = %v, want %q", d, err, want)
		}
	}
	ok := filepath.Join(base, "home-link", ".cache", "pnpm-new")
	if _, err := parseConfig(state, []byte(`{"codex_read_dirs": ["`+ok+`"]}`)); err != nil {
		t.Fatalf("a missing cache dir below a linked home: %v", err)
	}
}

// The denylist's minimum covers the codex, kubernetes, docker and gcloud credentials too.
func TestCodexReadDirsMoreSecretDirs(t *testing.T) {
	withCodexTmp(t)
	home := withCodexHome(t)
	state := filepath.Join(t.TempDir(), "aj", "agent")
	for _, d := range []string{".codex", ".kube", ".docker", filepath.Join(".config", "gcloud"), filepath.Join(".codex", "sessions")} {
		p := filepath.Join(home, d)
		if _, err := parseConfig(state, []byte(`{"codex_read_dirs": ["`+p+`"]}`)); err == nil || !strings.Contains(err.Error(), "holds credentials") {
			t.Errorf("%s: err = %v, want a secret-dir refusal", p, err)
		}
	}
}

// On a case-insensitive file system (macOS) ~/.SSH is ~/.ssh: the paths are compared case-folded there.
func TestCodexReadDirsCaseFolded(t *testing.T) {
	withCodexTmp(t)
	home := withCodexHome(t)
	state := filepath.Join(t.TempDir(), "aj", "agent")
	old := codexReadDirsFoldCase
	t.Cleanup(func() { codexReadDirsFoldCase = old })
	codexReadDirsFoldCase = true
	for _, d := range []string{
		filepath.Join(home, ".SSH"),
		filepath.Join(home, ".Claude-Lab"),
		filepath.Join(home, ".ſsh"), // U+017F (long s) folds to s: ~/.ssh
		filepath.Join(filepath.Dir(home), "HOME"),
		filepath.Join(filepath.Dir(filepath.Dir(state)), "AJ", "other-agent"), // the agent-jobs home
	} {
		if _, err := parseConfig(state, []byte(`{"codex_read_dirs": ["`+d+`"]}`)); err == nil {
			t.Errorf("%s: accepted with case-folded comparison", d)
		}
	}
	codexReadDirsFoldCase = false
	if _, err := parseConfig(state, []byte(`{"codex_read_dirs": ["`+filepath.Join(home, ".SSH")+`"]}`)); err != nil {
		t.Errorf("case-sensitive: %v", err)
	}
}

// On a case-insensitive file system a ~/.CLAUDE-lab link is a ~/.claude* entry like ~/.claude-lab, so
// its target is refused too (#122).
func TestCodexReadDirsUpperCaseClaudeLinkTarget(t *testing.T) {
	withCodexTmp(t)
	home := withCodexHome(t)
	state := filepath.Join(t.TempDir(), "aj", "agent")
	target := filepath.Join(filepath.Dir(home), "agent-config")
	mkdirs(t, filepath.Join(target, "secrets"))
	symlink(t, target, filepath.Join(home, ".CLAUDE-lab"))
	old := codexReadDirsFoldCase
	t.Cleanup(func() { codexReadDirsFoldCase = old })
	codexReadDirsFoldCase = true
	for _, d := range []string{target, filepath.Join(target, "secrets")} {
		if _, err := parseConfig(state, []byte(`{"codex_read_dirs": ["`+d+`"]}`)); err == nil || !strings.Contains(err.Error(), "holds credentials") {
			t.Errorf("%s: err = %v, want a secret-dir refusal", d, err)
		}
	}
}
