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

// job_amend, job_stop and job_resume over a real store (no daemon): amend a queued job, stop it,
// resume it with context, refuse what cannot be resumed, and list every attempt in job_result.
func TestJobStopResumeAmendTools(t *testing.T) {
	m := newManager(t)
	cs := connect(t, mustServer(t, m))

	var started JobView
	call(t, cs, "job_start", startArgs(nil), &started)
	var amended JobView
	call(t, cs, "job_amend", map[string]any{"id": started.ID, "context": "AMEND_1"}, &amended)
	if row, _ := m.Store.Get(started.ID); amended.Status != store.StatusQueued || !strings.Contains(row.Brief, "AMEND_1") {
		t.Fatalf("job_amend: %+v / %+v", amended, row)
	}
	var stopped JobView
	call(t, cs, "job_stop", map[string]any{"id": started.ID, "reason": "wait for the API"}, &stopped)
	if stopped.Status != store.StatusStopped || !strings.Contains(stopped.Error, "wait for the API") {
		t.Fatalf("job_stop: %+v", stopped)
	}
	if msg := callErr(t, cs, "job_amend", map[string]any{"id": started.ID, "context": "x"}); !strings.Contains(msg, "only a queued job") {
		t.Fatalf("amend of a stopped job: %q", msg)
	}
	var stoppedResult JobResultOutput
	call(t, cs, "job_result", map[string]any{"id": started.ID}, &stoppedResult)
	if !strings.Contains(stoppedResult.Note, "job_resume") {
		t.Fatalf("job_result of a stopped job: %+v", stoppedResult)
	}
	var listed JobListOutput
	call(t, cs, "job_list", map[string]any{"status": "stopped"}, &listed)
	if len(listed.Jobs) != 1 || listed.Jobs[0].ID != started.ID {
		t.Fatalf("job_list stopped: %+v", listed)
	}
	var resumed JobView
	call(t, cs, "job_resume", map[string]any{"id": started.ID, "context": "RESUME_1"}, &resumed)
	if row, _ := m.Store.Get(started.ID); resumed.Status != store.StatusQueued || !strings.Contains(row.Brief, "RESUME_1") {
		t.Fatalf("job_resume of a job stopped while queued: %+v / %+v", resumed, row)
	}
	if msg := callErr(t, cs, "job_resume", map[string]any{"id": started.ID, "context": "again"}); !strings.Contains(msg, "only stopped, timeout or failed") {
		t.Fatalf("resume of a queued job: %q", msg)
	}
	var cancelled JobCancelOutput
	call(t, cs, "job_cancel", map[string]any{"id": started.ID}, &cancelled)
	if msg := callErr(t, cs, "job_resume", map[string]any{"id": started.ID, "context": "x"}); !strings.Contains(msg, "start a new job") {
		t.Fatalf("resume of a cancelled job: %q", msg)
	}

	// A job that ran once and was stopped (as the daemon leaves it): resume queues attempt 2 with the
	// remaining limits; job_result lists both attempts.
	now := time.Now()
	ran := &store.Job{ID: "261017-120000-cafe", Agent: "test", Title: "ran", Kind: "fix", Brief: "b",
		Status: store.StatusStopped, Model: "opus", MaxTurns: 80, MaxBudgetUSD: 10, TimeoutMinutes: 45,
		CreatedAt: now, StartedAt: &now, FinishedAt: &now, Turns: 20, CostUSD: 1, DurationSec: 600,
		SessionID: jobs.NewSessionID(), Attempt: 1, RunMode: store.RunNew}
	if err := m.Store.Insert(ran); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(m.JobDir(ran.ID), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(m.JobDir(ran.ID), "prompt.md"), []byte("p"), 0o600); err != nil {
		t.Fatal(err)
	}
	if msg := callErr(t, cs, "job_resume", map[string]any{"id": ran.ID, "context": "x", "max_turns": 500}); !strings.Contains(msg, "not clamped") {
		t.Fatalf("limits above the ceiling: %q", msg)
	}
	call(t, cs, "job_resume", map[string]any{"id": ran.ID, "context": "NEXT"}, &resumed)
	if resumed.MaxTurns != 60 || resumed.MaxBudgetUSD != 9 || resumed.TimeoutMinutes != 35 || resumed.Attempt != 1 {
		t.Fatalf("remaining limits: %+v", resumed)
	}
	var res JobResultOutput
	call(t, cs, "job_result", map[string]any{"id": ran.ID}, &res)
	if len(res.Attempts) != 2 || res.Attempts[0].Status != store.StatusStopped || res.Attempts[0].Turns != 20 ||
		res.Attempts[1].Context != "NEXT" || res.Attempts[1].Status != store.StatusQueued {
		t.Fatalf("attempts: %+v", res.Attempts)
	}
	for tool, args := range map[string]map[string]any{"job_stop": {"id": "nope"}, "job_resume": {"id": "nope", "context": "x"},
		"job_amend": {"id": "nope", "context": "x"}} {
		if msg := callErr(t, cs, tool, args); !strings.Contains(msg, `job "nope" not found`) {
			t.Fatalf("%s unknown id: %q", tool, msg)
		}
	}
}
