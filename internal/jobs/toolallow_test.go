package jobs

import (
	"slices"
	"testing"

	"github.com/qwwiwi/agent-jobs/internal/guard"
)

// U1: the deny rules name the known tools that reach a person or outlive the run, the guard refuses
// them too, and the guard's MCP prefix is this server's name.
func TestDenyRulesNameOutwardTools(t *testing.T) {
	outward := []string{
		"ScheduleWakeup", "CronCreate", "CronDelete", "CronList", "SendMessage", "RemoteTrigger",
		"PushNotification", "Monitor", "ListAgents", "Workflow", "EnterWorktree", "ExitWorktree",
		"AskUserQuestion", "ExitPlanMode", "Artifact", "ArtifactData", "ArtifactComments", "ArtifactCheck",
	}
	m := &Manager{SelfBin: "/bin/agent-jobs"}
	deny, _ := m.Settings("fix")["permissions"].(map[string]any)["deny"].([]string)
	for _, tool := range outward {
		if !slices.Contains(deny, tool) {
			t.Errorf("deny rules miss %s", tool)
		}
		if guard.ToolAllowed(tool) {
			t.Errorf("guard allows %s", tool)
		}
	}
	if guard.JobMCPToolPrefix != "mcp__"+JobMCPServerName+"__" {
		t.Errorf("guard MCP prefix %q does not match server %q", guard.JobMCPToolPrefix, JobMCPServerName)
	}
}
