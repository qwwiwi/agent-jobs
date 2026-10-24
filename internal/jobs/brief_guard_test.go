package jobs

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

// A repository job never writes its worktree's brief/BRIEF.md or brief/CHECKLIST.md (the orchestrator's
// checklist tools are their only writers): the job's guard hook refuses Write, Edit and Bash writes to
// them, while reads and other files under brief/ stay allowed.
func TestJobGuardRefusesBriefFiles(t *testing.T) {
	m := newManager(t)
	repo := newRepo(t)
	j := runToEnd(t, m, enqueue(t, m, "x", func(r *StartRequest) { r.RepoPath = repo }).ID)
	work := filepath.Join(m.JobDir(j.ID), "work")
	if err := os.MkdirAll(filepath.Join(work, "brief"), 0o755); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(filepath.Join(m.JobDir(j.ID), "settings.json"))
	if err != nil {
		t.Fatal(err)
	}
	var s struct {
		Permissions struct {
			Deny []string `json:"deny"`
		} `json:"permissions"`
		Hooks struct {
			PreToolUse []struct {
				Hooks []struct{ Command string } `json:"hooks"`
			} `json:"PreToolUse"`
		} `json:"hooks"`
	}
	if err := json.Unmarshal(b, &s); err != nil || len(s.Hooks.PreToolUse) == 0 || len(s.Hooks.PreToolUse[0].Hooks) == 0 {
		t.Fatalf("settings.json: %v %s", err, b)
	}
	cmd := s.Hooks.PreToolUse[0].Hooks[0].Command
	run := func(tool string, input map[string]string) int {
		t.Helper()
		in, _ := json.Marshal(map[string]any{"tool_name": tool, "cwd": work, "tool_input": input})
		c := exec.Command("sh", "-c", cmd)
		c.Stdin = bytes.NewReader(in)
		var ee *exec.ExitError
		if err := c.Run(); errors.As(err, &ee) {
			return ee.ExitCode()
		} else if err != nil {
			t.Fatal(err)
		}
		return 0
	}
	for _, rel := range []string{"brief/BRIEF.md", "brief/CHECKLIST.md"} {
		abs := filepath.Join(work, rel)
		for _, c := range []struct {
			tool  string
			input map[string]string
		}{
			{"Write", map[string]string{"file_path": abs}},
			{"Write", map[string]string{"file_path": rel}},
			{"Edit", map[string]string{"file_path": abs}},
			{"Bash", map[string]string{"command": "echo x > " + rel}},
			{"Bash", map[string]string{"command": "sed -i s/a/b/ " + abs}},
			{"Bash", map[string]string{"command": "cp /dev/null " + rel}},
		} {
			if code := run(c.tool, c.input); code != 2 {
				t.Errorf("%s %v: exit %d, want 2", c.tool, c.input, code)
			}
		}
		if !containsString(s.Permissions.Deny, "Write(//"+filepath.ToSlash(abs)[1:]+")") {
			t.Errorf("no Write deny rule for %s: %v", abs, s.Permissions.Deny)
		}
	}
	for _, c := range []struct {
		tool  string
		input map[string]string
	}{
		{"Bash", map[string]string{"command": "cat brief/BRIEF.md"}},
		{"Write", map[string]string{"file_path": filepath.Join(work, "brief", "notes.md")}},
		{"Write", map[string]string{"file_path": filepath.Join(work, "main.go")}},
	} {
		if code := run(c.tool, c.input); code != 0 {
			t.Errorf("%s %v: exit %d, want 0", c.tool, c.input, code)
		}
	}
}

func containsString(list []string, s string) bool {
	for _, x := range list {
		if x == s {
			return true
		}
	}
	return false
}
