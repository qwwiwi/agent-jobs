package guard

import (
	"encoding/json"
	"strings"
	"testing"
)

// Real refusals of the guard map back to their rule names; a refusal the classifier does not know
// would show up as "other" in job stats.
func TestRuleNameOfRealRefusals(t *testing.T) {
	cfg := Config{OwnerNames: []string{"Alex"}, GitName: "Job Bot", GitEmail: "bot@example.com"}
	cases := map[string]string{
		"git push origin main":                                      RuleOutward,
		"gh pr create --fill":                                       RuleOutward,
		"go test ./... | tail -5; echo $?":                          RulePipeStatus,
		"while pgrep -f 'npm run dev' >/dev/null; do sleep 1; done": RulePgrepLoop,
		`git commit -m "fix by alex"`:                               RuleOwnerName,
		`git commit --author="X <x@y>" -m x`:                        RuleGitIdentity,
		"claude -p hello":                                           RuleNestedAgent,
		"agent-jobs start --title t":                                RuleNestedJob,
		"npx some-tool":                                             RulePackageRunner,
		"git -c core.pager=less log":                                RuleGitConfig,
		"sh -c 'git status'":                                        RuleUnverifiable,
		"unset GIT_CONFIG_GLOBAL":                                   RuleGitEnv,
		"unset AGENT_JOBS_JOB":                                      RuleAgentJobsEnv,
		"git rebase -x 'make' HEAD~3":                               RuleGitSubcommand,
		"echo x > .git/hooks/pre-commit":                            RuleGitDirWrite,
		"shopt -s dotglob":                                          RuleGlobOptions,
		"cp a.txt dir/*":                                            RuleGlobWrite,
	}
	for cmd, want := range cases {
		reason := Check(cmd, cfg)
		if reason == "" {
			t.Fatalf("%q was not refused", cmd)
		}
		if got := RuleName(reason); got != want {
			t.Fatalf("%q: rule %q, want %q (reason: %s)", cmd, got, want, reason)
		}
	}
	in, _ := json.Marshal(map[string]any{"description": "d", "prompt": "p", "run_in_background": true})
	if got := RuleName(CheckSubagent(in, Config{SubagentLog: "/nonexistent/log"})); got != RuleSubagentEscape {
		t.Fatalf("subagent escape: %q", got)
	}
	if got := RuleName(capReached()); got != RuleSubagentLimit {
		t.Fatalf("subagent cap: %q", got)
	}
	if got := RuleName(CheckFileWrite("Write", "../x", Config{Dir: t.TempDir()})); got != RulePathDotDot {
		t.Fatalf("dotdot: %q", got)
	}
	if got := RuleName("something else"); got != RuleOther {
		t.Fatalf("unknown: %q", got)
	}
}

func TestRefusalIn(t *testing.T) {
	hook := "PreToolUse:Bash hook error: ['/bin/agent-jobs' guard]: blocked by guard: `git push` reaches outside the job."
	if got := RefusalIn(hook); !strings.HasPrefix(got, "blocked by guard: `git push`") {
		t.Fatalf("%q", got)
	}
	if RefusalIn("File does not exist.") != "" {
		t.Fatal("a plain tool error is not a refusal")
	}
	if got := RefusalIn("agent-jobs guard: at most 3 subagents per job"); got == "" {
		t.Fatal("a bare guard message is a refusal")
	}
	// Guard text in ordinary output (a failing test printing guard messages) is not a refusal.
	if RefusalIn("--- FAIL: TestCheck\n    got \"blocked by guard: x\"") != "" {
		t.Fatal("guard text inside test output counted as a refusal")
	}
}
