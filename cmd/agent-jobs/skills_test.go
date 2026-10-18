package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/qwwiwi/agent-jobs/internal/jobs"
)

// E7 on the command line: start --skills, skills list|search, stats --by skill,repo, status warnings.
func TestSkillsCLI(t *testing.T) {
	home := t.TempDir()
	t.Setenv("AGENT_JOBS_HOME", home)
	root := t.TempDir()
	for _, n := range []string{"golang", "reviewing"} {
		dir := filepath.Join(root, n)
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		body := "---\ndescription: The " + n + " skill.\n---\nzebra " + n + "\n"
		if err := os.WriteFile(filepath.Join(dir, "SKILL.md"), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	day := time.Now().Format("2006-01-02")
	idx, _ := json.Marshal(map[string]any{"skills": []map[string]any{
		{"name": "golang", "path": "golang", "status": "core", "kinds": []string{"code"}, "last_verified": day},
		{"name": "reviewing", "path": "reviewing", "status": "active", "kinds": []string{"review"}, "last_verified": day},
	}})
	if err := os.WriteFile(filepath.Join(root, "index.json"), idx, 0o644); err != nil {
		t.Fatal(err)
	}
	state := filepath.Join(home, "test")
	if err := os.MkdirAll(state, 0o700); err != nil {
		t.Fatal(err)
	}
	cfg, _ := json.Marshal(map[string]any{"skill_roots": []string{root, filepath.Join(root, "missing")}})
	if err := os.WriteFile(filepath.Join(state, "config.json"), cfg, 0o600); err != nil {
		t.Fatal(err)
	}
	id := strings.TrimSpace(captureStdout(t, func() error {
		return cmdStart([]string{"--agent", "test", "--title", "t", "--kind", "fix", "--brief", "b", "--model", "sonnet",
			"--skills", "+reviewing"})
	}))
	m, err := jobs.Open("test")
	if err != nil {
		t.Fatal(err)
	}
	defer m.Close()
	j, err := m.Store.Get(id)
	if err != nil || strings.Join(j.Skills, ",") != "golang,reviewing" {
		t.Fatalf("job %q: %+v %v", id, j, err)
	}
	if err := cmdStart([]string{"--agent", "test", "--title", "t", "--kind", "fix", "--brief", "b", "--model", "sonnet",
		"--skills", "golnag"}); err == nil || !strings.Contains(err.Error(), "close: golang") {
		t.Fatalf("unknown skill: %v", err)
	}
	out := captureStdout(t, func() error { return cmdSkills([]string{"search", "--agent", "test", "zebra", "reviewing"}) })
	lines := strings.Split(strings.TrimSpace(out), "\n")
	if len(lines) != 3 || !strings.HasPrefix(lines[0], "SCORE") || !strings.Contains(lines[1], "reviewing") {
		t.Fatalf("search:\n%s", out)
	}
	out = captureStdout(t, func() error { return cmdSkills([]string{"list", "--agent", "test", "--kind", "review"}) })
	if !strings.Contains(out, "reviewing") || strings.Contains(out, "golang") {
		t.Fatalf("list:\n%s", out)
	}
	if err := cmdSkills([]string{"search", "--agent", "test"}); err == nil {
		t.Fatal("search without a query")
	}
	out = captureStdout(t, func() error { return cmdStats([]string{"--agent", "test", "--by", "skill,repo"}) })
	if !strings.HasPrefix(out, "SKILL") || !strings.Contains(out, "golang") || !strings.Contains(out, "BY REPO") {
		t.Fatalf("stats --by skill,repo:\n%s", out)
	}
	out = captureStdout(t, func() error { return cmdStatus([]string{"--agent", "test"}) })
	if !strings.Contains(out, "skills warning:") {
		t.Fatalf("status lacks the root warning:\n%s", out)
	}
}
