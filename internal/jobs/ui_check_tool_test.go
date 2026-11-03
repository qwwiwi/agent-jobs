package jobs

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"

	agentjobs "github.com/qwwiwi/agent-jobs"
)

// The kind checklists tell a job that touches UI to run <job dir>/tools/ui-check.mjs: prepare puts the
// shipped copy there, whatever repository the job works in.
func TestPrepareWritesUICheckTool(t *testing.T) {
	m := newManager(t)
	j := enqueue(t, m, "do the thing", nil)
	j = runToEnd(t, m, j.ID)
	got, err := os.ReadFile(filepath.Join(m.JobDir(j.ID), UICheckToolRel))
	if err != nil {
		t.Fatal(err)
	}
	want, err := agentjobs.Templates.ReadFile("scripts/ui-check.mjs")
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, want) {
		t.Fatal("tools/ui-check.mjs differs from the shipped scripts/ui-check.mjs")
	}
}
