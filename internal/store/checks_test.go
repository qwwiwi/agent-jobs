package store

import (
	"database/sql"
	"errors"
	"path/filepath"
	"testing"
	"time"
)

// A stage-1 database gains the checks tables on Open; a check round-trips, a cron or grace change
// and enabling re-arm it (a pause or a no-op edit do not), every edit bumps the revision, marks are
// matched in (after, until] above a mark id and pruned only below the evaluation watermark, an
// expected time is judged once (also by a second Open and for the same local minute, never at a
// stale revision), a missed one's alert attempts are claimed once each, and remove deletes the
// check with its marks.
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

	if c.Revision != 3 {
		t.Fatalf("revision %d after three edits", c.Revision)
	}

	// Marks above the evaluation watermark are all kept (nothing judged yet).
	for i := 0; i < MaxMarksPerCheck+5; i++ {
		if _, err := s.AddMark("backup", "n", "", t0.Add(time.Duration(i)*time.Second)); err != nil {
			t.Fatal(err)
		}
	}
	countMarks := func() int {
		var n int
		if err := s.db.QueryRow(`SELECT COUNT(*) FROM check_marks WHERE check_id = ?`, c.ID).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	if n := countMarks(); n != MaxMarksPerCheck+5 {
		t.Fatalf("marks kept %d, want all %d", n, MaxMarksPerCheck+5)
	}
	last := t0.Add(time.Duration(MaxMarksPerCheck+4) * time.Second)
	// An older mark arriving later does not move the last mark back.
	if c, err = s.AddMark("backup", "old", "j1", t0); err != nil || !c.LastMarkAt.Equal(last) || c.LastMarkNote != "n" {
		t.Fatalf("last mark: %+v %v", c, err)
	}
	first, err := s.FirstMark(c.ID, 0, time.Time{}, last)
	if err != nil || first == nil || !first.At.Equal(t0) {
		t.Fatalf("first mark: %+v %v", first, err)
	}
	for _, w := range []struct {
		afterID      int64
		after, until time.Time
		want         time.Time // zero = none
	}{
		{0, last, last.Add(time.Hour), time.Time{}},               // after is exclusive
		{0, last.Add(-time.Second), last, last},                   // until is inclusive
		{0, time.Time{}, t0.Add(-time.Second), time.Time{}},       // nothing that early
		{first.ID, t0.Add(-time.Second), t0.Add(time.Second), t0}, // the later mark at t0 ("old")
	} {
		got, err := s.FirstMark(c.ID, w.afterID, w.after, w.until)
		if err != nil || (got == nil) != w.want.IsZero() || (got != nil && !got.At.Equal(w.want)) {
			t.Fatalf("FirstMark(%d, %v, %v) = %+v %v, want %v", w.afterID, w.after, w.until, got, err, w.want)
		}
	}

	if got, _ := s.FirstMark(c.ID, first.ID, time.Time{}, t0); got == nil || got.ID == first.ID {
		t.Fatalf("FirstMark above id %d returned %+v", first.ID, got)
	}

	// A judgement at a stale revision is refused; ok counts its mark, missed makes a pending alert.
	e := last.Add(time.Hour)
	if ok, err := s.ClaimCheckEvaluation(c.ID, c.Revision-1, e, "2026-02-02 03:00", nil); err != nil || ok {
		t.Fatalf("stale revision claimed: %t %v", ok, err)
	}
	if ok, err := s.ClaimCheckEvaluation(c.ID, c.Revision, e, "2026-02-02 03:00", first); err != nil || !ok {
		t.Fatalf("ok claim: %t %v", ok, err)
	}
	if c, _ = s.GetCheck("backup"); c.EvaluatedResult != EvalOK || c.CountedMarkID != first.ID || c.LastMissedAt != nil || c.AlertState != "" {
		t.Fatalf("judged ok: %+v", c)
	}
	// Marks at or before the judged time are pruned beyond MaxMarksPerCheck.
	if _, err := s.AddMark("backup", "new", "", e.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	if n := countMarks(); n != MaxMarksPerCheck {
		t.Fatalf("marks after the watermark prune %d, want %d", n, MaxMarksPerCheck)
	}
	e2 := e.Add(time.Hour)
	if ok, err := s.ClaimCheckEvaluation(c.ID, c.Revision, e2, "2026-02-02 04:00", nil); err != nil || !ok {
		t.Fatalf("missed claim: %t %v", ok, err)
	}
	s2, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s2.Close()
	if ok, _ := s2.ClaimCheckEvaluation(c.ID, c.Revision, e2, "2026-02-02 04:00", nil); ok {
		t.Fatal("second claim of the same expected time succeeded")
	}
	if ok, _ := s2.ClaimCheckEvaluation(c.ID, c.Revision, e2.Add(time.Hour), "2026-02-02 04:00", nil); ok {
		t.Fatal("a later instant with the same local minute (DST fold) was claimed")
	}
	if c, _ = s2.GetCheck("backup"); c.AlertState != AlertPending || c.AlertAttempts != 0 || !c.LastMissedAt.Equal(e2) || c.CountedMarkID != first.ID {
		t.Fatalf("pending: %+v", c)
	}
	if p, err := s2.PendingCheckAlerts(e2); err != nil || len(p) != 1 {
		t.Fatalf("pending list: %+v %v", p, err)
	}
	// Attempts are claimed once each; a failure stays pending, then a final one fails.
	if ok, _ := s2.ClaimCheckAlertAttempt(c.ID, e2, 0, e2.Add(time.Minute)); !ok {
		t.Fatal("first attempt not claimed")
	}
	if ok, _ := s.ClaimCheckAlertAttempt(c.ID, e2, 0, e2.Add(time.Minute)); ok {
		t.Fatal("the same attempt claimed twice")
	}
	if p, _ := s2.PendingCheckAlerts(e2); len(p) != 0 {
		t.Fatalf("pending before its backoff: %+v", p)
	}
	if err := s2.RecordCheckAlert(c.ID, e2, "tmux failed", false); err != nil {
		t.Fatal(err)
	}
	if c, _ = s2.GetCheck("backup"); c.AlertState != AlertPending || c.AlertAttempts != 1 || c.LastAlertError != "tmux failed" {
		t.Fatalf("after a failure: %+v", c)
	}
	if err := s2.RecordCheckAlert(c.ID, e2, "tmux failed", true); err != nil {
		t.Fatal(err)
	}
	if c, _ = s2.GetCheck("backup"); c.AlertState != AlertFailed {
		t.Fatalf("after the final failure: %+v", c)
	}
	if err := s2.RecordCheckAlert(c.ID, e2, "", false); err != nil {
		t.Fatal(err)
	}
	if c, _ = s2.GetCheck("backup"); c.AlertState != AlertFailed {
		t.Fatalf("a failed alert was overwritten: %+v", c)
	}
	var n int
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

// checksSchemaV1 is the checks table before the evaluation watermark, the revision and the alert
// state (feature/deadman before fix loop 1).
const checksSchemaV1 = `
CREATE TABLE checks (
	id                INTEGER PRIMARY KEY AUTOINCREMENT,
	name              TEXT NOT NULL UNIQUE,
	cron              TEXT NOT NULL,
	grace_minutes     INTEGER NOT NULL,
	enabled           INTEGER NOT NULL DEFAULT 1,
	created_at        INTEGER NOT NULL,
	updated_at        INTEGER NOT NULL,
	armed_at          INTEGER NOT NULL,
	last_mark_at      INTEGER,
	last_mark_note    TEXT NOT NULL DEFAULT '',
	last_mark_job     TEXT NOT NULL DEFAULT '',
	last_missed_at    INTEGER,
	last_missed_local TEXT NOT NULL DEFAULT '',
	last_alert_error  TEXT NOT NULL DEFAULT ''
);
INSERT INTO checks (name, cron, grace_minutes, created_at, updated_at, armed_at, last_missed_at, last_missed_local, last_alert_error)
	VALUES ('a', '0 9 * * *', 60, 1, 1, 1, 1770000000, '2026-02-02 03:40', ''),
	       ('b', '0 9 * * *', 60, 1, 1, 1, 1770000000, '2026-02-02 03:40', 'tmux failed'),
	       ('c', '0 9 * * *', 60, 1, 1, 1, NULL, '', '');
`

// An existing checks table gains the new columns on Open; a missed time claimed before them becomes
// the evaluation watermark with a finished delivery (never re-alerted), and Open twice is a no-op.
func TestChecksMigrationAdditive(t *testing.T) {
	path := filepath.Join(t.TempDir(), "jobs.db")
	raw, err := sql.Open("sqlite", "file:"+path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := raw.Exec(stage1Schema + checksSchemaV1); err != nil {
		t.Fatal(err)
	}
	_ = raw.Close()
	for i := 0; i < 2; i++ {
		s, err := Open(path)
		if err != nil {
			t.Fatal(err)
		}
		a, _ := s.GetCheck("a")
		b, _ := s.GetCheck("b")
		c, _ := s.GetCheck("c")
		if a == nil || a.EvaluatedAt == nil || a.EvaluatedResult != EvalMissed || a.EvaluatedLocal != "2026-02-02 03:40" ||
			a.AlertState != AlertDelivered || a.Revision != 0 {
			t.Fatalf("a: %+v", a)
		}
		if b == nil || b.AlertState != AlertFailed || b.LastAlertError != "tmux failed" {
			t.Fatalf("b: %+v", b)
		}
		if c == nil || c.EvaluatedAt != nil || c.AlertState != "" {
			t.Fatalf("c: %+v", c)
		}
		if ok, _ := s.ClaimCheckEvaluation(a.ID, 0, *a.EvaluatedAt, a.EvaluatedLocal, nil); ok {
			t.Fatal("a migrated missed time was judged again")
		}
		_ = s.Close()
	}
}
