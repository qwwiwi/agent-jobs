package store

import (
	"path/filepath"
	"slices"
	"testing"
	"time"
)

// A job and a schedule carrying both skills (E7) and an acceptance command keep both: the column
// lists, placeholders and scans of the two features line up.
func TestSkillsAndAcceptanceRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "jobs.db")
	st, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Unix(1760000000, 0)
	j := &Job{ID: "j1", Agent: "a", Title: "t", Kind: "fix", Brief: "b", Status: StatusQueued, Model: "opus",
		MaxTurns: 1, MaxBudgetUSD: 1, TimeoutMinutes: 1, CreatedAt: now, AcceptanceCmd: "make test",
		Skills: []string{"golang", "git-workflows"}, SkillsDelivered: []string{"golang"}, SkillsNote: "repository wins"}
	if err := st.Insert(j); err != nil {
		t.Fatal(err)
	}
	sc, err := st.AddSchedule(&Schedule{Cron: "@daily", Prompt: "p", Enabled: true, Kind: "fix",
		AcceptanceCmd: "go test ./...", Skills: []string{"+golang", "-tdd"}}, now)
	if err != nil {
		t.Fatal(err)
	}
	_ = st.Close()
	// Reopen: the migrations of both features run again over the same database.
	if st, err = Open(path); err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	got, err := st.Get("j1")
	if err != nil {
		t.Fatal(err)
	}
	if got.AcceptanceCmd != "make test" || !slices.Equal(got.Skills, j.Skills) ||
		!slices.Equal(got.SkillsDelivered, j.SkillsDelivered) || got.SkillsNote != j.SkillsNote ||
		got.AcceptanceExit != nil || got.TimedOutAt != nil {
		t.Fatalf("job: %+v", got)
	}
	gs, err := st.GetSchedule(sc.ID)
	if err != nil {
		t.Fatal(err)
	}
	if gs.AcceptanceCmd != "go test ./..." || !slices.Equal(gs.Skills, []string{"+golang", "-tdd"}) {
		t.Fatalf("schedule: %+v", gs)
	}
}
