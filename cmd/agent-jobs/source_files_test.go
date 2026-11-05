package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/qwwiwi/agent-jobs/internal/jobs"
)

func TestStartFileSources(t *testing.T) {
	t.Setenv("AGENT_JOBS_HOME", t.TempDir())
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	origin := filepath.Join(dir, "report.md")
	if err := os.WriteFile(origin, []byte("CLI evidence"), 0o600); err != nil {
		t.Fatal(err)
	}
	id := strings.TrimSpace(captureStdout(t, func() error {
		return cmdStart([]string{"--agent", "test", "--title", "review", "--kind", "review", "--brief", "read", "--model", "sonnet",
			"--source", origin, "--source", origin})
	}))
	m, err := jobs.Open("test")
	if err != nil {
		t.Fatal(err)
	}
	defer m.Close()
	for _, name := range []string{"report.md", "report-2.md"} {
		b, err := os.ReadFile(filepath.Join(m.JobDir(id), "sources", name))
		if err != nil || string(b) != "CLI evidence" {
			t.Fatalf("repeatable --source copy %s: %q, %v", name, b, err)
		}
	}
}
