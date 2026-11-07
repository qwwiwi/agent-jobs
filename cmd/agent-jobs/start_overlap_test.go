package main

import (
	"os/exec"
	"slices"
	"strings"
	"testing"

	"github.com/qwwiwi/agent-jobs/internal/jobs"
)

func TestStartCLIOverlapParameters(t *testing.T) {
	t.Setenv("AGENT_JOBS_HOME", t.TempDir())
	repo := t.TempDir()
	for _, args := range [][]string{{"init", "-q", "-b", "main"}, {"-c", "user.name=T", "-c", "user.email=t@example.com", "commit", "-q", "--allow-empty", "-m", "init"}} {
		if out, err := exec.Command("git", append([]string{"-C", repo}, args...)...).CombinedOutput(); err != nil {
			t.Fatalf("git: %v %s", err, out)
		}
	}
	args := []string{"--agent", "test", "--title", "scope", "--kind", "fix", "--brief", "edit declared paths", "--model", "sonnet", "--repo", repo, "--paths", "src/**", "--paths", "new, file.go"}
	first := captureStdoutErr(t, func() error { return cmdStart(args) })
	if first.err != nil {
		t.Fatal(first.err)
	}
	parent := strings.TrimSpace(first.stdout)
	second := captureStdoutErr(t, func() error {
		return cmdStart(append(slices.Clone(args), "--on-overlap", "serialize", "--parent", parent))
	})
	if second.err != nil {
		t.Fatal(second.err)
	}
	m, err := jobs.Open("test")
	if err != nil {
		t.Fatal(err)
	}
	defer m.Close()
	j, err := m.Store.Get(strings.TrimSpace(second.stdout))
	if err != nil {
		t.Fatal(err)
	}
	if j.Parent != parent || j.OnOverlap != "serialize" || !slices.Equal(j.Paths, []string{"src/**", "new, file.go"}) || len(j.Overlap) != 1 {
		t.Fatalf("CLI metadata/output: %+v %+v", j, second)
	}
	status := captureStdout(t, func() error { return cmdStatus([]string{"--agent", "test", j.ID}) })
	if !strings.Contains(status, "parent:    "+parent) || !strings.Contains(status, "serialized wait") {
		t.Fatalf("CLI status: %s", status)
	}
}
