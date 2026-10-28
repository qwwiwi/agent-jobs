package main

import (
	"errors"
	"testing"

	"github.com/qwwiwi/agent-jobs/internal/jobs"
)

// G02 is deferred (owner decision 2026-10-26): start --source is refused before anything is stored.
func TestStartSourceDisabled(t *testing.T) {
	t.Setenv("AGENT_JOBS_HOME", t.TempDir())
	err := cmdStart([]string{"--agent", "test", "--title", "t", "--kind", "research", "--brief", "b", "--model", "sonnet",
		"--source", "https://github.com/a/lib"})
	if !errors.Is(err, jobs.ErrSourcesDisabled) {
		t.Fatalf("start --source: want the sources-disabled refusal, got %v", err)
	}
	m, err := jobs.Open("test")
	if err != nil {
		t.Fatal(err)
	}
	defer m.Close()
	if all, err := m.Store.List(100); err != nil || len(all) != 0 {
		t.Fatalf("jobs after the refusal: %d %v", len(all), err)
	}
	captureStdout(t, func() error {
		return cmdStart([]string{"--agent", "test", "--title", "t", "--kind", "research", "--brief", "b", "--model", "sonnet"})
	})
	if all, err := m.Store.List(100); err != nil || len(all) != 1 {
		t.Fatalf("jobs without --source: %d %v", len(all), err)
	}
}
