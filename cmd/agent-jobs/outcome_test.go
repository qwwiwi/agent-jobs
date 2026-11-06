package main

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/qwwiwi/agent-jobs/internal/jobs"
	"github.com/qwwiwi/agent-jobs/internal/store"
)

func outcomeFixture(t *testing.T) *jobs.Manager {
	t.Helper()
	t.Setenv("AGENT_JOBS_HOME", t.TempDir())
	m, err := jobs.Open("test")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { m.Close() })
	fin := time.Now().Add(-time.Hour)
	for id, agent := range map[string]string{"a1": "test", "a2": "test", "alien": "other"} {
		if err := m.Store.Insert(&store.Job{ID: id, Agent: agent, Title: id, Kind: "general", Brief: "b",
			Status: store.StatusSucceeded, Model: "sonnet", CreatedAt: fin, FinishedAt: &fin}); err != nil {
			t.Fatal(err)
		}
	}
	return m
}

// The single form records one outcome; a repeat overwrites; a refused one exits non-zero.
func TestCmdOutcomeSingle(t *testing.T) {
	m := outcomeFixture(t)
	c := captureStdoutErr(t, func() error {
		return cmdOutcome([]string{"--agent", "test", "a1", "accepted", "--note", "ok"}, strings.NewReader(""))
	})
	if c.err != nil || !strings.Contains(c.stdout, "recorded a1: accepted") {
		t.Fatalf("single: %v %q", c.err, c.stdout)
	}
	c = captureStdoutErr(t, func() error { return cmdOutcome([]string{"--agent", "test", "a1", "discarded"}, strings.NewReader("")) })
	if j, _ := m.Store.Get("a1"); c.err != nil || j.Outcome != "discarded" || j.OutcomeNote != "" {
		t.Fatalf("overwrite: %v %+v", c.err, j)
	}
	c = captureStdoutErr(t, func() error {
		return cmdOutcome([]string{"--agent", "test", "alien", "accepted"}, strings.NewReader(""))
	})
	if c.err == nil || !strings.Contains(c.stdout, "failed alien: job alien belongs to agent other") {
		t.Fatalf("other agent's job: %v %q", c.err, c.stdout)
	}
	if err := cmdOutcome([]string{"--agent", "test", "a1"}, strings.NewReader("")); err == nil || !strings.Contains(err.Error(), "usage") {
		t.Fatalf("missing outcome: %v", err)
	}
}

// The batch form (stdin or file) answers each item; a failing one does not stop the rest, the exit is an error.
func TestCmdOutcomeBatch(t *testing.T) {
	m := outcomeFixture(t)
	in := `[{"id":"a1","outcome":"accepted","note":"n"},{"id":"nope","outcome":"accepted"},{"id":"alien","outcome":"accepted"},{"id":"a2","outcome":"bogus"},{"id":"a2","outcome":"rerun"}]`
	c := captureStdoutErr(t, func() error {
		return cmdOutcome([]string{"--agent", "test", "--input", "-", "--json"}, strings.NewReader(in))
	})
	if c.err == nil || !strings.Contains(c.err.Error(), "3 of 5 outcomes not recorded") {
		t.Fatalf("exit: %v", c.err)
	}
	var out struct {
		Recorded int                  `json:"recorded"`
		Failed   int                  `json:"failed"`
		Results  []jobs.OutcomeResult `json:"results"`
	}
	if err := json.Unmarshal([]byte(c.stdout), &out); err != nil || out.Recorded != 2 || out.Failed != 3 || len(out.Results) != 5 {
		t.Fatalf("json: %v %+v", err, out)
	}
	for id, want := range map[string]string{"a1": "accepted", "a2": "rerun", "alien": ""} {
		if j, _ := m.Store.Get(id); j.Outcome != want {
			t.Errorf("%s: outcome %q, want %q", id, j.Outcome, want)
		}
	}
	for _, bad := range []string{`[{"id":"a1","outcome":"accepted","notes":"typo"}]`, `[] x`, `{"items":[]}`, `[]`} {
		c = captureStdoutErr(t, func() error {
			return cmdOutcome([]string{"--agent", "test", "--input", "-"}, strings.NewReader(bad))
		})
		if c.err == nil {
			t.Errorf("input %q accepted", bad)
		}
	}
	if err := cmdOutcome([]string{"--agent", "test", "--input", "-", "a1", "accepted"}, strings.NewReader("[]")); err == nil {
		t.Fatal("--input with positionals accepted")
	}
}

// Incomplete items (no outcome, no id) fail on their own and the valid ones are recorded, as in MCP.
func TestCmdOutcomeIncompleteItems(t *testing.T) {
	m := outcomeFixture(t)
	in := `[{"id":"a1","outcome":"accepted"},{"id":"a2"},{"outcome":"accepted"},{"note":"n"}]`
	c := captureStdoutErr(t, func() error {
		return cmdOutcome([]string{"--agent", "test", "--input", "-", "--json"}, strings.NewReader(in))
	})
	if c.err == nil || !strings.Contains(c.err.Error(), "3 of 4 outcomes not recorded") {
		t.Fatalf("exit: %v", c.err)
	}
	var out struct {
		Recorded int                  `json:"recorded"`
		Failed   int                  `json:"failed"`
		Results  []jobs.OutcomeResult `json:"results"`
	}
	if err := json.Unmarshal([]byte(c.stdout), &out); err != nil || out.Recorded != 1 || out.Failed != 3 || len(out.Results) != 4 {
		t.Fatalf("json: %v %+v", err, out)
	}
	for id, want := range map[string]string{"a1": "accepted", "a2": ""} {
		if j, _ := m.Store.Get(id); j.Outcome != want {
			t.Errorf("%s: outcome %q, want %q", id, j.Outcome, want)
		}
	}
}
