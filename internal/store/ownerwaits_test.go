package store

import (
	"database/sql"
	"path/filepath"
	"slices"
	"testing"
	"time"
)

func insertJob(t *testing.T, st *Store, id string, ownerWaits bool, created time.Time) {
	t.Helper()
	if err := st.Insert(&Job{ID: id, Agent: "agent1", Title: "title " + id, Kind: "fix", Brief: "b",
		Status: StatusSucceeded, Model: "opus", MaxTurns: 10, MaxBudgetUSD: 1, TimeoutMinutes: 5, CreatedAt: created,
		OwnerWaits: ownerWaits}); err != nil {
		t.Fatal(err)
	}
}

func pendingIDs(t *testing.T, st *Store, now time.Time, maxAge time.Duration) ([]string, int) {
	t.Helper()
	list, expired, err := st.OwnerPendingList(now, maxAge)
	if err != nil {
		t.Fatal(err)
	}
	ids := []string{}
	for _, p := range list {
		ids = append(ids, p.JobID)
	}
	return ids, expired
}

// A delivered job-done line records «owner reply pending» only for an owner-waits job; a later line
// renews it; owner_replied clears one or all; records older than the max age expire.
func TestOwnerPendingLifecycle(t *testing.T) {
	st, err := Open(filepath.Join(t.TempDir(), "jobs.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	t0 := time.Unix(1760000000, 0)
	insertJob(t, st, "plain", false, t0)
	insertJob(t, st, "w1", true, t0)
	insertJob(t, st, "w2", true, t0)
	for _, id := range []string{"plain", "w1"} {
		if err := st.MarkNotified(id, t0); err != nil {
			t.Fatal(err)
		}
	}
	if err := st.MarkNotified("w2", t0.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	if j, _ := st.Get("plain"); j.NotifiedAt == nil {
		t.Fatal("notified_at not set")
	}
	list, _, err := st.OwnerPendingList(t0, 6*time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 2 || list[0].JobID != "w1" || list[0].Title != "title w1" || list[0].Status != StatusSucceeded ||
		!list[0].CreatedAt.Equal(t0) || list[1].JobID != "w2" {
		t.Fatalf("pending: %+v", list)
	}

	// w1 expires (older than 6 h at t0+6h30m), w2 does not.
	ids, expired := pendingIDs(t, st, t0.Add(6*time.Hour+30*time.Minute), 6*time.Hour)
	if !slices.Equal(ids, []string{"w2"}) || expired != 1 {
		t.Fatalf("after expiry: %v, expired %d", ids, expired)
	}
	// A renewed line (a resumed attempt) records it again with the new time.
	if err := st.MarkNotified("w1", t0.Add(7*time.Hour)); err != nil {
		t.Fatal(err)
	}
	if ids, _ := pendingIDs(t, st, t0.Add(7*time.Hour), 6*time.Hour); !slices.Equal(ids, []string{"w2", "w1"}) {
		t.Fatalf("renewed: %v", ids)
	}

	cleared, err := st.ClearOwnerPending("w2")
	if err != nil || !slices.Equal(cleared, []string{"w2"}) {
		t.Fatalf("clear w2: %v %v", cleared, err)
	}
	if cleared, err := st.ClearOwnerPending("w2"); err != nil || len(cleared) != 0 {
		t.Fatalf("clear w2 again: %v %v", cleared, err)
	}
	insertJob(t, st, "w3", true, t0)
	if err := st.MarkNotified("w3", t0.Add(7*time.Hour)); err != nil {
		t.Fatal(err)
	}
	cleared, err = st.ClearOwnerPending("")
	slices.Sort(cleared)
	if err != nil || !slices.Equal(cleared, []string{"w1", "w3"}) {
		t.Fatalf("clear all: %v %v", cleared, err)
	}
	if ids, _ := pendingIDs(t, st, t0.Add(7*time.Hour), 6*time.Hour); len(ids) != 0 {
		t.Fatalf("after clear all: %v", ids)
	}
}

// A database from before owner waits (no owner_waits column, no owner_pending table) opens, keeps
// its rows (owner_waits false) and gains the table; reopening is a no-op.
func TestOpenMigratesOwnerPending(t *testing.T) {
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
		if err != nil || j.OwnerWaits {
			t.Fatalf("old row: %+v %v", j, err)
		}
		if err := st.MarkNotified("old-1", time.Unix(1760000100, 0)); err != nil {
			t.Fatal(err)
		}
		if ids, _ := pendingIDs(t, st, time.Unix(1760000100, 0), time.Hour); len(ids) != 0 {
			t.Fatalf("an old job is never pending: %v", ids)
		}
		_ = st.Close()
	}
}
