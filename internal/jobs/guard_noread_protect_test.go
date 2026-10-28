package jobs

import (
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/qwwiwi/agent-jobs/internal/guard"
)

var reNoRead = regexp.MustCompile(`--no-read '([^']*)'`)

// The merged guard (PR #72 protected paths + Wave F checklist locks) runs both rules from one hook:
// a command touching the shared checklist lock dir and a protected path is refused, in either
// order, through Check and through Decide, and so is each part alone; the protected job gitconfig
// stays readable while the lock dir does not.
func TestJobGuardRefusesChecklistLocksWithProtectedPaths(t *testing.T) {
	m := newManager(t)
	j := protectJob(t, m, "lockprot")
	dir := m.JobDir(j.ID)
	if err := m.writeGitFiles(dir, ""); err != nil {
		t.Fatal(err)
	}
	locks := m.ChecklistLockDir()
	if err := os.MkdirAll(locks, 0o700); err != nil {
		t.Fatal(err)
	}
	lock := filepath.Join(locks, strings.Repeat("cd", 32)+".lock")
	if err := os.WriteFile(lock, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	cfg := jobHookConfig(t, m, j)
	hook := m.guardCommand(j.ID, "", nil, nil, nil)
	for _, g := range reNoRead.FindAllStringSubmatch(hook, -1) {
		cfg.NoRead = append(cfg.NoRead, g[1])
	}
	if len(cfg.NoRead) != 1 || cfg.NoRead[0] != locks {
		t.Fatalf("hook --no-read = %v, want [%s]", cfg.NoRead, locks)
	}
	gc := filepath.Join(dir, gitConfigFile)
	if !strings.Contains(hook, "--protect "+shellQuote(gc)) {
		t.Fatalf("hook does not protect %s: %s", gc, hook)
	}
	ql, qg := shellQuote(lock), shellQuote(gc)
	combined := []string{
		"cat " + ql + "; printf '[diff]\\n' >> " + qg,
		"printf '[diff]\\n' >> " + qg + "; cat " + ql,
		"flock " + ql + " true && chmod u+w " + qg,
		"exec 9<" + ql + "; exec 8<" + qg,
	}
	for _, c := range combined {
		if r := guard.Check(c, cfg); r == "" {
			t.Errorf("Check %q allowed", c)
		}
		b, _ := json.Marshal(map[string]any{"tool_name": "Bash", "cwd": cfg.Dir, "tool_input": map[string]string{"command": c}})
		if r, err := guard.Decide(strings.NewReader(string(b)), cfg); err != nil || r == "" {
			t.Errorf("Decide %q: %q %v, want refused", c, r, err)
		}
	}
	// Each part alone is refused by its own rule.
	if r := guard.Check("cat "+ql, cfg); !strings.Contains(r, "checklist locks") {
		t.Errorf("lock read: %q, want the checklist-lock refusal", r)
	}
	if r := guard.Check("printf x >> "+qg, cfg); !strings.Contains(r, "a job does not write there") {
		t.Errorf("gitconfig write: %q, want the protected-path refusal", r)
	}
	for _, p := range []string{lock, gc} {
		if r := guard.CheckFileWrite("Write", p, cfg); r == "" {
			t.Errorf("Write %s allowed", p)
		}
	}
	if r := guard.CheckFileRead("Read", lock, "", cfg); r == "" {
		t.Error("Read of the lock allowed")
	}
	if r := guard.CheckFileRead("Read", gc, "", cfg); r != "" {
		t.Errorf("Read of the job gitconfig refused: %s", r)
	}
	allowedCmds := []string{"cat " + qg, "echo ok > out.txt"}
	for _, c := range allowedCmds {
		if r := guard.Check(c, cfg); r != "" {
			t.Errorf("%q refused: %s", c, r)
		}
	}
}
