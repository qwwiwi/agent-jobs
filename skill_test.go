package agentjobs

import (
	"strings"
	"testing"
)

// The agent-jobs skill teaches the MCP tools, the mandatory model-or-complexity, the routing rule,
// the subagent cap and how to read a result.
func TestSkillContent(t *testing.T) {
	b, err := Templates.ReadFile("skills/agent-jobs/SKILL.md")
	if err != nil {
		t.Fatal(err)
	}
	s := string(b)
	if !strings.HasPrefix(s, "---\nname: agent-jobs\ndescription: ") {
		t.Fatal("SKILL.md must start with front matter: name agent-jobs and a description")
	}
	for _, want := range []string{
		"job_start", "job_status", "job_result", "job_list", "job_cancel",
		"`model` **or** `complexity`", "mandatory", "no default and no classifier",
		"do it yourself", "a subagent", "a job", "survive a restart", "overnight",
		"at most three per job", "No nested jobs",
		"«Not done / uncertain» first", "Check the diff", "leave the machine",
		"80 / $10 / 45 min", "200 / $50 / 180",
		"agent-jobs profile", "agent-jobs card new",
		"job_stop", "job_resume", "job_amend", "same claude session", "Orchestrator update",
		"memory_search", "memory_write", "task_update", "send_message", "inbox", "cannot write a `decision`",
	} {
		if !strings.Contains(s, want) {
			t.Errorf("SKILL.md lacks %q", want)
		}
	}
}
