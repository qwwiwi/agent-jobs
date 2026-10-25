package main

import (
	"bytes"
	"strings"
	"testing"

	"github.com/qwwiwi/agent-jobs/internal/jobs"
)

// The merge hook refuses a Bash command of the orchestrator session that runs `agent-jobs gate retire`
// (the owner runs it in a terminal outside the session), in any spelling the reader can read, gated
// repository or not; a command that only mentions it passes.
func TestHookMergeGateRefusesRetire(t *testing.T) {
	t.Setenv("AGENT_JOBS_HOME", t.TempDir())
	dir := t.TempDir()
	run := func(command string) (int, string) {
		var out, errb bytes.Buffer
		code := runHook([]string{"merge-gate", "--agent", "test"}, strings.NewReader(bashEvent(dir, command)), &out, &errb, open)
		return code, errb.String()
	}
	for _, cmd := range []string{
		"agent-jobs gate retire --agent test /srv/repo",
		"/home/openclaw/.local/bin/agent-jobs gate retire --agent test /srv/repo",
		"cd /tmp && agent-jobs gate retire /srv/repo --agent test",
		"sh -c 'agent-jobs gate retire --agent test /srv/repo'",
		"env X=1 agent-jobs gate retire --agent test /srv/repo",
		"go run ./cmd/agent-jobs gate retire --agent test /srv/repo",
		`"$AJ" gate retire --agent test /srv/repo`,
	} {
		if code, errs := run(cmd); code != 2 || !strings.HasPrefix(errs, jobs.MergeGatePrefix) || !strings.Contains(errs, "gate retire") {
			t.Errorf("%q: exit %d, stderr %q", cmd, code, errs)
		}
	}
	for _, cmd := range []string{
		"echo agent-jobs gate retire",
		"agent-jobs gate release-check --agent test --repo /srv/repo --sha x",
		"grep -n 'gate retire' README.md",
	} {
		if code, errs := run(cmd); code != 0 || errs != "" {
			t.Errorf("%q: exit %d, stderr %q", cmd, code, errs)
		}
	}
}
