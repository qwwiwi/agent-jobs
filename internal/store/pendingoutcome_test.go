package store

import (
	"path/filepath"
	"slices"
	"testing"
	"time"
)

func TestPendingOutcomes(t *testing.T) {
	st, err := Open(filepath.Join(t.TempDir(), "jobs.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	now := time.Date(2026, 11, 5, 5, 0, 0, 0, time.UTC)
	cutoff := now.Add(-7 * 24 * time.Hour)
	old := cutoff.Add(-time.Second)
	for _, c := range []struct {
		id, agent, status string
		finished          *time.Time
		notified          bool
	}{
		{"success", "test", StatusSucceeded, &now, true},
		{"failure", "test", StatusFailed, &now, false},
		{"timeout", "test", StatusTimeout, &now, true},
		{"boundary", "test", StatusSucceeded, &cutoff, true},
		{"old", "test", StatusSucceeded, &old, true},
		{"cancelled", "test", StatusCancelled, &now, false},
		{"stopped", "test", StatusStopped, &now, false},
		{"queued", "test", StatusQueued, &now, false},
		{"running", "test", StatusRunning, &now, false},
		{"other-agent", "other", StatusSucceeded, &now, false},
		{"no-finish", "test", StatusSucceeded, nil, false},
		{"accepted", "test", StatusSucceeded, &now, true},
		{"discarded", "test", StatusFailed, &now, true},
	} {
		j := &Job{ID: c.id, Agent: c.agent, Title: c.id, Kind: "fix", Brief: "b", Status: c.status,
			Model: "sonnet", CreatedAt: cutoff, FinishedAt: c.finished}
		if c.notified {
			j.NotifiedAt = &now
		}
		if err := st.Insert(j); err != nil {
			t.Fatal(err)
		}
	}
	for _, id := range []string{"accepted", "discarded"} {
		if err := st.SetOutcome(id, "test", id, "", now); err != nil {
			t.Fatal(err)
		}
	}
	check := func(want []string) {
		t.Helper()
		rows, err := st.PendingOutcomes("test", cutoff)
		if err != nil {
			t.Fatal(err)
		}
		var ids []string
		for _, j := range rows {
			ids = append(ids, j.ID)
		}
		if !slices.Equal(ids, want) {
			t.Fatalf("pending outcomes: got %v, want %v", ids, want)
		}
	}
	check([]string{"timeout", "failure", "success", "boundary"})
	if err := st.SetOutcome("failure", "test", "discarded", "", now); err != nil {
		t.Fatal(err)
	}
	check([]string{"timeout", "success", "boundary"})
}
