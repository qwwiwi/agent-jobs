package main

import (
	"bytes"
	"strings"
	"testing"

	"github.com/qwwiwi/agent-jobs/internal/jobs"
)

// The merge hook refuses agent-jobs gate retire through go run with build flags before the package or a
// @version on the package path.
func TestHookMergeGateRefusesRetireGoRunForms(t *testing.T) {
	t.Setenv("AGENT_JOBS_HOME", t.TempDir())
	dir := t.TempDir()
	for _, cmd := range []string{
		"go run -tags x ./cmd/agent-jobs gate retire --agent test /srv/repo",
		"go run github.com/qwwiwi/agent-jobs/cmd/agent-jobs@latest gate retire --agent test /srv/repo",
	} {
		var out, errb bytes.Buffer
		code := runHook([]string{"merge-gate", "--agent", "test"}, strings.NewReader(bashEvent(dir, cmd)), &out, &errb, open)
		if errs := errb.String(); code != 2 || !strings.HasPrefix(errs, jobs.MergeGatePrefix) || !strings.Contains(errs, "gate retire") {
			t.Errorf("%q: exit %d, stderr %q", cmd, code, errs)
		}
	}
}
