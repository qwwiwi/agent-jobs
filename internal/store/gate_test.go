package store

import (
	"database/sql"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// A database from before the gate kinds (stage-1 schema, kind CHECK without slicing and judge) is
// rebuilt on Open: its rows stay, a judge job row inserts.
func TestKindCheckMigration(t *testing.T) {
	path := filepath.Join(t.TempDir(), "jobs.db")
	raw, err := sql.Open("sqlite", "file:"+path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := raw.Exec(stage1Schema); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(stage1Schema, kindCheckOld) {
		t.Fatal("stage-1 schema has no old kind check")
	}
	if _, err := raw.Exec(`INSERT INTO jobs (id, agent, title, kind, brief, status, model, max_turns, max_budget_usd, timeout_minutes,
		created_at) VALUES ('old','a','t','fix','b','succeeded','opus',1,1,1,1)`); err != nil {
		t.Fatal(err)
	}
	_ = raw.Close()
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if j, err := s.Get("old"); err != nil || j.Kind != "fix" {
		t.Fatalf("old row: %+v %v", j, err)
	}
	g := &GateInput{Repo: "/r", ChecklistID: "demo", Version: 2, SHA: strings.Repeat("a", 40), Rows: []GateRow{{ID: "R01", Basis: "b"}}}
	j := &Job{ID: "judge1", Agent: "a", Title: "t", Kind: "judge", Brief: "b", Status: StatusQueued, Model: "opus", MaxTurns: 1,
		MaxBudgetUSD: 1, TimeoutMinutes: 1, CreatedAt: time.Now(), Gate: g}
	if err := s.Insert(j); err != nil {
		t.Fatal(err)
	}
	got, err := s.Get("judge1")
	if err != nil || got.Gate == nil || got.Gate.SHA != g.SHA || got.Gate.Rows[0].Basis != "b" {
		t.Fatalf("gate input round trip: %+v %v", got, err)
	}
	if err := s.Insert(&Job{ID: "bad", Agent: "a", Title: "t", Kind: "deploy", Brief: "b", Status: StatusQueued, Model: "opus",
		CreatedAt: time.Now()}); err == nil {
		t.Fatal("an unknown kind passed the check")
	}
}

// Gate results are written only under the running launch token of their job; the latest result per
// checklist id, version and hash is the one read; a result for another version does not match.
func TestGateResultsTokenBound(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "jobs.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	j := &Job{ID: "s1", Agent: "a", Title: "t", Kind: "slicing", Brief: "b", Status: StatusRunning, Model: "opus", MaxTurns: 1,
		MaxBudgetUSD: 1, TimeoutMinutes: 1, CreatedAt: time.Now(), LaunchToken: "tok"}
	if err := s.Insert(j); err != nil {
		t.Fatal(err)
	}
	r := SlicingResult{JobID: "s1", Repo: "/r", ChecklistID: "demo", Version: 2, ChecklistSHA256: "h2",
		Unmapped: []Quote{{Quote: "q"}}, CreatedAt: time.Now()}
	for _, tok := range []string{"", "forged"} {
		if _, err := s.PutSlicing(tok, r); !errors.Is(err, ErrNotRunningLaunch) {
			t.Fatalf("token %q: %v", tok, err)
		}
	}
	if _, err := s.PutSlicing("tok", r); err != nil {
		t.Fatal(err)
	}
	if got, err := s.SlicingFor("/r", "demo", 2, "h2"); err != nil || got == nil || got.Unmapped[0].Quote != "q" || got.Half == nil {
		t.Fatalf("SlicingFor: %+v %v", got, err)
	}
	for _, k := range []struct {
		v int
		h string
	}{{3, "h2"}, {2, "other"}} {
		if got, err := s.SlicingFor("/r", "demo", k.v, k.h); err != nil || got != nil {
			t.Fatalf("SlicingFor %d %s: %+v %v", k.v, k.h, got, err)
		}
	}
	if err := s.PutSlicingDecision(SlicingDecision{Repo: "/r", ChecklistID: "demo", Version: 2, ChecklistSHA256: "h2", Unmapped: "q"}); err == nil {
		t.Fatal("a decision without the owner's words stored")
	}
	if err := s.Finish("s1", "tok", StatusSucceeded, 0, 0, 0, "", time.Now()); err != nil {
		t.Fatal(err)
	}
	if _, err := s.PutSlicing("tok", r); !errors.Is(err, ErrNotRunningLaunch) {
		t.Fatalf("a finished job wrote a result: %v", err)
	}
}
