package jobs

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/qwwiwi/agent-jobs/internal/store"
)

// idleManager is a manager whose wrapper closes claude after 1 s of silence after a result.
func idleManager(t *testing.T) *Manager {
	t.Helper()
	if runtime.GOOS != "linux" {
		t.Skip("idle-after-result reads /proc (Linux)")
	}
	m := newManager(t)
	if err := os.WriteFile(filepath.Join(m.StateDir, "config.json"), []byte(`{"idle_after_result_seconds": 1}`), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("AGENT_JOBS_SELF", selfBin)
	return m
}

// A result, then silence with only the job MCP server alive: the wrapper closes claude, the job
// succeeds with turns/cost of the last result, and report.md comes from the stream's report text.
func TestIdleAfterResultClosesFinishedJob(t *testing.T) {
	m := idleManager(t)
	start := time.Now()
	j := runToEnd(t, m, enqueue(t, m, "FAKE_IDLE_OK FAKE_IDLE_MCP", nil).ID)
	if j.Status != store.StatusSucceeded {
		t.Fatalf("job: %s %q", j.Status, j.Error)
	}
	if took := time.Since(start); took > 12*time.Second {
		t.Fatalf("closed after %s, want within the grace period", took)
	}
	if j.Turns != 6 || j.CostUSD != 0.75 {
		t.Fatalf("turns/cost %d/%v, want the last result's 6/0.75", j.Turns, j.CostUSD)
	}
	if !strings.Contains(j.Error, ReasonIdleClosed) {
		t.Fatalf("job row does not record the idle close: %q", j.Error)
	}
	dir := m.JobDir(j.ID)
	if r := readFile(t, filepath.Join(dir, "report.md")); !strings.HasPrefix(r, "## Summary\nidle ok") {
		t.Fatalf("report.md not taken from the stream's report text: %q", r)
	}
	s := readFile(t, filepath.Join(dir, "stderr.log"))
	if !strings.Contains(s, ReasonIdleClosed) {
		t.Fatalf("stderr.log does not record the idle close: %s", s)
	}
	// The MCP server stand-in was alive when claude was closed and did not block the close.
	pid := strings.TrimSpace(readFile(t, filepath.Join(dir, "mcp.pid")))
	if !strings.Contains(s, "job MCP server ["+pid+"]") {
		t.Fatalf("stderr.log does not show the live job MCP server (pid %s) at the close: %s", pid, s)
	}
}

// A result without any report text: closed, report.md says so, and the job fails (no report).
func TestIdleAfterResultWithoutReport(t *testing.T) {
	m := idleManager(t)
	j := runToEnd(t, m, enqueue(t, m, "FAKE_IDLE_NOREPORT", nil).ID)
	if j.Status != store.StatusFailed || !strings.Contains(j.Error, ReasonIdleClosed) {
		t.Fatalf("job: %s %q", j.Status, j.Error)
	}
	if r := readFile(t, filepath.Join(m.JobDir(j.ID), "report.md")); !strings.Contains(r, "ended without a report") {
		t.Fatalf("report.md: %q", r)
	}
}

// A failed result subtype, then silence: closed and failed.
func TestIdleAfterResultFailedSubtype(t *testing.T) {
	m := idleManager(t)
	j := runToEnd(t, m, enqueue(t, m, "FAKE_IDLE_FAIL", nil).ID)
	if j.Status != store.StatusFailed || !strings.Contains(j.Error, ReasonIdleClosed) ||
		!strings.Contains(j.Error, "error_max_turns") {
		t.Fatalf("job: %s %q", j.Status, j.Error)
	}
	if j.Turns != 9 {
		t.Fatalf("turns %d, want 9", j.Turns)
	}
}

// Still working after a result (a live child, a growing stream, a listed background task): never
// closed early; the job ends on claude's own exit as before.
func TestIdleAfterResultNotClosedWhileWorking(t *testing.T) {
	for _, mode := range []string{"FAKE_IDLE_CHILD", "FAKE_IDLE_CHATTY", "FAKE_IDLE_BGTASK"} {
		t.Run(mode, func(t *testing.T) {
			m := idleManager(t)
			start := time.Now()
			j := runToEnd(t, m, enqueue(t, m, mode, nil).ID)
			if j.Status != store.StatusSucceeded || j.Error != "" {
				t.Fatalf("job: %s %q", j.Status, j.Error)
			}
			if took := time.Since(start); took < 2500*time.Millisecond {
				t.Fatalf("job ended after %s: closed early", took)
			}
			if j.Turns != 3 {
				t.Fatalf("turns %d, want the final result's 3", j.Turns)
			}
			if _, err := os.Stat(filepath.Join(m.JobDir(j.ID), IdleClosedMarker)); err == nil {
				t.Fatal("idle marker written for a job that exited by itself")
			}
		})
	}
}

// A normal exit is unchanged with the rule on: no marker, no message.
func TestIdleAfterResultNormalExitUnchanged(t *testing.T) {
	m := idleManager(t)
	j := runToEnd(t, m, enqueue(t, m, "x", nil).ID)
	if j.Status != store.StatusSucceeded || j.Error != "" || j.Turns != 3 {
		t.Fatalf("job: %s %q turns %d", j.Status, j.Error, j.Turns)
	}
	if _, err := os.Stat(filepath.Join(m.JobDir(j.ID), IdleClosedMarker)); err == nil {
		t.Fatal("idle marker written for a normal exit")
	}
}

// A normal exit whose result text is empty: report.md comes from the last assistant text with the
// report sections.
func TestReportFromAssistantTextOnNormalExit(t *testing.T) {
	m := newManager(t)
	j := runToEnd(t, m, enqueue(t, m, "FAKE_ASSISTANT_REPORT", nil).ID)
	if j.Status != store.StatusSucceeded || j.Error != "" {
		t.Fatalf("job: %s %q", j.Status, j.Error)
	}
	if r := readFile(t, filepath.Join(m.JobDir(j.ID), "report.md")); r != "## Summary\nfrom assistant\n" {
		t.Fatalf("report.md: %q", r)
	}
}

func TestLastReportText(t *testing.T) {
	path := filepath.Join(t.TempDir(), "stream.jsonl")
	lines := `{"type":"assistant","message":{"content":[{"type":"text","text":"## Summary\nfirst"}]}}
not json
{"type":"assistant","message":{"content":[{"type":"tool_use","name":"Bash"},{"type":"text","text":"## Summary\nsecond"}]}}
{"type":"assistant","message":{"content":[{"type":"text","text":"no sections here"}]}}
{"type":"assistant","parent_tool_use_id":"toolu_1","message":{"content":[{"type":"text","text":"## Summary\nsubagent"}]}}
{"type":"assistant","message":{"content":[{"type":"text","text":"Quoting the contract: ## Summary"}]}}
{"type":"result","subtype":"success","result":"done"}
`
	if err := os.WriteFile(path, []byte(lines), 0o600); err != nil {
		t.Fatal(err)
	}
	if got := lastReportText(path); got != "## Summary\nsecond" {
		t.Fatalf("lastReportText = %q", got)
	}
}

func TestIdleConfig(t *testing.T) {
	load := func(v string) (Config, error) {
		dir := t.TempDir()
		if err := os.WriteFile(filepath.Join(dir, "config.json"), []byte(`{"idle_after_result_seconds": `+v+`}`), 0o600); err != nil {
			t.Fatal(err)
		}
		return LoadConfig(dir)
	}
	if _, err := load("-2"); err == nil {
		t.Fatal("idle_after_result_seconds below -1 must be rejected")
	}
	if c, err := load("-1"); err != nil || c.IdleAfterResult() != 0 {
		t.Fatalf("-1 must turn the rule off: %v, %v", c.IdleAfterResult(), err)
	}
	if c, err := load("5"); err != nil || c.IdleAfterResult() != 5*time.Second {
		t.Fatalf("idle grace %v, %v", c.IdleAfterResult(), err)
	}
	c, err := LoadConfig(t.TempDir())
	if err != nil || c.IdleAfterResult() != DefaultIdleAfterResult {
		t.Fatalf("default idle grace %v, %v", c.IdleAfterResult(), err)
	}
}

// The off switch: a report result followed by silence longer than any grace is not closed; the job
// ends on claude's own exit.
func TestIdleAfterResultOff(t *testing.T) {
	m := idleManager(t)
	if err := os.WriteFile(filepath.Join(m.StateDir, "config.json"), []byte(`{"idle_after_result_seconds": -1}`), 0o600); err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	j := runToEnd(t, m, enqueue(t, m, "FAKE_IDLE_SHORT", nil).ID)
	if j.Status != store.StatusSucceeded || j.Error != "" {
		t.Fatalf("job: %s %q", j.Status, j.Error)
	}
	if took := time.Since(start); took < 2500*time.Millisecond {
		t.Fatalf("job ended after %s: closed although the rule is off", took)
	}
	if _, err := os.Stat(filepath.Join(m.JobDir(j.ID), IdleClosedMarker)); err == nil {
		t.Fatal("idle marker written with the rule off")
	}
	// Off by the config, not by a rejected config falling back to the default grace.
	if s := readFile(t, filepath.Join(m.JobDir(j.ID), "stderr.log")); strings.Contains(s, "falls back to the default") {
		t.Fatalf("config rejected: %s", s)
	}
}

// An idle close keeps a report.md the job wrote itself when the last result is not a report.
func TestIdleAfterResultKeepsOwnReport(t *testing.T) {
	m := idleManager(t)
	j := runToEnd(t, m, enqueue(t, m, "FAKE_IDLE_OWNREPORT", nil).ID)
	if j.Status != store.StatusSucceeded || !strings.Contains(j.Error, ReasonIdleClosed) {
		t.Fatalf("job: %s %q", j.Status, j.Error)
	}
	if r := readFile(t, filepath.Join(m.JobDir(j.ID), "report.md")); r != "## Summary\nown report\n" {
		t.Fatalf("report.md: %q", r)
	}
}

// A normal exit with an empty result text: subagent output and a quoted heading are not the report.
func TestReportFallbackIgnoresSubagentAndQuotes(t *testing.T) {
	m := newManager(t)
	j := runToEnd(t, m, enqueue(t, m, "FAKE_SUBAGENT_REPORT", nil).ID)
	if j.Status != store.StatusFailed || j.Error != "exit 0 without a report" {
		t.Fatalf("job: %s %q", j.Status, j.Error)
	}
}

// A normal exit whose result is not a success: no report from the assistant text.
func TestReportFallbackOnlyAfterSuccess(t *testing.T) {
	m := newManager(t)
	j := runToEnd(t, m, enqueue(t, m, "FAKE_NONSUCCESS_REPORT", nil).ID)
	if j.Status != store.StatusFailed || j.Error != "exit 0 without a report" {
		t.Fatalf("job: %s %q", j.Status, j.Error)
	}
	if _, err := os.Stat(filepath.Join(m.JobDir(j.ID), "report.md")); err == nil {
		t.Fatal("report.md written from assistant text after a non-success result")
	}
}
