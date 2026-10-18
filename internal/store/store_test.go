package store

import (
	"database/sql"
	"errors"
	"path/filepath"
	"slices"
	"strings"
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

// Databases created by any parallel branch (with or without `subagents`, the scope columns, the
// stop/resume columns) open, gain the missing columns, and reopen as a no-op.
func TestOpenMigratesEitherBranchDatabase(t *testing.T) {
	resume := []string{"session_id", "attempt", "run_mode", "pending_context", "cost_unknown", "session_digest"}
	for name, skip := range map[string][]string{
		"subagents only": append([]string{"scope_"}, resume...), "scope only": append([]string{"subagents"}, resume...),
		"stop/resume without scope": {"scope_"}, "scope without stop/resume": resume,
	} {
		t.Run(name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "jobs.db")
			raw, err := sql.Open("sqlite", "file:"+path)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := raw.Exec(stage1Schema); err != nil {
				t.Fatal(err)
			}
			for _, alter := range migrations {
				// Other tables (attempts) are created by Open itself, before the migrations run.
				if strings.HasPrefix(alter, "ALTER TABLE jobs ") && !slices.ContainsFunc(skip, func(c string) bool {
					return strings.Contains(alter, "COLUMN "+c)
				}) {
					if _, err := raw.Exec(alter); err != nil {
						t.Fatal(err)
					}
				}
			}
			_ = raw.Close()
			for range 2 {
				st, err := Open(path)
				if err != nil {
					t.Fatal(err)
				}
				if n, err := st.SubagentCount("old-1"); err != nil || n != 0 {
					t.Fatalf("subagents = %d, %v", n, err)
				}
				j, err := st.Get("old-1")
				if err != nil {
					t.Fatal(err)
				}
				if j.ScopeUnit != "" || j.ScopePending {
					t.Fatalf("scope columns: %+v", j)
				}
				if j.SessionID != "" || j.Attempt != 0 || j.RunMode != "" || j.CostUnknown {
					t.Fatalf("session columns: %+v", j)
				}
				_ = st.Close()
			}
		})
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

// A database whose status CHECK predates «stopped» is rebuilt once: rows, columns and the index kept,
// stopped accepted afterwards; a second Open leaves it alone.
func TestOpenMigratesStatusCheckForStopped(t *testing.T) {
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
		var def string
		if err := st.db.QueryRow(`SELECT sql FROM sqlite_master WHERE name='jobs'`).Scan(&def); err != nil {
			t.Fatal(err)
		}
		if strings.Count(def, "'stopped'") != 1 || !strings.Contains(def, "session_id") {
			t.Fatalf("definition: %s", def)
		}
		var idx int
		if err := st.db.QueryRow(`SELECT COUNT(*) FROM sqlite_master WHERE type='index' AND name='jobs_status'`).Scan(&idx); err != nil || idx != 1 {
			t.Fatalf("index: %d %v", idx, err)
		}
		if j, err := st.Get("old-1"); err != nil || j.Title != "old job" || j.Status != StatusSucceeded {
			t.Fatalf("row kept: %+v %v", j, err)
		}
		_ = st.Close()
	}
	st, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	if err := st.Insert(&Job{ID: "q", Agent: "a", Title: "t", Kind: "general", Brief: "b", Status: StatusQueued,
		Model: "m", CreatedAt: time.Now()}); err != nil {
		t.Fatal(err)
	}
	if err := st.Finish("q", "", StatusStopped, 0, 0, 0, "stopped", time.Now()); err != nil {
		t.Fatalf("stop on a migrated database: %v", err)
	}
}

// Requeue archives the attempt that ran and resets the run fields; it only takes stopped, timeout
// and failed rows. Amend appends to a queued brief only.
func TestRequeueArchivesAttemptAndAmend(t *testing.T) {
	st, err := Open(filepath.Join(t.TempDir(), "jobs.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	now := time.Unix(1760000000, 0)
	if err := st.Insert(&Job{ID: "j", Agent: "a", Title: "t", Kind: "fix", Brief: "b", Status: StatusQueued, Model: "m",
		MaxTurns: 80, MaxBudgetUSD: 10, TimeoutMinutes: 45, CreatedAt: now}); err != nil {
		t.Fatal(err)
	}
	if err := st.Amend("j", " more"); err != nil {
		t.Fatal(err)
	}
	if err := st.Claim("j", "tok", now); err != nil {
		t.Fatal(err)
	}
	if err := st.Amend("j", "late"); !errors.Is(err, ErrImmutable) {
		t.Fatalf("amend of a running job: %v", err)
	}
	if err := st.SetSession("j", "tok", "sid-1", RunNew); err != nil {
		t.Fatal(err)
	}
	if err := st.Requeue("j", ResumeLimits{MaxTurns: 1, MaxBudgetUSD: 1, TimeoutMinutes: 1}, "x", ""); !errors.Is(err, ErrImmutable) {
		t.Fatalf("requeue of a running job: %v", err)
	}
	if err := st.Finish("j", "tok", StatusTimeout, 30, 2.5, 2700, "timeout after 45 minutes", now); err != nil {
		t.Fatal(err)
	}
	if err := st.MarkNotified("j", now); err != nil {
		t.Fatal(err)
	}
	if err := st.Requeue("j", ResumeLimits{MaxTurns: 50, MaxBudgetUSD: 7.5, TimeoutMinutes: 30}, "update", ""); err != nil {
		t.Fatal(err)
	}
	j, err := st.Get("j")
	if err != nil {
		t.Fatal(err)
	}
	if j.Status != StatusQueued || j.Brief != "b more" || j.Attempt != 1 || j.MaxTurns != 50 || j.Turns != 0 ||
		j.NotifiedAt != nil || j.FinishedAt != nil || j.StartedAt != nil || j.Error != "" || j.PendingContext != "update" || j.SessionID != "sid-1" {
		t.Fatalf("requeued: %+v", j)
	}
	atts, err := st.Attempts("j")
	if err != nil || len(atts) != 1 {
		t.Fatalf("attempts: %+v %v", atts, err)
	}
	if a := atts[0]; a.N != 1 || a.Mode != RunNew || a.Status != StatusTimeout || a.Turns != 30 || a.CostUSD != 2.5 ||
		a.MaxTurns != 80 || a.SessionID != "sid-1" || a.Error != "timeout after 45 minutes" || a.FinishedAt == nil {
		t.Fatalf("attempt 1: %+v", a)
	}
}

// A requeued attempt that is stopped before it ran is not archived again by the next Requeue (it
// would collide with the attempt already archived); its update is kept by the caller.
func TestRequeueOfNeverRunAttemptSkipsArchive(t *testing.T) {
	st, err := Open(filepath.Join(t.TempDir(), "jobs.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	now := time.Unix(1760000000, 0)
	if err := st.Insert(&Job{ID: "j", Agent: "a", Title: "t", Kind: "fix", Brief: "b", Status: StatusQueued, Model: "m",
		MaxTurns: 80, MaxBudgetUSD: 10, TimeoutMinutes: 45, CreatedAt: now}); err != nil {
		t.Fatal(err)
	}
	if err := st.Claim("j", "tok", now); err != nil {
		t.Fatal(err)
	}
	if err := st.Finish("j", "", StatusStopped, 1, 0, 60, "stopped", now); err != nil {
		t.Fatal(err)
	}
	if err := st.Requeue("j", ResumeLimits{MaxTurns: 9, MaxBudgetUSD: 3, TimeoutMinutes: 30}, "u1", ""); err != nil {
		t.Fatal(err)
	}
	if err := st.AmendContext("j", "\n\nu2"); err != nil {
		t.Fatal(err)
	}
	if err := st.Finish("j", "", StatusStopped, 0, 0, 0, "stopped while queued", now); err != nil {
		t.Fatal(err)
	}
	if err := st.Requeue("j", ResumeLimits{MaxTurns: 9, MaxBudgetUSD: 3, TimeoutMinutes: 30}, "u1\n\nu2\n\nu3", ""); err != nil {
		t.Fatalf("second requeue: %v", err)
	}
	atts, err := st.Attempts("j")
	if err != nil || len(atts) != 1 || atts[0].Status != StatusStopped {
		t.Fatalf("attempts: %+v %v", atts, err)
	}
	j, err := st.Get("j")
	if err != nil || j.Status != StatusQueued || j.PendingContext != "u1\n\nu2\n\nu3" {
		t.Fatalf("row: %+v %v", j, err)
	}
	if err := st.SetCostUnknown("j"); err == nil {
		t.Fatal("cost unknown set on a queued row")
	}
	if err := st.SetSessionDigest("j", "abc"); err != nil {
		t.Fatal(err)
	}
	if j, _ := st.Get("j"); j.SessionDigest != "abc" {
		t.Fatalf("digest: %+v", j)
	}
}

// A database from before redaction_pending: every job that existed then is marked pending (queued
// and running ones write their report later, finished ones were never redacted by this build), once.
func TestRedactionPendingMigrationBackfill(t *testing.T) {
	path := filepath.Join(t.TempDir(), "jobs.db")
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Unix(1770000000, 0)
	statuses := map[string]string{"q": StatusQueued, "r": StatusRunning, "done": StatusSucceeded, "stop": StatusStopped}
	for id, st := range statuses {
		j := &Job{ID: id, Agent: "a", Title: "t", Kind: "general", Brief: "b", Status: st, Model: "opus", CreatedAt: now}
		if err := s.Insert(j); err != nil {
			t.Fatal(err)
		}
	}
	// The pre-migration fixture: the same database without the column.
	if _, err := s.db.Exec(`ALTER TABLE jobs DROP COLUMN redaction_pending`); err != nil {
		t.Fatal(err)
	}
	_ = s.Close()
	if s, err = Open(path); err != nil {
		t.Fatal(err)
	}
	for id := range statuses {
		var p int
		if err := s.db.QueryRow(`SELECT redaction_pending FROM jobs WHERE id=?`, id).Scan(&p); err != nil || p != 1 {
			t.Errorf("%s: redaction_pending %d %v, want 1", id, p, err)
		}
	}
	ids, err := s.RedactionPending()
	if slices.Sort(ids); err != nil || !slices.Equal(ids, []string{"done", "stop"}) {
		t.Fatalf("pending: %v %v", ids, err)
	}
	// Once only: a later Open keeps a cleared mark.
	if err := s.ClearRedactionPending("done"); err != nil {
		t.Fatal(err)
	}
	_ = s.Close()
	if s, err = Open(path); err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if ids, _ := s.RedactionPending(); !slices.Equal(ids, []string{"stop"}) {
		t.Fatalf("pending after a second open: %v", ids)
	}
}

// Finish persists the pending mark with the terminal status, before any redaction is tried: a running
// row whose mark was never set (claimed by an older build) is still redacted.
func TestFinishMarksRedactionPending(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "jobs.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	now := time.Unix(1770000000, 0)
	j := &Job{ID: "r", Agent: "a", Title: "t", Kind: "general", Brief: "b", Status: StatusRunning, Model: "opus", CreatedAt: now}
	if err := s.Insert(j); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec(`UPDATE jobs SET redaction_pending=0, launch_token='tok' WHERE id='r'`); err != nil {
		t.Fatal(err)
	}
	if err := s.Finish("r", "tok", StatusFailed, 1, 0, 1, "x", now); err != nil {
		t.Fatal(err)
	}
	if ids, err := s.RedactionPending(); err != nil || !slices.Equal(ids, []string{"r"}) {
		t.Fatalf("pending after finish: %v %v", ids, err)
	}
}
