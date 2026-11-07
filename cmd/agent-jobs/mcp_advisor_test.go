package main

import (
	"testing"

	"github.com/qwwiwi/agent-jobs/internal/jobs"
)

// A crew job's server command line (`mcp --role job --agent A --advisor`) still runs inside the job;
// the orchestrator role stays refused there, with or without the switch.
func TestMCPAdvisorFlagInsideJob(t *testing.T) {
	t.Setenv(jobs.JobEnvVar, "261106-000000-crew")
	if reason := refuseInsideJob("mcp", []string{"--role", "job", "--agent", "test", jobs.JobMCPAdvisorFlag}); reason != "" {
		t.Fatalf("crew job server refused inside its job: %s", reason)
	}
	if got := mcpRole([]string{"--role", "job", "--agent", "test", jobs.JobMCPAdvisorFlag}); got != "job" {
		t.Fatalf("role with the advisor switch: %q", got)
	}
	if reason := refuseInsideJob("mcp", []string{"--role", "orchestrator", "--agent", "test", jobs.JobMCPAdvisorFlag}); reason == "" {
		t.Fatal("orchestrator role allowed inside a job")
	}
	if err := cmdMCP([]string{"--role", "orchestrator", "--agent", "test", jobs.JobMCPAdvisorFlag}); err == nil {
		t.Fatal("--advisor accepted for the orchestrator role")
	}
}
