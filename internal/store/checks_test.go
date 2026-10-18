package store

import (
	"database/sql"
	"errors"
	"path/filepath"
	"testing"
	"time"
)

// A stage-1 database gains the checks tables on Open; a check round-trips, a cron or grace change
// and enabling re-arm it (a pause or a no-op edit do not), marks are matched in (after, until] and
// trimmed to MaxMarksPerCheck, a missed expected time is claimed once (also by a second Open and
// for the same local minute), and remove deletes the check with its marks.
func TestChecksRoundTripClaimAndMarks(t *testing.T) {
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
	c, err := s.AddCheck("backup", "0 9 * * *", 60, t0)
	if err != nil || c.ID == 0 || !c.Enabled || !c.ArmedAt.Equal(t0) || c.LastMarkAt != nil || c.LastMissedAt != nil {
		t.Fatalf("added: %+v %v", c, err)
	}
	if _, err := s.AddCheck("backup", "@daily", 60, t0); !errors.Is(err, ErrCheckExists) {
		t.Fatalf("duplicate: %v", err)
	}
	t1 := t0.Add(time.Hour)
	off, on := false, true
	if c, err = s.SetCheck("backup", "0 9 * * *", 60, &off, t1); err != nil || c.Enabled || !c.ArmedAt.Equal(t0) {
		t.Fatalf("pause / same values must not re-arm: %+v %v", c, err)
	}
	if c, err = s.SetCheck("backup", "", 0, &on, t1); err != nil || !c.Enabled || !c.ArmedAt.Equal(t1) {
		t.Fatalf("enable re-arms: %+v %v", c, err)
	}
	t2 := t1.Add(time.Hour)
	if c, err = s.SetCheck("backup", "", 30, nil, t2); err != nil || c.GraceMinutes != 30 || c.Cron != "0 9 * * *" || !c.ArmedAt.Equal(t2) {
		t.Fatalf("grace re-arms: %+v %v", c, err)
	}
	if _, err := s.SetCheck("nope", "", 30, nil, t2); !errors.Is(err, ErrCheckNotFound) {
		t.Fatalf("set unknown: %v", err)
	}

	for i := 0; i < MaxMarksPerCheck+5; i++ {
		if _, err := s.AddMark("backup", "n", "", t0.Add(time.Duration(i)*time.Second)); err != nil {
			t.Fatal(err)
		}
	}
	var n int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM check_marks WHERE check_id = ?`, c.ID).Scan(&n); err != nil || n != MaxMarksPerCheck {
		t.Fatalf("marks kept %d, want %d (%v)", n, MaxMarksPerCheck, err)
	}
	last := t0.Add(time.Duration(MaxMarksPerCheck+4) * time.Second)
	// An older mark arriving later does not move the last mark back.
	if c, err = s.AddMark("backup", "old", "j1", t0); err != nil || !c.LastMarkAt.Equal(last) || c.LastMarkNote != "n" {
		t.Fatalf("last mark: %+v %v", c, err)
	}
	for _, w := range []struct {
		after, until time.Time
		want         bool
	}{
		{last, last.Add(time.Hour), false},         // after is exclusive
		{last.Add(-time.Second), last, true},       // until is inclusive
		{time.Time{}, t0.Add(-time.Second), false}, // nothing that early
		{last.Add(time.Second), last.Add(time.Hour), false},
	} {
		if got, err := s.HasMark(c.ID, w.after, w.until); err != nil || got != w.want {
			t.Fatalf("HasMark(%v, %v) = %t %v, want %t", w.after, w.until, got, err, w.want)
		}
	}

	e := time.Unix(1770001200, 0)
	if ok, err := s.ClaimCheckMissed(c.ID, e, "2026-02-02 03:00"); err != nil || !ok {
		t.Fatalf("first claim: %t %v", ok, err)
	}
	s2, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s2.Close()
	if ok, _ := s2.ClaimCheckMissed(c.ID, e, "2026-02-02 03:00"); ok {
		t.Fatal("second claim of the same expected time succeeded")
	}
	if ok, _ := s2.ClaimCheckMissed(c.ID, e.Add(time.Hour), "2026-02-02 03:00"); ok {
		t.Fatal("a later instant with the same local minute (DST fold) was claimed")
	}
	if err := s2.RecordCheckAlert(c.ID, e, "tmux failed"); err != nil {
		t.Fatal(err)
	}
	if c, _ = s2.GetCheck("backup"); c.LastMissedAt == nil || !c.LastMissedAt.Equal(e) || c.LastAlertError != "tmux failed" {
		t.Fatalf("missed: %+v", c)
	}
	if err := s.RemoveCheck("backup"); err != nil {
		t.Fatal(err)
	}
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM check_marks`).Scan(&n); err != nil || n != 0 {
		t.Fatalf("marks after remove: %d %v", n, err)
	}
	if err := s.RemoveCheck("backup"); !errors.Is(err, ErrCheckNotFound) {
		t.Fatalf("remove twice: %v", err)
	}
	if _, err := s.AddMark("backup", "", "", t0); !errors.Is(err, ErrCheckNotFound) {
		t.Fatalf("mark removed: %v", err)
	}
	_ = s.Close()
}
