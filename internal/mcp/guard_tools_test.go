package mcp

import (
	"slices"
	"testing"

	"github.com/qwwiwi/agent-jobs/internal/guard"
	"github.com/qwwiwi/agent-jobs/internal/jobs"
)

// The guard keeps its own copy of the job-role tool names (guard cannot import this package, which
// imports guard): the two lists must stay equal, and the guard admits exactly these fully qualified
// names of this server and no orchestrator-only tool.
func TestGuardJobToolsMatchRegistry(t *testing.T) {
	got, want := slices.Sorted(slices.Values(guard.JobMCPTools)), slices.Sorted(slices.Values(JobAllowed))
	if !slices.Equal(got, want) {
		t.Fatalf("guard.JobMCPTools %v, registry JobAllowed %v", got, want)
	}
	prefix := "mcp__" + jobs.JobMCPServerName + "__"
	if guard.JobMCPToolPrefix != prefix {
		t.Fatalf("guard prefix %q, server prefix %q", guard.JobMCPToolPrefix, prefix)
	}
	for _, n := range JobAllowed {
		if !guard.ToolAllowed(prefix + n) {
			t.Errorf("guard refuses job tool %s", n)
		}
	}
	for _, n := range OrchestratorOnly {
		if guard.ToolAllowed(prefix + n) {
			t.Errorf("guard allows orchestrator-only tool %s", n)
		}
	}
}
