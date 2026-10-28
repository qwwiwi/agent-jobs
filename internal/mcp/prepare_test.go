package mcp

import (
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/qwwiwi/agent-jobs/internal/jobs"
	"github.com/qwwiwi/agent-jobs/internal/store"
)

// job_status and job_result show the prepare step as the job's wrapper recorded it; the done line says
// it failed; prepare.log is among the artifacts.
func TestPrepareShownInStatusAndResult(t *testing.T) {
	m := newManager(t)
	cs := connect(t, mustServer(t, m))
	now := time.Now()
	j := &store.Job{ID: "261028-120000-prp1", Agent: "test", Title: "prep job", Kind: "fix", Brief: "b",
		Status: store.StatusQueued, Model: "opus", MaxTurns: 10, MaxBudgetUSD: 1, TimeoutMinutes: 5, CreatedAt: now.Add(-time.Hour)}
	if err := m.Store.Insert(j); err != nil {
		t.Fatal(err)
	}
	const token = "tok-prepare"
	if err := m.Store.Claim(j.ID, token, now); err != nil {
		t.Fatal(err)
	}
	fail := "prepare failed (exit 4 after 2s); see prepare.log"
	rec, err := json.Marshal(jobs.StreamResult{Completed: true, ExitCode: 1, PrepareFailure: fail,
		Prepare: &jobs.PrepareResult{Command: "pnpm install --frozen-lockfile", Dir: "terminal", Ran: true, Exit: 4, DurationSec: 2}})
	if err != nil {
		t.Fatal(err)
	}
	if err := m.Store.SetWrapperResult(j.ID, token, string(rec)); err != nil {
		t.Fatal(err)
	}
	if err := m.Store.Finish(j.ID, token, store.StatusFailed, 0, 0, 3, fail, now); err != nil {
		t.Fatal(err)
	}
	dir := m.JobDir(j.ID)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, jobs.PrepareLog), []byte("ERR_PNPM_OUTDATED_LOCKFILE\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	var st JobView
	call(t, cs, "job_status", map[string]any{"id": j.ID}, &st)
	if p := st.Prepare; p == nil || p.Command != "pnpm install --frozen-lockfile" || p.Dir != "terminal" || !p.Ran ||
		p.Exit != 4 || p.DurationSec != 2 {
		t.Fatalf("job_status prepare: %+v", p)
	}
	var res JobResultOutput
	call(t, cs, "job_result", map[string]any{"id": j.ID}, &res)
	if p := res.Prepare; p == nil || p.Exit != 4 || p.DurationSec != 2 {
		t.Fatalf("job_result prepare: %+v", p)
	}
	if !strings.Contains(res.DoneLine, "prepare failed (exit 4 after 2s)") || !strings.Contains(res.Error, fail) {
		t.Fatalf("done line %q, error %q", res.DoneLine, res.Error)
	}
	if !slices.Contains(res.Artifacts, filepath.Join(dir, jobs.PrepareLog)) {
		t.Fatalf("prepare.log not among the artifacts: %v", res.Artifacts)
	}
}
