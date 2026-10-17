package store

import (
	"database/sql"
	"errors"
	"path/filepath"
	"testing"
	"time"
)

// stage1Schema is the jobs table as stage 1 first shipped it (before launch_token and the stage-2 columns).
const stage1Schema = `
CREATE TABLE jobs (
	id TEXT PRIMARY KEY, agent TEXT NOT NULL, title TEXT NOT NULL,
	kind TEXT NOT NULL CHECK (kind IN ('review','fix','research','general')),
	brief TEXT NOT NULL, repo_path TEXT NOT NULL DEFAULT '', base_commit TEXT NOT NULL DEFAULT '',
	status TEXT NOT NULL CHECK (status IN ('queued','running','succeeded','failed','timeout','cancelled')),
	model TEXT NOT NULL, max_turns INTEGER NOT NULL, max_budget_usd REAL NOT NULL, timeout_minutes INTEGER NOT NULL,
	pid INTEGER NOT NULL DEFAULT 0, pgid INTEGER NOT NULL DEFAULT 0, created_at INTEGER NOT NULL,
	started_at INTEGER, finished_at INTEGER, notified_at INTEGER, cleaned_at INTEGER,
	turns INTEGER NOT NULL DEFAULT 0, cost_usd REAL NOT NULL DEFAULT 0, duration_sec INTEGER NOT NULL DEFAULT 0,
	error TEXT NOT NULL DEFAULT ''
);
INSERT INTO jobs (id, agent, title, kind, brief, status, model, max_turns, max_budget_usd, timeout_minutes, created_at)
VALUES ('old-1', 'agent1', 'old job', 'general', 'b', 'succeeded', 'opus', 80, 10, 45, 1760000000);
`

// A database from stage 1 opens, gains every new column with its default, keeps its rows, and a
// second Open (columns already there) is a no-op.
func TestOpenMigratesStage1Database(t *testing.T) {
	path := filepath.Join(t.TempDir(), "jobs.db")
	raw, err := sql.Open("sqlite", "file:"+path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := raw.Exec(stage1Schema); err != nil {
		t.Fatal(err)
	}
	_ = raw.Close()
	for range 2 {
		st, err := Open(path)
		if err != nil {
			t.Fatal(err)
		}
		j, err := st.Get("old-1")
		if err != nil {
			t.Fatal(err)
		}
		if j.Title != "old job" || j.LaunchToken != "" || j.Complexity != "" || j.Runner != RunnerClaude ||
			j.OwnerWaits || j.ResumedFrom != "" || j.ScheduleID != "" || j.Billing != BillingSubscription {
			t.Fatalf("migrated row: %+v", j)
		}
		_ = st.Close()
	}
}

// Every new column round-trips through Insert/Get, and the status filter of ListRecentByStatus holds.
func TestNewColumnsRoundTrip(t *testing.T) {
	st, err := Open(filepath.Join(t.TempDir(), "jobs.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	now := time.Unix(1760000000, 0)
	in := &Job{ID: "a", Agent: "agent1", Title: "t", Kind: "fix", Brief: "b", Status: StatusQueued, Model: "opus",
		MaxTurns: 10, MaxBudgetUSD: 1, TimeoutMinutes: 5, CreatedAt: now,
		Complexity: "hard", OwnerWaits: true, ResumedFrom: "prev", ScheduleID: "sch-1", Billing: "api"}
	if err := st.Insert(in); err != nil {
		t.Fatal(err)
	}
	if err := st.Insert(&Job{ID: "b", Agent: "agent1", Title: "t", Kind: "fix", Brief: "b", Status: StatusFailed,
		Model: "opus", MaxTurns: 10, MaxBudgetUSD: 1, TimeoutMinutes: 5, CreatedAt: now.Add(time.Second)}); err != nil {
		t.Fatal(err)
	}
	got, err := st.Get("a")
	if err != nil {
		t.Fatal(err)
	}
	if got.Complexity != "hard" || !got.OwnerWaits || got.ResumedFrom != "prev" || got.ScheduleID != "sch-1" ||
		got.Billing != "api" || got.Runner != RunnerClaude {
		t.Fatalf("round trip: %+v", got)
	}
	queued, err := st.ListRecentByStatus(StatusQueued, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(queued) != 1 || queued[0].ID != "a" {
		t.Fatalf("queued: %v", queued)
	}
}

func TestReserveSubagentMonotonic(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "jobs.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	now := time.Now()
	j := &Job{ID: "j1", Agent: "a", Title: "t", Kind: "general", Brief: "b", Status: StatusQueued, Model: "m", CreatedAt: now}
	if err := s.Insert(j); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.ReserveSubagent("j1", 3); err == nil {
		t.Fatal("a queued job got a subagent")
	}
	if _, err := s.db.Exec(`UPDATE jobs SET status = 'running' WHERE id = 'j1'`); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 3; i++ {
		prev, ok, err := s.ReserveSubagent("j1", 3)
		if err != nil || !ok || prev != i {
			t.Fatalf("reserve %d: prev=%d ok=%v err=%v", i, prev, ok, err)
		}
	}
	if prev, ok, err := s.ReserveSubagent("j1", 3); err != nil || ok || prev != 3 {
		t.Fatalf("fourth: prev=%d ok=%v err=%v", prev, ok, err)
	}
	if n, _ := s.SubagentCount("j1"); n != 3 {
		t.Fatalf("count %d", n)
	}
	if _, _, err := s.ReserveSubagent("nope", 3); !errors.Is(err, ErrNotFound) {
		t.Fatalf("unknown job: %v", err)
	}
}
