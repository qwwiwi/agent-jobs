package main

import (
	"strings"
	"testing"
	"time"

	"github.com/qwwiwi/agent-jobs/internal/jobs"
	"github.com/qwwiwi/agent-jobs/internal/store"
)

// `status` and `list` of a codex job say its cost is unknown and its turn/budget caps n/a, never $0.00.
func TestStatusListCodexCostUnknown(t *testing.T) {
	t.Setenv("AGENT_JOBS_HOME", t.TempDir())
	m, err := jobs.Open("test")
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	j := &store.Job{ID: "261018-120000-c0de", Agent: "test", Title: "codex review", Kind: "review", Brief: "b",
		Status: store.StatusSucceeded, Model: "gpt-6.1-sol", Runner: store.RunnerCodex, TimeoutMinutes: 45,
		CreatedAt: now, StartedAt: &now, FinishedAt: &now, Turns: 4, DurationSec: 600, CostUnknown: true}
	if err := m.Store.Insert(j); err != nil {
		t.Fatal(err)
	}
	_ = m.Close()
	out := captureStdout(t, func() error { return cmdStatus([]string{"--agent", "test", j.ID}) })
	for _, want := range []string{"limits:    n/a (codex) turns · n/a (codex) budget · 45m",
		"usage:     4 turns · cost unknown · 600s"} {
		if !strings.Contains(out, want) {
			t.Fatalf("status: missing %q:\n%s", want, out)
		}
	}
	out = captureStdout(t, func() error { return cmdList([]string{"--agent", "test"}) })
	if !strings.Contains(out, " unknown ") || strings.Contains(out, "$0.00") {
		t.Fatalf("list:\n%s", out)
	}
}
