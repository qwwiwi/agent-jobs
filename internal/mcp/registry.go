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
	// What the orchestrator did with a job's result (feeds job_stats): the session's judgement, not a job's.
	"job_outcome",
	// The skill catalog (E7): the session chooses a job's skills; a job gets them delivered, it does
	// not browse the catalog.
	"skill_list",
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
	// The agent's own goals: a job does not set or read them.
	"intention_add",
	"intention_list",
	"intention_set",
	// Schedules: only the session adds, edits and removes them (removal only on the owner's instruction).
	"schedule_add",
	"schedule_list",
	"schedule_set",
	"schedule_remove",
	// Checks (dead-man's switch): only the session adds, edits, lists and removes them (removal only
	// on the owner's instruction); marking is open to jobs.
	"check_add",
	"check_list",
	"check_set",
	"check_remove",
	// «Owner waits»: only the session that writes to the owner clears the pending record.
	"owner_replied",
	// The heartbeat's stored decisions (their reason is untrusted model text): the session reads them.
	"heartbeat_get",
	// The brief checklist (Wave F, F2): only the session writes the owner's brief, rows, approval and
	// waivers into the project repo and the store; a job reports against the checklist, never edits it.
	"checklist_init",
	"checklist_get",
	"checklist_set",
	// The gate jobs (Wave F, F3 / F5 / F8): only the session starts the slicing check, the blind judge and
	// the briefing and reads their stored results; their input is assembled by the daemon.
	"slicing_start",
	"slicing_get",
	"briefing_start",
	"briefing_get",
	"judge_start",
	"judge_get",
	// The release check (Wave F, F7): only the session asks whether a commit may ship.
	"release_check",
}

// JobAllowed are tools a job may call (the orchestrator gets them too): reading and writing the
// team's shared memory, notify_owner, which reaches the owner only through the agent's own
// session (rate limited), and check_mark, a completion mark for an existing check; nothing that
// reaches another agent.
var JobAllowed = []string{
	"memory_search",
	"memory_write",
	"notify_owner",
	"check_mark",
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
