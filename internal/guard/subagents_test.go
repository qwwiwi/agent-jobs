package guard

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

func subagentCall(t *testing.T, tool string, input map[string]any) string {
	t.Helper()
	b, err := json.Marshal(map[string]any{"tool_name": tool, "cwd": "/tmp", "tool_input": input})
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func decide(t *testing.T, payload string, cfg Config) string {
	t.Helper()
	reason, err := Decide(strings.NewReader(payload), cfg)
	if err != nil {
		t.Fatal(err)
	}
	return reason
}

// Owner 2026-10-17: at most three subagents per job; Agent and its older name Task share one count.
func TestSubagentCapThreePerJob(t *testing.T) {
	cfg := Config{SubagentLog: filepath.Join(t.TempDir(), "job.log")}
	for i, tool := range []string{"Agent", "Task", "Agent"} {
		if r := decide(t, subagentCall(t, tool, map[string]any{"description": "part", "prompt": "p", "subagent_type": "general-purpose"}), cfg); r != "" {
			t.Fatalf("subagent %d (%s) refused: %s", i+1, tool, r)
		}
	}
	r := decide(t, subagentCall(t, "Agent", map[string]any{"description": "fourth", "prompt": "p"}), cfg)
	if !strings.Contains(r, "at most 3 subagents") {
		t.Fatalf("fourth subagent: %q, want the cap", r)
	}
	b, err := os.ReadFile(cfg.SubagentLog)
	if err != nil {
		t.Fatal(err)
	}
	if n := strings.Count(string(b), "\n"); n != 3 {
		t.Fatalf("log has %d lines, want 3 (a refused call is not recorded):\n%s", n, b)
	}
	if !strings.Contains(string(b), "general-purpose\tpart") {
		t.Fatalf("log lacks the subagent type and description:\n%s", b)
	}
}

func TestSubagentMustStayInsideTheJob(t *testing.T) {
	cfg := Config{SubagentLog: filepath.Join(t.TempDir(), "job.log")}
	for name, in := range map[string]map[string]any{
		"remote":     {"prompt": "p", "isolation": "remote"},
		"worktree":   {"prompt": "p", "isolation": "worktree"},
		"background": {"prompt": "p", "run_in_background": true},
		"team":       {"prompt": "p", "team_name": "crew"},
	} {
		if decide(t, subagentCall(t, "Agent", in), cfg) == "" {
			t.Errorf("%s subagent allowed", name)
		}
	}
	if _, err := os.Stat(cfg.SubagentLog); err == nil {
		b, _ := os.ReadFile(cfg.SubagentLog)
		if len(b) != 0 {
			t.Fatalf("refused subagents were counted:\n%s", b)
		}
	}
	// Explicit defaults are fine.
	if r := decide(t, subagentCall(t, "Agent", map[string]any{"prompt": "p", "run_in_background": false, "isolation": ""}), cfg); r != "" {
		t.Fatalf("plain foreground subagent refused: %s", r)
	}
}

// Fail closed: without a log the cap cannot be counted, so no subagent starts.
func TestSubagentWithoutLogRefused(t *testing.T) {
	if decide(t, subagentCall(t, "Agent", map[string]any{"prompt": "p"}), Config{}) == "" {
		t.Fatal("subagent allowed without a subagent log")
	}
}

func TestSubagentLogSymlinkRefused(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "elsewhere")
	if err := os.WriteFile(target, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dir, "job.log")
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
	if decide(t, subagentCall(t, "Agent", map[string]any{"prompt": "p"}), Config{SubagentLog: link}) == "" {
		t.Fatal("subagent counted through a symlinked log")
	}
}

// Parallel subagent calls of one message run the hook concurrently: the lock keeps the cap.
func TestSubagentCapHoldsUnderConcurrency(t *testing.T) {
	cfg := Config{SubagentLog: filepath.Join(t.TempDir(), "job.log")}
	payload := subagentCall(t, "Agent", map[string]any{"prompt": "p"})
	var mu sync.Mutex
	allowed := 0
	var wg sync.WaitGroup
	for range 12 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			r, err := Decide(strings.NewReader(payload), cfg)
			if err != nil {
				t.Error(err)
				return
			}
			if r == "" {
				mu.Lock()
				allowed++
				mu.Unlock()
			}
		}()
	}
	wg.Wait()
	if allowed != MaxSubagentsPerJob {
		t.Fatalf("%d subagents allowed concurrently, want %d", allowed, MaxSubagentsPerJob)
	}
}

// A subagent's own Bash calls go through the same hook: a nested job stays refused.
func TestSubagentCannotStartJobs(t *testing.T) {
	for _, cmd := range []string{
		"agent-jobs start --agent a --title t --model opus --brief b",
		"agent-jobs mcp --role orchestrator --agent a",
	} {
		if Check(cmd, Config{SubagentLog: "/x"}) == "" {
			t.Errorf("%q allowed", cmd)
		}
	}
}
