package guard

import "strings"

// Rule names of guard refusals, as job stats count them. The guard's messages are prose written for
// the model; RuleName maps one back to the short, stable name of the rule that produced it.
const (
	RulePathDotDot       = "path-dotdot"
	RuleGitDirWrite      = "git-dir-write"
	RuleAgentJobsState   = "agent-jobs-state"
	RuleUnverifiable     = "unverifiable-form"
	RulePackageRunner    = "package-runner"
	RuleNestedAgent      = "nested-agent"
	RuleNestedJob        = "nested-job"
	RuleAgentJobsEnv     = "agent-jobs-env"
	RuleGitEnv           = "git-env"
	RuleGlobWrite        = "glob-write"
	RuleGlobOptions      = "glob-options"
	RuleGitConfig        = "git-config"
	RuleOutward          = "outward"
	RuleGitSubcommand    = "git-subcommand"
	RulePipeStatus       = "pipe-status"
	RulePgrepLoop        = "pgrep-wait-loop"
	RuleOwnerName        = "owner-name"
	RuleGitIdentity      = "git-identity"
	RuleCommitText       = "commit-text"
	RuleSubagentLimit    = "subagent-limit"
	RuleSubagentEscape   = "subagent-isolation"
	RuleSubagentLog      = "subagent-log"
	RuleOther            = "other"
	refusalPrefixGuard   = "blocked by guard: "
	refusalPrefixSubagnt = "agent-jobs guard: "
)

// ruleMarkers map a phrase of a refusal message to its rule; the first match wins, so the more
// specific phrases come first.
var ruleMarkers = []struct{ phrase, rule string }{
	{"contains `..`", RulePathDotDot},
	{"a job does not write inside a .git directory", RuleGitDirWrite},
	{"is git's config, hooks or $GIT_DIR", RuleGitDirWrite},
	{"agent-jobs state", RuleAgentJobsState},
	{"guard cannot verify this form", RuleUnverifiable},
	{"fetches and runs a package by name", RulePackageRunner},
	{"starts a nested agent", RuleNestedAgent},
	{"controls jobs", RuleNestedJob},
	{"changes the agent-jobs environment", RuleAgentJobsEnv},
	{"changes the git environment", RuleGitEnv},
	{"is a glob/brace word", RuleGlobWrite},
	{"dotglob/globdots", RuleGlobOptions},
	{"allowlist of harmless git config keys", RuleGitConfig},
	{"reaches outside the job", RuleOutward},
	{"runs commands or git subcommands the guard does not see", RuleGitSubcommand},
	{"right after a pipe is the status of the last stage", RulePipeStatus},
	{"`pgrep -f` pattern matches the loop", RulePgrepLoop},
	{"the owner's name", RuleOwnerName},
	{"foreign git identity", RuleGitIdentity},
	{"subagents per job", RuleSubagentLimit},
	{"subagents are not counted for this call", RuleSubagentLimit},
	{"runs in the foreground inside the job", RuleSubagentEscape},
	{"subagent log", RuleSubagentLog},
	{"subagent counter", RuleSubagentLog},
}

// RefusalIn returns the guard refusal inside a tool result text, or "" when the text is not one. A
// refusal reaches the model as a PreToolUse hook error ("PreToolUse:Bash hook error: [cmd]: blocked
// by guard: …") or as the bare message; guard text anywhere else (a failing test printing guard
// messages) does not count.
func RefusalIn(text string) string {
	text = strings.TrimSpace(text)
	prefixes := []string{refusalPrefixGuard, refusalPrefixSubagnt}
	if strings.HasPrefix(text, "PreToolUse:") {
		line, _, _ := strings.Cut(text, "\n")
		if !strings.Contains(line, "hook error") {
			return ""
		}
		for _, p := range prefixes {
			if i := strings.Index(line, p); i >= 0 {
				return text[i:]
			}
		}
		return ""
	}
	for _, p := range prefixes {
		if strings.HasPrefix(text, p) {
			return text
		}
	}
	return ""
}

// RuleName returns the rule a guard refusal message comes from (RuleOther when none is known). A
// "blocked by guard" message with no known phrase is the commit-text check (its problem is free
// text).
func RuleName(reason string) string {
	low := strings.ToLower(reason)
	for _, m := range ruleMarkers {
		if strings.Contains(low, strings.ToLower(m.phrase)) {
			return m.rule
		}
	}
	if strings.Contains(reason, refusalPrefixGuard) {
		return RuleCommitText
	}
	return RuleOther
}
