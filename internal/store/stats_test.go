package store

import (
	"database/sql"
	"errors"
	"path/filepath"
	"testing"
	"time"
)

// A stage-1 database gains the job_stats table on Open; a row round-trips (count maps included) and
// ListStats filters by finish time.
func TestJobStatsRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "jobs.db")
	raw, err := sql.Open("sqlite", "file:"+path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := raw.Exec(stage1Schema); err != nil {
		t.Fatal(err)
	}
	_ = raw.Close()
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if _, err := s.GetStats("x"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing row: %v", err)
	}
	old, recent := time.Unix(1760000000, 0), time.Unix(1770000000, 0)
	in := &JobStats{ID: "a", Kind: "fix", Model: "opus", Status: StatusSucceeded, Turns: 5, CostUSD: 1.25,
		DurationSec: 60, CreatedAt: old, StartedAt: &old, FinishedAt: &old, Repo: "agent-jobs",
		ToolCalls: map[string]int{"Bash": 3}, Skills: map[string]int{"deploy": 1}, GuardRefusals: 2,
		GuardRules: map[string]int{"outward": 2}, Subagents: 1, HasReport: true, ResultSubtype: "success",
		Source: "job_dir", RecordedAt: recent}
	if err := s.PutStats(in); err != nil {
		t.Fatal(err)
	}
	if err := s.PutStats(&JobStats{ID: "b", Kind: "review", Model: "sonnet", Status: StatusFailed, CreatedAt: recent,
		FinishedAt: &recent, RecordedAt: recent}); err != nil {
		t.Fatal(err)
	}
	got, err := s.GetStats("a")
	if err != nil {
		t.Fatal(err)
	}
	if got.ToolCalls["Bash"] != 3 || got.Skills["deploy"] != 1 || got.GuardRules["outward"] != 2 || !got.HasReport ||
		got.FinishedAt.Unix() != old.Unix() || got.Repo != "agent-jobs" || got.CostUSD != 1.25 || got.Source != "job_dir" {
		t.Fatalf("round trip: %+v", got)
	}
	b, _ := s.GetStats("b")
	if b == nil || b.ToolCalls == nil || len(b.ToolCalls) != 0 || b.StartedAt != nil {
		t.Fatalf("nil maps stored as {}: %+v", b)
	}
	all, _ := s.ListStats(time.Time{})
	since, _ := s.ListStats(recent.Add(-time.Second))
	if len(all) != 2 || len(since) != 1 || since[0].ID != "b" {
		t.Fatalf("list: %d all, %d since", len(all), len(since))
	}
	in.Status = StatusFailed
	if err := s.PutStats(in); err != nil {
		t.Fatal(err)
	}
	if got, _ := s.GetStats("a"); got.Status != StatusFailed {
		t.Fatal("PutStats does not replace the row")
	}
	term, err := s.ListTerminal()
	if err != nil || len(term) != 1 || term[0].ID != "old-1" {
		t.Fatalf("terminal jobs: %v %v", term, err)
	}
}

// A database from before job_stats.cost_unknown gains the column with the cost of finished codex jobs,
// and of jobs whose attempts all ended without a cost, marked unknown: never a $0.00 known cost.
func TestJobStatsCostUnknownMigration(t *testing.T) {
	path := filepath.Join(t.TempDir(), "jobs.db")
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Unix(1770000000, 0)
	for _, j := range []*Job{
		{ID: "codex", Runner: RunnerCodex, Model: "gpt-5.5"},
		{ID: "claude", Runner: RunnerClaude, Model: "opus"},
		{ID: "killed", Runner: RunnerClaude, Model: "opus"},
		{ID: "resumed", Runner: RunnerClaude, Model: "opus"},
	} {
		j.Agent, j.Title, j.Kind, j.Brief, j.Status, j.CreatedAt = "a", "t", "review", "b", StatusFailed, now
		if err := s.Insert(j); err != nil {
			t.Fatal(err)
		}
		if err := s.PutStats(&JobStats{ID: j.ID, Kind: j.Kind, Model: j.Model, Status: j.Status, CreatedAt: now,
			FinishedAt: &now, RecordedAt: now}); err != nil {
			t.Fatal(err)
		}
	}
	// killed: its only attempt ended without a cost; resumed: the archived attempt reported one.
	for _, q := range []string{
		`UPDATE jobs SET cost_unknown=1 WHERE id IN ('killed','resumed')`,
		`INSERT INTO attempts (job_id, n, mode, status, model, max_turns, max_budget_usd, timeout_minutes, turns,
			cost_usd, cost_unknown, duration_sec, error, context, session_id) VALUES
			('resumed', 1, 'new', 'stopped', 'opus', 0, 0, 0, 3, 0.5, 0, 10, '', '', '')`,
	} {
		if _, err := s.db.Exec(q); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := s.db.Exec(`ALTER TABLE job_stats DROP COLUMN cost_unknown`); err != nil {
		t.Fatal(err)
	}
	_ = s.Close()

	s, err = Open(path)
	if err != nil {
		t.Fatal(err)
	}
	for id, want := range map[string]bool{"codex": true, "claude": false, "killed": true, "resumed": false} {
		st, err := s.GetStats(id)
		if err != nil {
			t.Fatal(err)
		}
		if st.CostUnknown != want {
			t.Errorf("%s: cost_unknown %v, want %v", id, st.CostUnknown, want)
		}
	}
	// The backfill runs with the column's migration only: a later Open keeps a rewritten row as it is.
	st, _ := s.GetStats("killed")
	st.CostUnknown = false
	if err := s.PutStats(st); err != nil {
		t.Fatal(err)
	}
	_ = s.Close()
	if s, err = Open(path); err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if st, _ := s.GetStats("killed"); st.CostUnknown {
		t.Error("the backfill ran again on a later Open")
	}
}

// A database from before job_stats.effort gains the column filled from the effort recorded on each job;
// a new row round-trips its effort.
func TestJobStatsEffortMigration(t *testing.T) {
	path := filepath.Join(t.TempDir(), "jobs.db")
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Unix(1770000000, 0)
	for _, j := range []*Job{
		{ID: "high", Runner: RunnerClaude, Model: "opus", ReasoningEffort: "high"},
		{ID: "old", Runner: RunnerClaude, Model: "opus"},
	} {
		j.Agent, j.Title, j.Kind, j.Brief, j.Status, j.CreatedAt = "a", "t", "fix", "b", StatusSucceeded, now
		if err := s.Insert(j); err != nil {
			t.Fatal(err)
		}
		if err := s.PutStats(&JobStats{ID: j.ID, Kind: j.Kind, Model: j.Model, Status: j.Status, CreatedAt: now,
			FinishedAt: &now, RecordedAt: now}); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := s.db.Exec(`ALTER TABLE job_stats DROP COLUMN effort`); err != nil {
		t.Fatal(err)
	}
	_ = s.Close()
	if s, err = Open(path); err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	for id, want := range map[string]string{"high": "high", "old": ""} {
		if st, err := s.GetStats(id); err != nil || st.Effort != want {
			t.Errorf("%s: effort %+v %v, want %q", id, st, err, want)
		}
	}
	if err := s.PutStats(&JobStats{ID: "new", Kind: "fix", Model: "opus", Effort: "xhigh", Status: StatusSucceeded,
		CreatedAt: now, FinishedAt: &now, RecordedAt: now}); err != nil {
		t.Fatal(err)
	}
	if st, _ := s.GetStats("new"); st == nil || st.Effort != "xhigh" {
		t.Fatalf("round trip: %+v", st)
	}
}
