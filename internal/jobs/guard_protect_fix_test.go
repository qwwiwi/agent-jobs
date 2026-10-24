package jobs

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/qwwiwi/agent-jobs/internal/guard"
)

// REDPROOF 6: owner_rules_path and cards_dir are read by the daemon, the MCP server and the guard
// hook, each in a directory of its own: a relative value would name a different file in each, so it
// is refused at config load.
func TestRelativeOwnerRulesAndCardsRefused(t *testing.T) {
	state := t.TempDir()
	for _, body := range []string{`{"owner_rules_path":"owner-rules.md"}`, `{"cards_dir":"cards"}`, `{"cards_dir":"./c"}`} {
		_, err := parseConfig(state, []byte(body))
		if err == nil || !strings.Contains(err.Error(), "absolute") {
			t.Errorf("%s: want an absolute-path error, got %v", body, err)
		}
	}
	c, err := parseConfig(state, []byte(`{"owner_rules_path":"/etc/rules.md","cards_dir":"/srv/cards"}`))
	if err != nil || c.OwnerRulesPath != "/etc/rules.md" || c.CardsDir != "/srv/cards" {
		t.Errorf("absolute paths: %+v %v", c, err)
	}
}

// REDPROOF 7 (job side): the job's settings.json (its guard hook and permissions) and mcp.json are
// protected like its gitconfig, and the hook keeps file tools in the job's own folder; the files a
// job writes there (progress, report, scratch, its work tree) stay writable.
func TestJobGuardProtectsJobSettings(t *testing.T) {
	m := newManager(t)
	j := protectJob(t, m, "sets")
	dir := m.JobDir(j.ID)
	other := protectJob(t, m, "peer")
	cfg := jobHookConfig(t, m, j)
	settings, mcp := filepath.Join(dir, "settings.json"), filepath.Join(dir, "mcp.json")
	peer := filepath.Join(m.JobDir(other.ID), "report.md")
	var cmds []string
	for _, p := range []string{settings, mcp} {
		cmds = append(cmds, bashWrites(p)...)
	}
	assertWritesRefused(t, cfg, []string{settings, mcp, peer, filepath.Join(m.StateDir, "goals.md")}, cmds)
	for _, p := range []string{"PROGRESS.md", "report.md", "tmp/x.py"} {
		if r := guard.CheckFileWrite("Write", filepath.Join(dir, p), cfg); r != "" {
			t.Errorf("Write %s refused: %s", p, r)
		}
	}
	if r := guard.Check("cat "+shellQuote(settings), cfg); r != "" {
		t.Errorf("reading settings.json refused: %s", r)
	}
}

// REDPROOF 1 and 3 (job side): through the hook of a real job, the descriptor and the exported
// variable spellings of the job's gitconfig are refused.
func TestJobGuardGitconfigSecondNames(t *testing.T) {
	m := newManager(t)
	j := protectJob(t, m, "alias")
	dir := m.JobDir(j.ID)
	if err := m.writeGitFiles(dir); err != nil {
		t.Fatal(err)
	}
	cfg := jobHookConfig(t, m, j)
	gc := filepath.Join(dir, gitConfigFile)
	t.Setenv("GIT_CONFIG_GLOBAL", gc)
	t.Setenv("GIT_CONFIG_VALUE_0", filepath.Join(dir, noHooksDir))
	for _, c := range []string{
		"exec 9<" + shellQuote(gc) + "; chmod u+w /proc/self/fd/9; printf '[diff]\\n' >> /proc/self/fd/9",
		"ln -L /dev/stdin alias <" + shellQuote(gc), `chmod u+w "$GIT_CONFIG_GLOBAL"`, `echo x >> $GIT_CONFIG_GLOBAL`,
		`cp /tmp/evil "${GIT_CONFIG_VALUE_0}/pre-commit"`,
	} {
		if r := guard.Check(c, cfg); r == "" {
			t.Errorf("%q allowed", c)
		}
	}
	if _, err := os.Stat(gc); err != nil {
		t.Fatal(err)
	}
}

// bashWrites aimed at an ordinary file of the work dir pass the same hook: the refusals of the
// protected paths come from the protected-path rule, not from another rule (such as the git config
// key allowlist, which refuses diff.external anywhere).
func TestBashWritesAllowedElsewhere(t *testing.T) {
	m := newManager(t)
	j := protectJob(t, m, "ctl")
	cfg := jobHookConfig(t, m, j)
	for _, c := range bashWrites(filepath.Join(cfg.Dir, "plain.cfg")) {
		if r := guard.Check(c, cfg); r != "" {
			t.Errorf("%q refused: %s", c, r)
		}
	}
}
