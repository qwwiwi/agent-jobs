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

// Codex review (PR #25): the local wall-clock minute is a watermark. In the repeated hour at the
// end of DST the absolute time goes on while the local minute goes back; a claim needs both to be
// strictly later than the last firing, so 02:00 and 02:30 of the second pass are refused.
func TestScheduleClaimLocalWatermark(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "jobs.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	sc, err := s.AddSchedule(&Schedule{Cron: "*/30 2 * * *", Prompt: "p", Enabled: true}, time.Unix(1770000000, 0))
	if err != nil {
		t.Fatal(err)
	}
	first := time.Date(2026, 10, 25, 0, 0, 0, 0, time.UTC) // 02:00 CEST
	for i, c := range []struct {
		at    time.Time
		local string
		want  bool
	}{
		{first, "2026-10-25 02:00", true},
		{first.Add(30 * time.Minute), "2026-10-25 02:30", true},
		{first.Add(time.Hour), "2026-10-25 02:00", false},                  // 02:00 CET: local went back
		{first.Add(90 * time.Minute), "2026-10-25 02:30", false},           // 02:30 CET: same local minute
		{first.Add(26 * time.Hour), "2026-10-26 02:00", true},              // next day
		{first.Add(26*time.Hour - time.Minute), "2026-10-26 01:59", false}, // absolute went back
	} {
		if ok, err := s.ClaimScheduleFiring(sc.ID, c.at, c.local); err != nil || ok != c.want {
			t.Fatalf("claim %d (%s): %t %v, want %t", i, c.local, ok, err, c.want)
		}
	}
}
