package guard

// JobMCPToolPrefix is the name prefix of the job-role MCP tools (server "agent-jobs", see
// jobs.JobMCPServerName).
const JobMCPToolPrefix = "mcp__agent-jobs__"

// JobMCPTools are the job-role tools of the agent-jobs MCP server (mcp.JobAllowed; a test in
// internal/mcp keeps the two equal): the only MCP tools a job may call. The guard matches the fully
// qualified names exactly: a prefix match would also admit another server whose name starts with
// "agent-jobs__" (mcp__agent-jobs__other__send is tool "send" of server "agent-jobs__other").
var JobMCPTools = []string{"memory_search", "memory_write", "notify_owner", "check_mark"}

// allowedTools are the tools a job may call (an allowlist: the reference forbids tools that reach a
// person or outlive the run, and a new Claude Code tool must not pass just because it is unknown).
// Bash, the file and read tools and the subagent tool keep their own checks below; WebFetch and
// WebSearch are closed by the deny rules for every kind except research; Skill loads the delivered
// skills; ToolSearch loads deferred tool schemas (the MCP tools above are deferred). TaskCreate,
// TaskUpdate, TaskList and TaskGet are the newer in-session todo list (TodoWrite's successor);
// TaskOutput, BashOutput, TaskStop and KillShell read or stop the job's own background shells, which
// live inside the job's scope and end with it.
var allowedTools = func() map[string]bool {
	m := map[string]bool{
		"Bash": true, "Read": true, "Write": true, "Edit": true, "MultiEdit": true, "NotebookEdit": true,
		"Glob": true, "Grep": true, "LS": true, "TodoWrite": true, "WebFetch": true, "WebSearch": true,
		"Skill": true, "ToolSearch": true, "Agent": true, "Task": true,
		"TaskCreate": true, "TaskUpdate": true, "TaskList": true, "TaskGet": true,
		"TaskOutput": true, "BashOutput": true, "TaskStop": true, "KillShell": true,
	}
	for _, t := range JobMCPTools {
		m[JobMCPToolPrefix+t] = true
	}
	return m
}()

// ToolAllowed reports whether a job may call the tool at all.
func ToolAllowed(name string) bool { return allowedTools[name] }

// waitHint tells a job that reached for a waiting tool how to wait instead.
const waitHint = " To wait for a build or a file, run the command in the foreground (with a timeout), or poll " +
	"in a bounded Bash loop, e.g. `for i in $(seq 60); do test -e out && break; sleep 10; done`; " +
	"do not retry this tool."

// toolRefused is the reason for a tool outside the allowlist.
func toolRefused(name string) string {
	head := "blocked by guard: tool `" + name + "` is not allowed in a job."
	switch name {
	case "Monitor":
		// Monitor runs its own command (or opens a WebSocket), which the Bash guard never checks.
		return head + " Monitor runs its own command outside the Bash guard's checks." + waitHint
	case "ScheduleWakeup":
		// A job is one run: the daemon owns its lifetime (timeout, cancel, resume).
		return head + " A job does not reschedule itself: the daemon controls the job's lifetime, and " +
			"the job ends when this run ends." + waitHint
	}
	return head + " A job uses only Bash, the file tools, the subagent tool, Skill, ToolSearch and the " +
		"agent-jobs job tools; tools that reach a person or another session, schedule or wake the job, " +
		"or run commands outside the guard (schedules, wakeups, messages, notifications, monitors, " +
		"worktrees, questions) are refused. Do the work with the allowed tools or say in the report what is missing."
}
