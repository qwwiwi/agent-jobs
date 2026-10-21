package store

import (
	"database/sql"
	"errors"
	"path/filepath"
	"testing"
	"time"
)

// A stage-1 database gains the checks tables on Open; a check round-trips, a cron or grace change
// and enabling re-arm it (a pause or a no-op edit do not), every edit bumps the revision; a window
// stores at most MaxMarksPerWindow marks, and marks are pruned only when their owner is evaluated
// or they predate the arm time; consecutive expected times are judged once (also by a second Open
// and for the same local minute, never at a stale revision), each missed one is its own alert row
// whose attempts are claimed once each under a token; remove deletes the check, marks and alerts.
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
	countMarks := func() int {
		var n int
		if err := s.db.QueryRow(`SELECT COUNT(*) FROM check_marks WHERE check_id = ?`, c.ID).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	// Two windows of an hour each from t2 (the caller computes them; here fixed).
	w1 := MarkWindow{Lo: t2, Hi: t2.Add(time.Hour - time.Second), OK: true}
	w2 := MarkWindow{Lo: t2.Add(time.Hour), Hi: t2.Add(2*time.Hour - time.Second), OK: true}
	win := func(at time.Time) func(*Check) (MarkWindow, error) {
		return func(*Check) (MarkWindow, error) {
			if at.Before(w2.Lo) {
				return w1, nil
			}
			return w2, nil
		}
	}
	for i := 0; i < MaxMarksPerWindow+5; i++ {
		at := t2.Add(time.Duration(i) * time.Second)
		got, err := s.AddMark("backup", "n", "", at, win(at))
		want := MarkStored
		if i >= MaxMarksPerWindow {
			want = MarkNotStored
		}
		if err != nil || got.MarkResult != want || !got.LastMarkAt.Equal(at) {
			t.Fatalf("mark %d: %+v %v, want %q", i, got, err, want)
		}
	}
	if n := countMarks(); n != MaxMarksPerWindow {
		t.Fatalf("marks stored %d, want %d", n, MaxMarksPerWindow)
	}
	at2 := w2.Lo.Add(time.Minute)
	if got, err := s.AddMark("backup", "w2", "", at2, win(at2)); err != nil || got.MarkResult != MarkStored {
		t.Fatalf("a mark of another window: %+v %v", got, err)
	}
	// An older mark arriving later does not move the last mark back; a mark without owner is not stored.
	if c, err = s.AddMark("backup", "old", "j1", t0, func(*Check) (MarkWindow, error) { return MarkWindow{}, nil }); err != nil ||
		!c.LastMarkAt.Equal(at2) || c.LastMarkUntrustedNote != "w2" || c.MarkResult != MarkNotStored {
		t.Fatalf("last mark: %+v %v", c, err)
	}
	if got, err := s.MarkTimes(c.ID, w2.Lo, w2.Hi); err != nil || len(got) != 1 || !got[0].Equal(at2) {
		t.Fatalf("mark times: %v %v", got, err)
	}

	// A judgement at a stale revision is refused; ok/missed outcomes move the watermark to the last.
	e1, e2 := w1.Hi.Add(-30*time.Minute), w2.Hi.Add(-30*time.Minute) // owners with grace 30
	outs := []EvalOutcome{{Expected: e1, Local: "2026-02-02 04:30", OK: true}, {Expected: e2, Local: "2026-02-02 05:30"}}
	if ok, err := s.ClaimCheckEvaluations(c.ID, c.Revision-1, outs); err != nil || ok {
		t.Fatalf("stale revision claimed: %t %v", ok, err)
	}
	if ok, err := s.ClaimCheckEvaluations(c.ID, c.Revision, outs[:1]); err != nil || !ok {
		t.Fatalf("ok claim: %t %v", ok, err)
	}
	if c, _ = s.GetCheck("backup"); c.EvaluatedResult != EvalOK || !c.EvaluatedAt.Equal(e1) || c.LastMissedAt != nil || c.AlertState != "" {
		t.Fatalf("judged ok: %+v", c)
	}
	// The marks owned by e1 (at most e1+grace) are pruned; the one of e2 stays.
	if n := countMarks(); n != 1 {
		t.Fatalf("marks after the watermark prune %d, want 1", n)
	}
	s2, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s2.Close()
	if ok, _ := s2.ClaimCheckEvaluations(c.ID, c.Revision, outs); ok {
		t.Fatal("an expected time was judged twice")
	}
	if ok, _ := s2.ClaimCheckEvaluations(c.ID, c.Revision, []EvalOutcome{{Expected: e2, Local: "2026-02-02 04:30"}}); ok {
		t.Fatal("a later instant with the same local minute (DST fold) was claimed")
	}
	if ok, err := s2.ClaimCheckEvaluations(c.ID, c.Revision, outs[1:]); err != nil || !ok {
		t.Fatalf("missed claim: %t %v", ok, err)
	}
	if c, _ = s2.GetCheck("backup"); c.AlertState != AlertPending || c.AlertAttempts != 0 || !c.LastMissedAt.Equal(e2) ||
		c.AlertsPending != 1 || c.EvaluatedResult != EvalMissed {
		t.Fatalf("pending: %+v", c)
	}
	due, err := s2.DueCheckAlerts(e2)
	if err != nil || len(due) != 1 || due[0].Name != "backup" || due[0].Grace != 30 || !due[0].Expected.Equal(e2) {
		t.Fatalf("due: %+v %v", due, err)
	}
	a := due[0]
	// Attempts are claimed once each under a token; a failure stays pending, the last one fails.
	if ok, _ := s2.ClaimCheckAlertAttempt(a.ID, 0, "t1", e2, e2.Add(time.Minute)); !ok {
		t.Fatal("first attempt not claimed")
	}
	if ok, _ := s.ClaimCheckAlertAttempt(a.ID, 0, "t2", e2, e2.Add(time.Minute)); ok {
		t.Fatal("the same attempt claimed twice")
	}
	if p, _ := s2.DueCheckAlerts(e2); len(p) != 0 {
		t.Fatalf("due before its backoff: %+v", p)
	}
	if rows, _ := s2.TokenCheckAlerts("t1"); len(rows) != 1 || rows[0].Attempts != 1 {
		t.Fatalf("token rows: %+v", rows)
	}
	if err := s2.FailCheckAlertAttempt("t1", "tmux failed", 2); err != nil {
		t.Fatal(err)
	}
	if c, _ = s2.GetCheck("backup"); c.AlertState != AlertPending || c.AlertAttempts != 1 || c.LastAlertError != "tmux failed" {
		t.Fatalf("after a failure: %+v", c)
	}
	if ok, _ := s2.ClaimCheckAlertAttempt(a.ID, 1, "t3", e2.Add(time.Minute), e2.Add(3*time.Minute)); !ok {
		t.Fatal("second attempt not claimed")
	}
	if err := s2.FailCheckAlertAttempt("t1", "stale", 2); err != nil { // a stale token changes nothing
		t.Fatal(err)
	}
	if err := s2.FailCheckAlertAttempt("t3", "tmux failed", 2); err != nil {
		t.Fatal(err)
	}
	if c, _ = s2.GetCheck("backup"); c.AlertState != AlertFailed || c.AlertsFailed != 1 || c.AlertsPending != 0 {
		t.Fatalf("after the final failure: %+v", c)
	}
	if err := s2.DeliverCheckAlerts([]int64{a.ID}); err != nil {
		t.Fatal(err)
	}
	if c, _ = s2.GetCheck("backup"); c.AlertState != AlertFailed {
		t.Fatalf("a failed alert was overwritten: %+v", c)
	}
	// An attempt cut short at the last try is failed by FailCheckAlert (only at that attempt count).
	if ok, err := s2.ClaimCheckEvaluations(c.ID, c.Revision, []EvalOutcome{{Expected: e2.Add(24 * time.Hour), Local: "2026-02-03 05:30"}}); err != nil || !ok {
		t.Fatalf("third claim: %t %v", ok, err)
	}
	due, _ = s2.DueCheckAlerts(e2.Add(24 * time.Hour))
	if len(due) != 1 {
		t.Fatalf("due: %+v", due)
	}
	if err := s2.FailCheckAlert(due[0].ID, 3, "x"); err != nil { // wrong attempt count: no-op
		t.Fatal(err)
	}
	if c, _ = s2.GetCheck("backup"); c.AlertState != AlertPending || c.AlertsFailed != 1 {
		t.Fatalf("fail at a wrong count: %+v", c)
	}
	if err := s2.FailCheckAlert(due[0].ID, 0, "delivery interrupted"); err != nil {
		t.Fatal(err)
	}
	if c, _ = s2.GetCheck("backup"); c.AlertState != AlertFailed || c.AlertsFailed != 2 || c.LastAlertError != "delivery interrupted" {
		t.Fatalf("interrupted: %+v", c)
	}
	// check_set acknowledges failed alerts.
	if c, _ = s2.SetCheck("backup", "", 0, &on, t2); c.AlertsFailed != 0 || !c.AlertAcknowledged || c.AlertState != AlertFailed {
		t.Fatalf("acknowledged: %+v", c)
	}
	var n int
	if err := s.RemoveCheck("backup"); err != nil {
		t.Fatal(err)
	}
	if err := s.db.QueryRow(`SELECT (SELECT COUNT(*) FROM check_marks) + (SELECT COUNT(*) FROM check_alerts)`).Scan(&n); err != nil || n != 0 {
		t.Fatalf("marks and alerts after remove: %d %v", n, err)
	}
	if err := s.RemoveCheck("backup"); !errors.Is(err, ErrCheckNotFound) {
		t.Fatalf("remove twice: %v", err)
	}
	if _, err := s.AddMark("backup", "", "", t0, win(t0)); !errors.Is(err, ErrCheckNotFound) {
		t.Fatalf("mark removed: %v", err)
	}
	_ = s.Close()
}

// Beyond MaxPendingAlerts pending rows the oldest are merged into the newest row's counter, whose
// attempts are not reset; delivered history is trimmed to MaxDoneAlerts.
func TestCheckAlertsMergedBeyondCap(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "jobs.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	t0 := time.Unix(1770000000, 0)
	c, err := s.AddCheck("dense", "* * * * *", 1, t0)
	if err != nil {
		t.Fatal(err)
	}
	outs := func(from, n int) []EvalOutcome {
		var o []EvalOutcome
		for i := from; i < from+n; i++ {
			e := t0.Add(time.Duration(i) * time.Minute)
			o = append(o, EvalOutcome{Expected: e, Local: e.UTC().Format("2006-01-02 15:04")})
		}
		return o
	}
	if ok, err := s.ClaimCheckEvaluations(c.ID, 0, outs(1, MaxPendingAlerts)); err != nil || !ok {
		t.Fatal(ok, err)
	}
	all, _ := s.CheckAlerts(c.ID)
	oldest := all[0]
	if ok, _ := s.ClaimCheckAlertAttempt(oldest.ID, 0, "tok", t0.Add(time.Hour), t0.Add(time.Hour)); !ok {
		t.Fatal("claim")
	}
	if ok, err := s.ClaimCheckEvaluations(c.ID, 0, outs(1+MaxPendingAlerts, 10)); err != nil || !ok {
		t.Fatal(ok, err)
	}
	all, _ = s.CheckAlerts(c.ID)
	if len(all) != MaxPendingAlerts {
		t.Fatalf("rows %d, want %d", len(all), MaxPendingAlerts)
	}
	got, _ := s.GetCheck("dense")
	if got.AlertsPending != MaxPendingAlerts+10 || got.AlertMerged != 10 {
		t.Fatalf("counts: %+v", got)
	}
	for _, a := range all {
		if a.ID == oldest.ID {
			t.Fatal("the claimed row survived although older than the cap")
		}
	}
	// The newest row keeps its own attempts when older ones are merged into it.
	last := all[len(all)-1]
	if ok, _ := s.ClaimCheckAlertAttempt(last.ID, 0, "t2", t0.Add(2*time.Hour), t0.Add(2*time.Hour)); !ok {
		t.Fatal("claim newest")
	}
	if ok, err := s.ClaimCheckEvaluations(c.ID, 0, outs(11+MaxPendingAlerts, 1)); err != nil || !ok {
		t.Fatal(ok, err)
	}
	all, _ = s.CheckAlerts(c.ID)
	for _, a := range all {
		if a.ID == last.ID && a.Attempts != 1 {
			t.Fatalf("attempts reset: %+v", a)
		}
	}
	ids := make([]int64, 0, len(all))
	for _, a := range all {
		ids = append(ids, a.ID)
	}
	if err := s.DeliverCheckAlerts(ids); err != nil {
		t.Fatal(err)
	}
	if ok, err := s.ClaimCheckEvaluations(c.ID, 0, outs(12+MaxPendingAlerts, 5)); err != nil || !ok {
		t.Fatal(ok, err)
	}
	if all, _ = s.CheckAlerts(c.ID); len(all) != MaxDoneAlerts+5 {
		t.Fatalf("rows after trimming %d, want %d", len(all), MaxDoneAlerts+5)
	}
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
// the evaluation watermark (never re-judged) and an alert row: pending when no error was stored
// (the old code claimed before typing, so an empty error proves no delivery), failed otherwise;
// Open twice is a no-op (one alert row each).
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
			a.AlertState != AlertPending || a.Revision != 0 {
			t.Fatalf("a: %+v", a)
		}
		if b == nil || b.AlertState != AlertFailed || b.LastAlertError != "tmux failed" {
			t.Fatalf("b: %+v", b)
		}
		if c == nil || c.EvaluatedAt != nil || c.AlertState != "" {
			t.Fatalf("c: %+v", c)
		}
		if rows, _ := s.CheckAlerts(a.ID); len(rows) != 1 || rows[0].State != AlertPending || rows[0].Attempts != 0 {
			t.Fatalf("a's alert rows: %+v", rows)
		}
		if due, _ := s.DueCheckAlerts(time.Unix(1770000000, 0)); len(due) != 1 || due[0].Name != "a" {
			t.Fatalf("due after migration: %+v", due)
		}
		if ok, _ := s.ClaimCheckEvaluations(a.ID, 0, []EvalOutcome{{Expected: *a.EvaluatedAt, Local: a.EvaluatedLocal}}); ok {
			t.Fatal("a migrated missed time was judged again")
		}
		_ = s.Close()
	}
}

// checksSchemaLoop1 is the checks table of fix loop 1: the alert state on the check row.
const checksSchemaLoop1 = checksSchemaV1 + `
ALTER TABLE checks ADD COLUMN revision INTEGER NOT NULL DEFAULT 0;
ALTER TABLE checks ADD COLUMN eval_at INTEGER;
ALTER TABLE checks ADD COLUMN eval_local TEXT NOT NULL DEFAULT '';
ALTER TABLE checks ADD COLUMN eval_result TEXT NOT NULL DEFAULT '';
ALTER TABLE checks ADD COLUMN counted_mark_id INTEGER NOT NULL DEFAULT 0;
ALTER TABLE checks ADD COLUMN alert_attempts INTEGER NOT NULL DEFAULT 0;
ALTER TABLE checks ADD COLUMN alert_next_at INTEGER NOT NULL DEFAULT 0;
ALTER TABLE checks ADD COLUMN alert_state TEXT NOT NULL DEFAULT '';
UPDATE checks SET eval_at = last_missed_at, eval_local = last_missed_local, eval_result = 'missed',
	alert_state = 'pending', alert_attempts = 2, alert_next_at = 1770000300, last_alert_error = 'tmux failed'
	WHERE name = 'a';
`

// A fix-loop-1 database keeps each check's alert: it moves into check_alerts once, attempts and
// backoff included.
func TestChecksMigrationFromLoop1(t *testing.T) {
	path := filepath.Join(t.TempDir(), "jobs.db")
	raw, err := sql.Open("sqlite", "file:"+path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := raw.Exec(stage1Schema + checksSchemaLoop1); err != nil {
		t.Fatal(err)
	}
	_ = raw.Close()
	for i := 0; i < 2; i++ {
		s, err := Open(path)
		if err != nil {
			t.Fatal(err)
		}
		a, _ := s.GetCheck("a")
		rows, _ := s.CheckAlerts(a.ID)
		if len(rows) != 1 || rows[0].State != AlertPending || rows[0].Attempts != 2 || rows[0].Error != "tmux failed" {
			t.Fatalf("a's alert rows: %+v", rows)
		}
		if due, _ := s.DueCheckAlerts(time.Unix(1770000299, 0)); len(due) != 0 {
			t.Fatalf("due before the stored backoff: %+v", due)
		}
		if b, _ := s.GetCheck("b"); b.AlertState != "" { // no alert state in this fixture: nothing to move
			t.Fatalf("b: %+v", b)
		}
		_ = s.Close()
	}
}

// The appended migration keeps legacy policies open and backfills owner provenance only once.
func TestChecksMarkerMigration(t *testing.T) {
	path := filepath.Join(t.TempDir(), "jobs.db")
	db, err := sql.Open("sqlite", "file:"+path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(stage1Schema + checksSchemaV1 + checksSchema); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`UPDATE checks SET last_mark_at = created_at, last_mark_job = '' WHERE name = 'a';
 INSERT INTO check_marks (check_id, at, note, job_id) SELECT id, created_at, 'old', '' FROM checks WHERE name = 'a'`); err != nil {
		t.Fatal(err)
	}
	_ = db.Close()
	for i := 0; i < 2; i++ {
		s, err := Open(path)
		if err != nil {
			t.Fatal(err)
		}
		c, err := s.GetCheck("a")
		if err != nil || c.LastMarkJob != "owner" || len(c.AllowedScheduleIDs) != i {
			t.Fatalf("migration pass %d: %+v %v", i, c, err)
		}
		var marker string
		if err := s.db.QueryRow(`SELECT job_id FROM check_marks WHERE check_id = ?`, c.ID).Scan(&marker); err != nil || marker != "owner" {
			t.Fatalf("mark provenance: %q %v", marker, err)
		}
		if i == 0 {
			if _, err := s.SetCheck("a", "", 0, nil, time.Now(), []int64{7}); err != nil {
				t.Fatal(err)
			}
		}
		_ = s.Close()
	}
}
