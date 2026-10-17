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
		"remote":           {"prompt": "p", "isolation": "remote"},
		"worktree":         {"prompt": "p", "isolation": "worktree"},
		"empty isolation":  {"prompt": "p", "isolation": ""},
		"background":       {"prompt": "p", "run_in_background": true},
		"background false": {"prompt": "p", "run_in_background": false},
		"team":             {"prompt": "p", "team_name": "crew"},
		"unknown key":      {"prompt": "p", "mode": "plan"},
		"non-string":       {"prompt": 1},
	} {
		if decide(t, subagentCall(t, "Agent", in), cfg) == "" {
			t.Errorf("%s subagent allowed", name)
		}
	}
	if b, err := os.ReadFile(cfg.SubagentLog); err == nil && len(b) != 0 {
		t.Fatalf("refused subagents were counted:\n%s", b)
	}
}

// Claude Code matches keys case-sensitively, encoding/json does not and lets the last duplicate win:
// the guard reads the raw keys, so neither a case variant nor a duplicate can hide a denied field.
func TestSubagentInputKeyDifferentials(t *testing.T) {
	cfg := Config{SubagentLog: filepath.Join(t.TempDir(), "job.log")}
	for name, input := range map[string]string{
		"case variant hides isolation": `{"prompt":"p","isolation":"worktree","Isolation":""}`,
		"case variant of allowed key":  `{"Prompt":"p"}`,
		"duplicate key":                `{"prompt":"p","prompt":"q"}`,
		"duplicate description":        `{"description":"a","prompt":"p","description":"b"}`,
		"unknown key":                  `{"prompt":"p","cwd":"/"}`,
		"not an object":                `["prompt"]`,
		"null":                         `null`,
	} {
		payload := `{"tool_name":"Agent","cwd":"/tmp","tool_input":` + input + `}`
		// An error fails closed too: the hook exits 2 on it.
		if r, err := Decide(strings.NewReader(payload), cfg); r == "" && err == nil {
			t.Errorf("%s: allowed", name)
		}
	}
	// Each allowed key alone passes (the cap is not reached: four keys, so use a fresh log each time).
	for _, k := range []string{"description", "prompt", "subagent_type", "model"} {
		c := Config{SubagentLog: filepath.Join(t.TempDir(), "job.log")}
		payload := `{"tool_name":"Task","cwd":"/tmp","tool_input":{"` + k + `":"v"}}`
		if r := decide(t, payload, c); r != "" {
			t.Errorf("key %s alone refused: %s", k, r)
		}
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

// H1: with the store counter configured, a shortened log refuses further subagents, and the cap
// comes from the counter even when the log is empty.
func TestSubagentCounterAndTamper(t *testing.T) {
	count := 0
	reserve := func(max int) (int, bool, error) {
		if count >= max {
			return count, false, nil
		}
		count++
		return count - 1, true, nil
	}
	cfg := Config{SubagentLog: filepath.Join(t.TempDir(), "job.log"), SubagentReserve: reserve}
	call := subagentCall(t, "Agent", map[string]any{"prompt": "p"})
	for i := 0; i < 2; i++ {
		if r := decide(t, call, cfg); r != "" {
			t.Fatalf("subagent %d refused: %s", i+1, r)
		}
	}
	if err := os.WriteFile(cfg.SubagentLog, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if r := decide(t, call, cfg); !strings.Contains(r, "shortened") {
		t.Fatalf("after truncation: %q, want tampering", r)
	}
	count = 3
	cfg.SubagentLog = filepath.Join(t.TempDir(), "fresh.log")
	if r := decide(t, call, cfg); r == "" {
		t.Fatal("counter at the cap with an empty log: allowed")
	}
}

// H1: protected paths cannot be written by file tools (also through a symlink in the job dir) or
// by literal Bash targets; plain readers may read them.
func TestProtectedPaths(t *testing.T) {
	state := t.TempDir()
	logs := filepath.Join(state, "subagents")
	if err := os.MkdirAll(logs, 0o700); err != nil {
		t.Fatal(err)
	}
	logf := filepath.Join(logs, "j.log")
	if err := os.WriteFile(logf, []byte("x\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	jobdir := t.TempDir()
	if err := os.Symlink(logs, filepath.Join(jobdir, "l")); err != nil {
		t.Fatal(err)
	}
	cfg := Config{Protected: []string{logs, filepath.Join(state, "jobs.db")}, Dir: jobdir}
	for _, p := range []string{logf, filepath.Join(jobdir, "l", "j.log"), filepath.Join(state, "jobs.db"), "l/new.log"} {
		if CheckFileWrite("Write", p, cfg) == "" {
			t.Errorf("Write %s allowed", p)
		}
	}
	if CheckFileWrite("Write", filepath.Join(jobdir, "ok.txt"), cfg) != "" {
		t.Error("Write in the job dir refused")
	}
	for _, c := range []string{
		"echo > " + logf, ": >> " + logf, "rm " + logf, "mv " + logf + " /tmp/x", "truncate -s0 " + logf,
		"cp /dev/null " + logf, "tee " + logf, "sed -i d " + logf, "ln -sf /dev/null " + logf,
		"cd " + logs + " && rm j.log", "rm l/j.log", "printf x > l/j.log",
	} {
		if Check(c, cfg) == "" {
			t.Errorf("%q allowed", c)
		}
	}
	for _, c := range []string{"cat " + logf, "wc -l " + logf, "echo hi > out.txt", "go test ./..."} {
		if r := Check(c, cfg); r != "" {
			t.Errorf("%q refused: %s", c, r)
		}
	}
}
