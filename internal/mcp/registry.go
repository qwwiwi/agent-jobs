// Package mcp is the agent-jobs MCP server (stdio). One binary serves two audiences:
//
//   - the orchestrator role: the agent's live session (job_start, job_status, …);
//   - the job role: a running job, which gets only the tools allow-listed for jobs.
//
// The job list is an allow-list, not a deny-list (reference, section "MCP tools"): a new tool is closed to
// jobs until it is opened on purpose, and a test fails when a tool is in neither list.
package mcp

import (
	"fmt"
	"slices"
)

// Role is the audience a server instance serves.
type Role string

// Roles.
const (
	RoleOrchestrator Role = "orchestrator"
	RoleJob          Role = "job"
)

// ParseRole validates a --role value.
func ParseRole(s string) (Role, error) {
	switch Role(s) {
	case RoleOrchestrator, RoleJob:
		return Role(s), nil
	}
	return "", fmt.Errorf("role %q must be orchestrator or job", s)
}

// OrchestratorOnly are tools only the agent's session may call. A job never sees them and the
// job-role server refuses them even if one were registered there.
var OrchestratorOnly = []string{
	"job_start",
	"job_status",
	"job_result",
	"job_list",
	"job_cancel",
	"job_stop",
	"job_resume",
	"job_amend",
	"job_stats",
	"memory_already_exists",
	"task_create",
	"task_list",
	"task_update",
	"task_get",
	"send_message",
	"inbox",
	// System and files (reference, «MCP-тулы»: not available to a job).
	"system_doctor",
	"system_logs",
	"activity_report",
	"trash_move",
	"trash_restore",
}

// JobAllowed are tools a job may call (the orchestrator gets them too): reading and writing the
// team's shared memory, nothing that reaches a person or another agent.
var JobAllowed = []string{
	"memory_search",
	"memory_write",
}

// Allowed reports whether role may see and call the tool.
func Allowed(role Role, name string) bool {
	switch role {
	case RoleOrchestrator:
		return slices.Contains(OrchestratorOnly, name) || slices.Contains(JobAllowed, name)
	case RoleJob:
		return slices.Contains(JobAllowed, name)
	}
	return false
}

// audienceOf returns the single list a tool belongs to, or an error when it is in none or both.
func audienceOf(name string) (string, error) {
	o, j := slices.Contains(OrchestratorOnly, name), slices.Contains(JobAllowed, name)
	switch {
	case o && j:
		return "", fmt.Errorf("tool %q is in both OrchestratorOnly and JobAllowed", name)
	case o:
		return "orchestrator", nil
	case j:
		return "job", nil
	}
	return "", fmt.Errorf("tool %q is in neither OrchestratorOnly nor JobAllowed", name)
}
