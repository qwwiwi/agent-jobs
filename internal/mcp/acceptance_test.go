package mcp

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/qwwiwi/agent-jobs/internal/jobs"
	"github.com/qwwiwi/agent-jobs/internal/store"
)

// job_start takes acceptance_cmd (refused for review and research); job_result shows the acceptance
// command, its exit code, duration and log tail; the done line says the acceptance failed.
func TestAcceptanceTools(t *testing.T) {
	m := newManager(t)
	m.Cfg.AcceptanceAllowed = true
	cs := connect(t, mustServer(t, m))
	if msg := callErr(t, cs, "job_start", startArgs(func(a map[string]any) {
		a["kind"], a["acceptance_cmd"] = "review", "make test"
	})); !strings.Contains(msg, "acceptance_cmd does not apply to a review job") {
		t.Fatalf("review with acceptance_cmd: %q", msg)
	}
	var started JobView
	call(t, cs, "job_start", startArgs(func(a map[string]any) { a["kind"], a["acceptance_cmd"] = "fix", "make test" }), &started)
	if started.AcceptanceCmd != "make test" {
		t.Fatalf("job_start: %+v", started)
	}
	var pending JobResultOutput
	call(t, cs, "job_result", map[string]any{"id": started.ID}, &pending)
	if pending.Acceptance != nil {
		t.Fatalf("job_result of a queued job shows acceptance: %+v", pending.Acceptance)
	}

	now := time.Now()
	exit := 2
	done := &store.Job{ID: "261018-120000-acc1", Agent: "test", Title: "acc job", Kind: "fix", Brief: "b",
		Status: store.StatusFailed, Model: "opus", MaxTurns: 10, MaxBudgetUSD: 1, TimeoutMinutes: 5,
		CreatedAt: now.Add(-time.Hour), StartedAt: &now, FinishedAt: &now, Error: "acceptance failed (exit 2)",
		AcceptanceCmd: "make test", AcceptanceStartedAt: &now, AcceptanceExit: &exit, AcceptanceDurationSec: 4}
	if err := m.Store.Insert(done); err != nil {
		t.Fatal(err)
	}
	dir := m.JobDir(done.ID)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	// A token in the output, and a line cut by the tail cap: the tail shows neither.
	tok := "ghp_" + strings.Repeat("Q", 36)
	log := "export GH " + tok + "\n" + strings.Repeat("x", MaxAcceptanceTailBytes) + "\nFAIL: TestSomething " + tok + "\n"
	if err := os.WriteFile(filepath.Join(dir, jobs.AcceptanceLog), []byte(log), 0o600); err != nil {
		t.Fatal(err)
	}
	var res JobResultOutput
	call(t, cs, "job_result", map[string]any{"id": done.ID}, &res)
	a := res.Acceptance
	if a == nil || a.Cmd != "make test" || !a.Ran || a.Exit != 2 || a.TimedOut || a.DurationSec != 4 ||
		a.LogTail != "FAIL: TestSomething [redacted:github]\n" {
		t.Fatalf("acceptance view: %+v", a)
	}
	if !strings.Contains(res.DoneLine, "acceptance failed (exit 2)") {
		t.Fatalf("done line: %s", res.DoneLine)
	}
	found := false
	for _, f := range res.Artifacts {
		found = found || f == filepath.Join(dir, jobs.AcceptanceLog)
	}
	if !found {
		t.Fatalf("acceptance.log not among the artifacts: %v", res.Artifacts)
	}
}

// schedule_add carries acceptance_cmd to the schedule.
func TestScheduleAddAcceptance(t *testing.T) {
	m := newManager(t)
	m.Cfg.AcceptanceAllowed = true
	cs := connect(t, mustServer(t, m))
	var out ScheduleOutput
	call(t, cs, "schedule_add", map[string]any{"cron": "@daily", "prompt": "p", "kind": "fix", "model": "sonnet",
		"acceptance_cmd": "make test"}, &out)
	if out.Schedule.AcceptanceCmd != "make test" {
		t.Fatalf("schedule_add: %+v", out.Schedule.Schedule)
	}
}

// Without config acceptance_allowed, job_start and schedule_add refuse acceptance_cmd.
func TestAcceptanceNeedsOptIn(t *testing.T) {
	m := newManager(t)
	cs := connect(t, mustServer(t, m))
	if msg := callErr(t, cs, "job_start", startArgs(func(a map[string]any) {
		a["kind"], a["acceptance_cmd"] = "fix", "make test"
	})); !strings.Contains(msg, "acceptance_allowed") {
		t.Fatalf("job_start without opt-in: %q", msg)
	}
	if msg := callErr(t, cs, "schedule_add", map[string]any{"cron": "@daily", "prompt": "p", "kind": "fix", "model": "sonnet",
		"acceptance_cmd": "make test"}); !strings.Contains(msg, "acceptance_allowed") {
		t.Fatalf("schedule_add without opt-in: %q", msg)
	}
}
