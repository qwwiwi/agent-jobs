package guard

import (
	"encoding/json"
	"strings"
	"testing"
)

func decideTool(t *testing.T, tool string, input map[string]any) string {
	t.Helper()
	b, err := json.Marshal(map[string]any{"tool_name": tool, "cwd": t.TempDir(), "tool_input": input})
	if err != nil {
		t.Fatal(err)
	}
	r, err := Decide(strings.NewReader(string(b)), Config{})
	if err != nil {
		t.Fatalf("Decide(%s): %v", tool, err)
	}
	return r
}

// U1: a job may call only the tools it is explicitly allowed; everything else is refused.
func TestToolAllowlistRefusesOthers(t *testing.T) {
	refused := []string{
		"ScheduleWakeup", "CronCreate", "CronDelete", "CronList", "SendMessage", "RemoteTrigger",
		"PushNotification", "Monitor", "Artifact", "ArtifactData", "ArtifactComments", "ArtifactCheck",
		"EnterWorktree", "ExitWorktree", "AskUserQuestion", "ExitPlanMode", "ListAgents", "Workflow",
		"DesignSync", "ReportFindings",
		// an unknown future tool and MCP tools of other servers
		"SomeFutureTool", "mcp__other__send", "mcp__agent-jobs", "mcp__agent-jobs-x__job_start", "",
	}
	for _, tool := range refused {
		r := decideTool(t, tool, map[string]any{})
		if r == "" {
			t.Errorf("%q passed, want refused", tool)
			continue
		}
		if !strings.Contains(r, "not allowed in a job") {
			t.Errorf("%q: reason %q does not say why", tool, r)
		}
	}
}

func TestToolAllowlistPassesAllowed(t *testing.T) {
	dir := t.TempDir()
	cases := []struct {
		tool  string
		input map[string]any
	}{
		{"Bash", map[string]any{"command": "ls"}},
		{"Read", map[string]any{"file_path": dir + "/a"}},
		{"Write", map[string]any{"file_path": dir + "/a"}},
		{"Edit", map[string]any{"file_path": dir + "/a"}},
		{"MultiEdit", map[string]any{"file_path": dir + "/a"}},
		{"NotebookEdit", map[string]any{"notebook_path": dir + "/a.ipynb"}},
		{"Glob", map[string]any{"pattern": "*.go", "path": dir}},
		{"Grep", map[string]any{"pattern": "x", "path": dir}},
		{"LS", map[string]any{"path": dir}},
		{"TodoWrite", map[string]any{}},
		{"WebFetch", map[string]any{"url": "https://example.com"}},
		{"WebSearch", map[string]any{"query": "x"}},
		{"Skill", map[string]any{"skill": "git-workflows"}},
		{"ToolSearch", map[string]any{"query": "select:NotebookEdit"}},
		{"TaskCreate", map[string]any{}},
		{"TaskUpdate", map[string]any{}},
		{"TaskList", map[string]any{}},
		{"TaskGet", map[string]any{}},
		{"TaskOutput", map[string]any{}},
		{"BashOutput", map[string]any{}},
		{"TaskStop", map[string]any{}},
		{"KillShell", map[string]any{}},
		{"mcp__agent-jobs__memory_search", map[string]any{"query": "x"}},
		{"mcp__agent-jobs__memory_write", map[string]any{}},
		{"mcp__agent-jobs__notify_owner", map[string]any{}},
		{"mcp__agent-jobs__check_mark", map[string]any{}},
	}
	for _, c := range cases {
		if r := decideTool(t, c.tool, c.input); r != "" {
			t.Errorf("%s refused: %q", c.tool, r)
		}
	}
	// The subagent tool is allowed but stays under its own rules (no log configured = refused by the cap).
	for _, tool := range []string{"Agent", "Task"} {
		r := decideTool(t, tool, map[string]any{"description": "d", "prompt": "p"})
		if strings.Contains(r, "not allowed in a job") {
			t.Errorf("%s refused by the allowlist, want the subagent rules: %q", tool, r)
		}
	}
}

// LS lists a directory: it is held to the state-read rule like Read, Grep and Glob.
func TestLSStateReadChecked(t *testing.T) {
	state := t.TempDir()
	own := state + "/jobs/j1"
	cfg := Config{StateDir: state, OwnDir: own, Dir: own + "/work"}
	b, _ := json.Marshal(map[string]any{"tool_name": "LS", "cwd": cfg.Dir, "tool_input": map[string]string{"path": state + "/jobs"}})
	r, err := Decide(strings.NewReader(string(b)), cfg)
	if err != nil || r == "" {
		t.Fatalf("LS of the state dir: %q %v, want refused", r, err)
	}
}
