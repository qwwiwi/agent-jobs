package jobs

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/qwwiwi/agent-jobs/internal/guard"
	"github.com/qwwiwi/agent-jobs/internal/store"
)

var reProtect = regexp.MustCompile(`--protect '([^']*)'`)

// jobHookConfig is the guard config the PreToolUse hook of job j's settings runs with: the hook's
// --protect paths, the job's work dir as the call's directory, the state dir and the job's folder.
func jobHookConfig(t *testing.T, m *Manager, j *store.Job) guard.Config {
	t.Helper()
	s := m.JobSettings(j)
	pre := s["hooks"].(map[string]any)["PreToolUse"].([]any)[0].(map[string]any)
	hook := pre["hooks"].([]any)[0].(map[string]any)["command"].(string)
	cfg := guard.Config{Dir: filepath.Join(m.JobDir(j.ID), "work"), StateDir: m.StateDir, OwnDir: m.JobDir(j.ID)}
	for _, g := range reProtect.FindAllStringSubmatch(hook, -1) {
		cfg.Protected = append(cfg.Protected, g[1])
	}
	return cfg
}

func protectJob(t *testing.T, m *Manager, id string) *store.Job {
	t.Helper()
	j := insertJob(t, m, id, store.StatusRunning, nil, func(j *store.Job) { j.Kind = "fix" })
	if err := os.MkdirAll(filepath.Join(m.JobDir(j.ID), "work"), 0o700); err != nil {
		t.Fatal(err)
	}
	return j
}

// assertWritesRefused checks that every file tool and every Bash write of paths is refused, and that
// plain reads and ordinary writes in the work dir stay allowed.
func assertWritesRefused(t *testing.T, cfg guard.Config, paths []string, cmds []string) {
	t.Helper()
	for _, p := range paths {
		for _, tool := range []string{"Write", "Edit", "MultiEdit", "NotebookEdit"} {
			if r := guard.CheckFileWrite(tool, p, cfg); r == "" {
				t.Errorf("%s %s allowed", tool, p)
			}
		}
	}
	for _, c := range cmds {
		if r := guard.Check(c, cfg); r == "" {
			t.Errorf("%q allowed", c)
		}
	}
	for _, c := range []string{"echo hi > out.txt", "go test ./...", "git status"} {
		if r := guard.Check(c, cfg); r != "" {
			t.Errorf("%q refused: %s", c, r)
		}
	}
	if r := guard.CheckFileWrite("Write", filepath.Join(cfg.Dir, "main.go"), cfg); r != "" {
		t.Errorf("Write in the work dir refused: %s", r)
	}
}

// bashWrites are the shell writes of p a job could try: redirections, copy/move/link onto it, mode
// changes, in-place edits and git config --file (with an allowlisted key, so only the protected-path
// rule refuses it: TestBashWritesAllowedElsewhere).
func bashWrites(p string) []string {
	q := shellQuote(p)
	return []string{
		"echo '{\"max_running\":9}' > " + q, "printf x >> " + q, "tee " + q + " < /dev/null",
		"cp /etc/hostname " + q, "mv /tmp/x " + q, "ln -sf /tmp/x " + q, "chmod u+w " + q,
		"sed -i s/a/b/ " + q, "truncate -s0 " + q, "git config --file " + q + " core.quotepath false",
	}
}

// REDPROOF 1: the config files the daemon and the MCP server load (<state>/config.json, hot-reloaded
// each tick; the owner rules and the project cards glued into every brief, also at configured
// paths outside the state dir) are protected from a job's file tools and Bash writes.
func TestJobGuardProtectsDaemonConfig(t *testing.T) {
	m := newManager(t)
	ext := t.TempDir()
	m.Cfg.OwnerRulesPath = filepath.Join(ext, "rules.md")
	m.Cfg.CardsDir = filepath.Join(ext, "cards")
	for _, f := range []string{filepath.Join(m.StateDir, "config.json"), m.Cfg.OwnerRulesPath} {
		if err := os.WriteFile(f, []byte("{}"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	j := protectJob(t, m, "cfg")
	cfg := jobHookConfig(t, m, j)
	// A link to the state dir planted in the work dir does not get around it.
	if err := os.Symlink(m.StateDir, filepath.Join(cfg.Dir, "st")); err != nil {
		t.Fatal(err)
	}
	conf := filepath.Join(m.StateDir, "config.json")
	paths := []string{conf, "st/config.json", m.Cfg.OwnerRulesPath, filepath.Join(m.Cfg.CardsDir, "repo.md")}
	var cmds []string
	for _, p := range paths {
		cmds = append(cmds, bashWrites(p)...)
	}
	assertWritesRefused(t, cfg, paths, cmds)
}

// REDPROOF 2: <jobdir>/gitconfig (GIT_CONFIG_GLOBAL of the job) and <jobdir>/no-hooks (its
// core.hooksPath) are protected by the guard, not only by their 0400/0500 modes, which the job
// owns and can change (`chmod u+w`, then diff.external runs a program).
func TestJobGuardProtectsJobGitFiles(t *testing.T) {
	m := newManager(t)
	j := protectJob(t, m, "gitf")
	dir := m.JobDir(j.ID)
	if err := m.writeGitFiles(dir, ""); err != nil {
		t.Fatal(err)
	}
	cfg := jobHookConfig(t, m, j)
	gc, hooks := filepath.Join(dir, gitConfigFile), filepath.Join(dir, noHooksDir)
	paths := []string{gc, filepath.Join(hooks, "pre-commit")}
	cmds := append(bashWrites(gc), bashWrites(filepath.Join(hooks, "pre-commit"))...)
	cmds = append(cmds, "chmod u+w "+shellQuote(hooks), "chmod -R u+w "+shellQuote(dir),
		"cp /tmp/evil "+shellQuote(hooks)+"/", "rm -r "+shellQuote(hooks), "mv "+shellQuote(hooks)+" /tmp/h",
		"ln -s /tmp/evil "+shellQuote(hooks)+"/pre-commit", "cd .. && chmod u+w gitconfig", "chmod u+w ../gitconfig",
		// The job folder holds both, so it is no copy or move destination either.
		"cp /tmp/gitconfig "+shellQuote(dir)+"/", "mv /tmp/no-hooks "+shellQuote(dir))
	assertWritesRefused(t, cfg, paths, cmds)
	// The job's own folder stays writable where it is told to write.
	if r := guard.Check("cat > "+shellQuote(filepath.Join(dir, "PROGRESS.md"))+" <<'EOF'\nstep\nEOF", cfg); r != "" {
		t.Errorf("PROGRESS.md refused: %s", r)
	}
	if r := guard.CheckFileWrite("Write", filepath.Join(dir, "PROGRESS.md"), cfg); r != "" {
		t.Errorf("Write PROGRESS.md refused: %s", r)
	}
	if r := guard.Check("cat "+shellQuote(gc), cfg); r != "" {
		t.Errorf("reading gitconfig refused: %s", r)
	}
	if !strings.Contains(guard.CheckFileWrite("Write", gc, cfg), "git") {
		t.Errorf("refusal does not name the job's git files: %s", guard.CheckFileWrite("Write", gc, cfg))
	}
}
