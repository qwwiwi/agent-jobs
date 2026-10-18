package store

import (
	"database/sql"
	"errors"
	"path/filepath"
	"testing"
	"time"
)

// A stage-1 database gains the schedules table on Open; a schedule round-trips, is edited in place,
// a firing is claimed once per minute (and once per local wall-clock minute), a second Open keeps
// the claim, and remove deletes the row.
func TestSchedulesRoundTripAndClaim(t *testing.T) {
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
	t0 := time.Unix(1770000000, 0)
	sc, err := s.AddSchedule(&Schedule{Cron: "0 9 * * *", Prompt: "p", Enabled: true, Kind: "review", Model: "sonnet",
		MaxTurns: 7, MaxBudgetUSD: 1.5, TimeoutMinutes: 9, RepoPath: "/r", Base: "main"}, t0)
	if err != nil {
		t.Fatal(err)
	}
	if sc.ID == 0 || sc.Kind != "review" || sc.MaxBudgetUSD != 1.5 || sc.Base != "main" || sc.LastFiredAt != nil {
		t.Fatalf("added: %+v", sc)
	}
	off := false
	if sc, err = s.SetSchedule(sc.ID, "", "q", &off, t0); err != nil || sc.Prompt != "q" || sc.Cron != "0 9 * * *" || sc.Enabled {
		t.Fatalf("set: %+v %v", sc, err)
	}
	minute := time.Unix(1770000000/60*60, 0)
	if ok, err := s.ClaimScheduleFiring(sc.ID, minute, "l1"); err != nil || ok {
		t.Fatalf("a disabled schedule was claimed: %t %v", ok, err)
	}
	on := true
	if _, err := s.SetSchedule(sc.ID, "", "", &on, t0); err != nil {
		t.Fatal(err)
	}
	for i, c := range []struct {
		at    time.Time
		local string
		want  bool
	}{
		{minute, "l1", true},
		{minute, "l1", false},                             // same minute
		{minute.Add(-time.Minute), "l0", false},           // earlier minute (clock stepped back)
		{minute.Add(time.Hour), "l1", false},              // same local wall clock (DST repeated hour)
		{minute.Add(time.Hour + time.Minute), "l2", true}, // later minute
	} {
		if ok, err := s.ClaimScheduleFiring(sc.ID, c.at, c.local); err != nil || ok != c.want {
			t.Fatalf("claim %d: %t %v, want %t", i, ok, err, c.want)
		}
	}
	if err := s.RecordScheduleFiring(sc.ID, minute, "stale", "x"); err != nil {
		t.Fatal(err)
	}
	if err := s.RecordScheduleFiring(sc.ID, minute.Add(time.Hour+time.Minute), "job-1", ""); err != nil {
		t.Fatal(err)
	}
	_ = s.Close()
	if s, err = Open(path); err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	got, err := s.GetSchedule(sc.ID)
	if err != nil || got.LastJobID != "job-1" || got.LastError != "" || !got.LastFiredAt.Equal(minute.Add(time.Hour+time.Minute)) {
		t.Fatalf("after reopen: %+v %v", got, err)
	}
	if ok, _ := s.ClaimScheduleFiring(sc.ID, minute.Add(time.Hour+time.Minute), "l2"); ok {
		t.Fatal("claim repeated after reopen")
	}
	if err := s.RemoveSchedule(sc.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.GetSchedule(sc.ID); !errors.Is(err, ErrScheduleNotFound) {
		t.Fatalf("after remove: %v", err)
	}
	if _, err := s.SetSchedule(sc.ID, "x", "", nil, t0); !errors.Is(err, ErrScheduleNotFound) {
		t.Fatalf("set removed: %v", err)
	}
}
