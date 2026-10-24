package store

import (
	"database/sql"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// A database whose jobs table has the kind CHECK of the first two gate kinds (slicing, judge) is rebuilt
// on Open for briefing (F8): its rows stay, a briefing job row inserts, reopening is a no-op.
func TestKindCheckMigrationAddsBriefing(t *testing.T) {
	path := filepath.Join(t.TempDir(), "jobs.db")
	raw, err := sql.Open("sqlite", "file:"+path)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(schema, kindCheckNew) {
		t.Fatal("schema has no current kind check")
	}
	if _, err := raw.Exec(strings.Replace(schema, kindCheckNew, kindCheckGate, 1)); err != nil {
		t.Fatal(err)
	}
	if _, err := raw.Exec(`INSERT INTO jobs (id, agent, title, kind, brief, status, model, max_turns, max_budget_usd, timeout_minutes,
		created_at) VALUES ('old','a','t','slicing','b','succeeded','opus',1,1,1,1)`); err != nil {
		t.Fatal(err)
	}
	if _, err := raw.Exec(`INSERT INTO jobs (id, agent, title, kind, brief, status, model, max_turns, max_budget_usd, timeout_minutes,
		created_at) VALUES ('early','a','t','briefing','b','queued','opus',1,1,1,1)`); err == nil {
		t.Fatal("the old check took a briefing row")
	}
	_ = raw.Close()
	for i := range 2 {
		s, err := Open(path)
		if err != nil {
			t.Fatal(err)
		}
		if j, err := s.Get("old"); err != nil || j.Kind != "slicing" {
			t.Fatalf("old row: %+v %v", j, err)
		}
		if i == 0 {
			j := &Job{ID: "b1", Agent: "a", Title: "t", Kind: "briefing", Brief: "b", Status: StatusQueued, Model: "opus", MaxTurns: 1,
				MaxBudgetUSD: 1, TimeoutMinutes: 1, CreatedAt: time.Now(), Gate: &GateInput{Repo: "/r", ChecklistID: "demo", Version: 1}}
			if err := s.Insert(j); err != nil {
				t.Fatal(err)
			}
		}
		_ = s.Close()
	}
}

// A briefing result is written only under the running launch token of its job; it reads back whole by
// job, by checklist version and hash, and as the latest of the id.
func TestBriefingResultTokenBound(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "jobs.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	j := &Job{ID: "b1", Agent: "a", Title: "t", Kind: "briefing", Brief: "b", Status: StatusRunning, Model: "opus", MaxTurns: 1,
		MaxBudgetUSD: 1, TimeoutMinutes: 1, CreatedAt: time.Now(), LaunchToken: "tok", Attempt: 1}
	if err := s.Insert(j); err != nil {
		t.Fatal(err)
	}
	q := BriefingQuestion{N: 1, Closes: []string{"R01"}, Kind: "owner_visible", Q: "Dark or light?", Recommended: "light", Why: "brand"}
	r := BriefingResult{JobID: "b1", Repo: "/r", ChecklistID: "demo", Version: 2, ChecklistSHA256: "h2", Model: "opus",
		Runner: RunnerClaude, EnforcedBlind: true, Rounds: []BriefingRound{{Questions: []BriefingQuestion{q}}},
		Decided: []BriefingDecided{{Text: "ASSUMPTION: x", Closes: []string{"R02"}}}, CreatedAt: time.Now()}
	for _, tok := range []string{"", "forged"} {
		if _, err := s.PutBriefing(tok, r); !errors.Is(err, ErrNotRunningLaunch) {
			t.Fatalf("token %q: %v", tok, err)
		}
	}
	if _, err := s.PutBriefing("tok", BriefingResult{JobID: "b1", Repo: "/r"}); !errors.Is(err, ErrGateIncomplete) {
		t.Fatalf("incomplete: %v", err)
	}
	if _, err := s.PutBriefing("tok", r); err != nil {
		t.Fatal(err)
	}
	got, err := s.BriefingByJob("b1", 1)
	if err != nil || got == nil || got.Attempt != 1 || got.Rounds[0].Questions[0].Q != "Dark or light?" || got.Decided[0].Closes[0] != "R02" ||
		got.Adversarial == nil || !got.EnforcedBlind {
		t.Fatalf("BriefingByJob: %+v %v", got, err)
	}
	if got, err := s.BriefingByJob("b1", 2); err != nil || got != nil {
		t.Fatalf("another attempt: %+v %v", got, err)
	}
	if got, err := s.BriefingFor("/r", "demo", 2, "h2"); err != nil || got == nil || got.JobID != "b1" {
		t.Fatalf("BriefingFor: %+v %v", got, err)
	}
	if got, err := s.BriefingFor("/r", "demo", 3, "h2"); err != nil || got != nil {
		t.Fatalf("BriefingFor another version: %+v %v", got, err)
	}
	if got, err := s.LatestBriefing("/r", "demo"); err != nil || got == nil || got.Version != 2 {
		t.Fatalf("LatestBriefing: %+v %v", got, err)
	}
	if err := s.Finish("b1", "tok", StatusSucceeded, 0, 0, 0, "", time.Now()); err != nil {
		t.Fatal(err)
	}
	if _, err := s.PutBriefing("tok", r); !errors.Is(err, ErrNotRunningLaunch) {
		t.Fatalf("a finished job wrote a result: %v", err)
	}
}

// oldApprovalsSchema is checklist_approvals before the approved rows were recorded.
const oldApprovalsSchema = `CREATE TABLE checklist_approvals (
	seq              INTEGER PRIMARY KEY AUTOINCREMENT,
	repo             TEXT NOT NULL,
	checklist_id     TEXT NOT NULL,
	version          INTEGER NOT NULL,
	checklist_sha256 TEXT NOT NULL,
	brief_sha256     TEXT NOT NULL,
	quote            TEXT NOT NULL,
	approved_at      INTEGER NOT NULL
)`

// An approval records its rows and the whole BRIEF.md hash; one stored before they were recorded reads
// back without them (the columns are added on Open).
func TestApprovalRecordsRows(t *testing.T) {
	path := filepath.Join(t.TempDir(), "jobs.db")
	raw, err := sql.Open("sqlite", "file:"+path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := raw.Exec(oldApprovalsSchema); err != nil {
		t.Fatal(err)
	}
	if _, err := raw.Exec(`INSERT INTO checklist_approvals (repo, checklist_id, version, checklist_sha256, brief_sha256, quote,
		approved_at) VALUES ('/r','demo',2,'h2','b','yes',1)`); err != nil {
		t.Fatal(err)
	}
	_ = raw.Close()
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	a, err := s.LatestApproval("/r")
	if err != nil || a == nil || a.Version != 2 || a.Rows != nil || a.BriefFileSHA256 != "" {
		t.Fatalf("old approval: %+v %v", a, err)
	}
	rows := []GateRow{{ID: "R01", Verbatim: "a login page", Contract: "live", Rationale: "brief"}}
	if err := s.PutApproval(ChecklistApproval{Repo: "/r", ChecklistID: "demo", Version: 3, ChecklistSHA256: "h3", BriefSHA256: "b",
		Quote: "yes", ApprovedAt: time.Now(), Rows: rows, BriefFileSHA256: "f3"}); err != nil {
		t.Fatal(err)
	}
	a, err = s.LatestApproval("/r")
	if err != nil || a == nil || a.Version != 3 || len(a.Rows) != 1 || a.Rows[0] != rows[0] || a.BriefFileSHA256 != "f3" {
		t.Fatalf("new approval: %+v %v", a, err)
	}
}
