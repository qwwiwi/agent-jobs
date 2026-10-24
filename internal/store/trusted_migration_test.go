package store

import (
	"path/filepath"
	"slices"
	"testing"
	"time"
)

func TestTrustedLaunchMigrationPreservesPopulatedRows(t *testing.T) {
	all := migrations
	defer func() { migrations = all }()
	start := 0
	for i, migration := range all {
		if migration == "ALTER TABLE jobs ADD COLUMN fallback_session TEXT NOT NULL DEFAULT ''" {
			start = i
			break
		}
	}
	for _, preadded := range []int{0, 1, 2, 3} {
		path := filepath.Join(t.TempDir(), "jobs.db")
		// The four trusted-launch migrations from fallback_session on, minus the pre-added ones, are
		// missing; later migrations (columns the current Insert writes) are kept.
		migrations = append(slices.Clone(all[:start+preadded]), all[start+4:]...)
		s, err := Open(path)
		if err != nil {
			t.Fatal(err)
		}
		for _, status := range []string{StatusRunning, StatusQueued, StatusFailed, StatusSucceeded} {
			j := &Job{ID: status, Agent: "test", Title: "existing", Kind: "general", Brief: "b", Model: "sonnet", Status: status, LaunchToken: "old-token", CostUSD: 2.5, Turns: 7, CreatedAt: time.Now()}
			if err := s.Insert(j); err != nil {
				t.Fatal(err)
			}
		}
		if _, err := s.db.Exec(`INSERT INTO attempts (job_id,n,mode,status,model,max_turns,max_budget_usd,timeout_minutes,turns,cost_usd,duration_sec,error,context,session_id)
   VALUES ('failed',1,'new','failed','sonnet',10,4,5,7,2.5,12,'old error','context','old-session')`); err != nil {
			t.Fatal(err)
		}
		if err := s.Close(); err != nil {
			t.Fatal(err)
		}
		migrations = all
		for range 2 {
			s, err = Open(path)
			if err != nil {
				t.Fatal(err)
			}
			for _, status := range []string{StatusRunning, StatusQueued, StatusFailed, StatusSucceeded} {
				j, err := s.Get(status)
				if err != nil || j.Status != status || j.CostUSD != 2.5 || j.Turns != 7 || j.LaunchToken != "old-token" {
					t.Fatalf("row changed: %+v %v", j, err)
				}
				if sid, err := s.FallbackSession(status, "old-token"); err != nil || sid != "" {
					t.Fatalf("fallback: %q %v", sid, err)
				}
				if _, ok, err := s.WrapperResult(status, "old-token"); err != nil || ok {
					t.Fatalf("result: %v %v", ok, err)
				}
			}
			if raw, err := s.AttemptWrapperResult("failed", 1); err != nil || raw != "" {
				t.Fatalf("historical attempt gained trusted usage: %q %v", raw, err)
			}
			attempts, err := s.Attempts("failed")
			if err != nil || len(attempts) != 1 || attempts[0].Turns != 7 || attempts[0].CostUSD != 2.5 || attempts[0].SessionID != "old-session" {
				t.Fatalf("attempt changed: %+v %v", attempts, err)
			}
			if err := s.Close(); err != nil {
				t.Fatal(err)
			}
		}
	}
}
