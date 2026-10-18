package guard

import "strings"

// JobMCPToolPrefix is the name prefix of the job-role MCP tools (server "agent-jobs", see
// jobs.JobMCPServerName): the only MCP tools a job may call.
const JobMCPToolPrefix = "mcp__agent-jobs__"

// allowedTools are the tools a job may call (an allowlist: the reference forbids tools that reach a
// person or outlive the run, and a new Claude Code tool must not pass just because it is unknown).
// Bash, the file and read tools and the subagent tool keep their own checks below; WebFetch and
// WebSearch are closed by the deny rules for every kind except research; Skill loads the delivered
// skills; ToolSearch loads deferred tool schemas (the MCP tools above are deferred). TaskCreate,
// TaskUpdate, TaskList and TaskGet are the newer in-session todo list (TodoWrite's successor);
// TaskOutput, BashOutput, TaskStop and KillShell read or stop the job's own background shells, which
// live inside the job's scope and end with it.
var allowedTools = map[string]bool{
	"Bash": true, "Read": true, "Write": true, "Edit": true, "MultiEdit": true, "NotebookEdit": true,
	"Glob": true, "Grep": true, "LS": true, "TodoWrite": true, "WebFetch": true, "WebSearch": true,
	"Skill": true, "ToolSearch": true, "Agent": true, "Task": true,
	"TaskCreate": true, "TaskUpdate": true, "TaskList": true, "TaskGet": true,
	"TaskOutput": true, "BashOutput": true, "TaskStop": true, "KillShell": true,
}

// ToolAllowed reports whether a job may call the tool at all.
func ToolAllowed(name string) bool {
	if allowedTools[name] {
		return true
	}
	rest, ok := strings.CutPrefix(name, JobMCPToolPrefix)
	return ok && rest != ""
}

// toolRefused is the reason for a tool outside the allowlist.
func toolRefused(name string) string {
	return "blocked by guard: tool `" + name + "` is not allowed in a job. A job uses only Bash, the file " +
		"tools, the subagent tool, Skill, ToolSearch and the agent-jobs job tools; tools that reach a person, " +
		"another session or outlive the run (schedules, wakeups, messages, notifications, monitors, " +
		"worktrees, questions) are refused. Do the work with the allowed tools or say in the report what is missing."
}
