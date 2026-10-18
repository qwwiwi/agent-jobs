package store

import (
	"database/sql"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// A database from before acceptance_cmd opens with the acceptance columns at their defaults: no
// command, never started, no exit code.
func TestAcceptanceColumnsMigrate(t *testing.T) {
	path := filepath.Join(t.TempDir(), "jobs.db")
	raw, err := sql.Open("sqlite", "file:"+path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := raw.Exec(stage1Schema); err != nil {
		t.Fatal(err)
	}
	// A schedules table as it was before acceptance_cmd.
	old := strings.Replace(schedulesSchema, ",\n\tacceptance_cmd   TEXT NOT NULL DEFAULT ''", "", 1)
	if old == schedulesSchema {
		t.Fatal("test setup: acceptance_cmd not found in schedulesSchema")
	}
	if _, err := raw.Exec(old + `INSERT INTO schedules (cron, prompt, kind, created_at, updated_at)
		VALUES ('@daily', 'p', 'fix', 1, 1);`); err != nil {
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
		if j.AcceptanceCmd != "" || j.AcceptanceStartedAt != nil || j.AcceptanceExit != nil || j.AcceptanceDurationSec != 0 ||
			j.AcceptanceDeadline != nil || j.AcceptancePGID != 0 || j.TimedOutAt != nil {
			t.Fatalf("migrated acceptance fields: %+v", j)
		}
		sc, err := st.GetSchedule(1)
		if err != nil {
			t.Fatal(err)
		}
		if sc.AcceptanceCmd != "" {
			t.Fatalf("migrated schedule acceptance_cmd %q", sc.AcceptanceCmd)
		}
		_ = st.Close()
	}
}

// The acceptance fields round-trip; only the owning launch records them; a resume (Requeue) resets the
// run fields but keeps the command.
func TestAcceptanceFieldsRoundTrip(t *testing.T) {
	st, err := Open(filepath.Join(t.TempDir(), "jobs.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	now := time.Unix(1760000000, 0)
	j := &Job{ID: "j1", Agent: "a", Title: "t", Kind: "fix", Brief: "b", Status: StatusQueued, Model: "opus",
		MaxTurns: 1, MaxBudgetUSD: 1, TimeoutMinutes: 1, CreatedAt: now, AcceptanceCmd: "make test"}
	if err := st.Insert(j); err != nil {
		t.Fatal(err)
	}
	if err := st.Claim("j1", "tok", now); err != nil {
		t.Fatal(err)
	}
	if err := st.SetAcceptanceStarted("j1", "other", now, now.Add(time.Hour)); err == nil {
		t.Fatal("another launch recorded the acceptance start")
	}
	if err := st.SetAcceptanceStarted("j1", "tok", now.Add(time.Minute), now.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	if err := st.SetAcceptancePGID("j1", "tok", 4242); err != nil {
		t.Fatal(err)
	}
	if got, err := st.Get("j1"); err != nil || got.AcceptancePGID != 4242 || !got.AcceptanceDeadline.Equal(now.Add(time.Hour)) {
		t.Fatalf("pgid/deadline: %+v %v", got, err)
	}
	if err := st.SetAcceptanceResult("j1", "tok", 2, 7); err != nil {
		t.Fatal(err)
	}
	got, err := st.Get("j1")
	if err != nil {
		t.Fatal(err)
	}
	if got.AcceptanceCmd != "make test" || got.AcceptanceStartedAt == nil || !got.AcceptanceStartedAt.Equal(now.Add(time.Minute)) ||
		got.AcceptanceExit == nil || *got.AcceptanceExit != 2 || got.AcceptanceDurationSec != 7 || got.AcceptancePGID != 0 {
		t.Fatalf("acceptance fields: %+v", got)
	}
	if ok, err := st.ClaimTimeout("j1", "tok", now.Add(2*time.Hour)); err != nil || !ok {
		t.Fatalf("claim timeout: %v %v", ok, err)
	}
	if err := st.Finish("j1", "tok", StatusFailed, 0, 0, 1, "acceptance failed (exit 2)", now); err != nil {
		t.Fatal(err)
	}
	if err := st.Requeue("j1", ResumeLimits{MaxTurns: 1, MaxBudgetUSD: 1, TimeoutMinutes: 1}, "ctx", ""); err != nil {
		t.Fatal(err)
	}
	got, err = st.Get("j1")
	if err != nil {
		t.Fatal(err)
	}
	if got.AcceptanceCmd != "make test" || got.AcceptanceStartedAt != nil || got.AcceptanceExit != nil || got.AcceptanceDurationSec != 0 ||
		got.AcceptanceDeadline != nil || got.AcceptancePGID != 0 || got.TimedOutAt != nil {
		t.Fatalf("after requeue: %+v", got)
	}
}

// ClaimTimeout is the daemon's side of the timeout race: it holds only past the job's timeout and past
// a recorded acceptance deadline, and after it the acceptance command can no longer start.
func TestClaimTimeout(t *testing.T) {
	st, err := Open(filepath.Join(t.TempDir(), "jobs.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	now := time.Unix(1760000000, 0)
	j := &Job{ID: "j1", Agent: "a", Title: "t", Kind: "fix", Brief: "b", Status: StatusQueued, Model: "opus",
		MaxTurns: 1, MaxBudgetUSD: 1, TimeoutMinutes: 10, CreatedAt: now, AcceptanceCmd: "make test"}
	if err := st.Insert(j); err != nil {
		t.Fatal(err)
	}
	if err := st.Claim("j1", "tok", now); err != nil {
		t.Fatal(err)
	}
	claim := func(at time.Time, token string) bool {
		t.Helper()
		ok, err := st.ClaimTimeout("j1", token, at)
		if err != nil {
			t.Fatal(err)
		}
		return ok
	}
	if claim(now.Add(10*time.Minute), "tok") {
		t.Fatal("claimed at the timeout, not past it")
	}
	if err := st.SetAcceptanceStarted("j1", "tok", now.Add(9*time.Minute), now.Add(30*time.Minute)); err != nil {
		t.Fatal(err)
	}
	if claim(now.Add(20*time.Minute), "tok") {
		t.Fatal("claimed before the recorded acceptance deadline")
	}
	if claim(now.Add(31*time.Minute), "other") {
		t.Fatal("another launch claimed the timeout")
	}
	if !claim(now.Add(31*time.Minute), "tok") || !claim(now.Add(31*time.Minute), "tok") {
		t.Fatal("no claim past the acceptance deadline (or not idempotent)")
	}
	if err := st.SetAcceptanceStarted("j1", "tok", now.Add(32*time.Minute), now.Add(time.Hour)); err == nil {
		t.Fatal("acceptance start recorded after the timeout claim")
	}
	if got, err := st.Get("j1"); err != nil || got.TimedOutAt == nil || !got.TimedOutAt.Equal(now.Add(31*time.Minute)) {
		t.Fatalf("timed_out_at: %+v %v", got, err)
	}
}
