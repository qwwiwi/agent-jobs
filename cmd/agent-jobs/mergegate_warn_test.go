package main

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"

	"github.com/qwwiwi/agent-jobs/internal/jobs"
)

// A merge the hook cannot read runs (exit 0) and its warning is PreToolUse hook JSON on stdout: the
// additionalContext reaches the model, systemMessage the user; no permissionDecision (the hook never
// grants a permission) and nothing on stderr.
func TestHookMergeGateWarningIsHookJSON(t *testing.T) {
	t.Setenv("AGENT_JOBS_HOME", t.TempDir())
	t.Setenv("GH_REPO", "")
	repo, _ := gatedRepo(t)
	var out, errb bytes.Buffer
	code := runHook([]string{"merge-gate", "--agent", "test"}, strings.NewReader(bashEvent(repo, "sh -c 'gh pr merge 5'")), &out, &errb, open)
	if code != 0 || errb.Len() != 0 {
		t.Fatalf("exit %d, stderr %q", code, errb.String())
	}
	if strings.Count(out.String(), "\n") != 1 {
		t.Fatalf("stdout is not one JSON line: %q", out.String())
	}
	var got map[string]json.RawMessage
	if err := json.Unmarshal(out.Bytes(), &got); err != nil {
		t.Fatalf("stdout %q: %v", out.String(), err)
	}
	var spec map[string]string
	if err := json.Unmarshal(got["hookSpecificOutput"], &spec); err != nil {
		t.Fatalf("hookSpecificOutput %s: %v", got["hookSpecificOutput"], err)
	}
	var msg string
	if err := json.Unmarshal(got["systemMessage"], &msg); err != nil {
		t.Fatalf("systemMessage %s: %v", got["systemMessage"], err)
	}
	if len(got) != 2 || len(spec) != 2 || spec["hookEventName"] != "PreToolUse" ||
		!strings.HasPrefix(spec["additionalContext"], jobs.MergeGateWarnPrefix+"cannot tell whether") || msg != spec["additionalContext"] {
		t.Fatalf("hook JSON %s", out.String())
	}
}
