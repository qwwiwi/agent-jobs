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
