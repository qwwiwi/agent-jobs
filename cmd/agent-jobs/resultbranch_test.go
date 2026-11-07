package main

import (
	"strings"
	"testing"

	"github.com/qwwiwi/agent-jobs/internal/jobs"
)

func TestStartResultBranchFlag(t *testing.T) {
	t.Setenv("AGENT_JOBS_HOME", t.TempDir())
	repo := t.TempDir()
	gitOut(t, repo, "init", "-q", "-b", "main")
	gitOut(t, repo, "-c", "user.name=Test", "-c", "user.email=test@example.com", "commit", "-q", "--allow-empty", "-m", "base")
	id := strings.TrimSpace(captureStdout(t, func() error {
		return cmdStart([]string{"--agent", "test", "--title", "result branch", "--kind", "fix", "--brief", "test", "--model", "sonnet",
			"--repo", repo, "--branch", "feature"})
	}))
	m, err := jobs.Open("test")
	if err != nil {
		t.Fatal(err)
	}
	defer m.Close()
	j, err := m.Store.Get(id)
	if err != nil || j.RequestedBranch != "feature" || j.Branch != "" {
		t.Fatalf("--branch not recorded: %+v %v", j, err)
	}
}
