package store

import (
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"
)

// ErrCheckNotFound is returned when a check name does not exist.
var ErrCheckNotFound = errors.New("check not found")

// ErrCheckExists is returned when a check with the same name already exists.
var ErrCheckExists = errors.New("check already exists")

// MaxMarksPerCheck is how many marks a check always keeps. Older ones go only when they are below
// the evaluation watermark (at or before the last evaluated expected time, or already counted for
// it), so a mark that may still satisfy an expected time not evaluated yet is never pruned.
const MaxMarksPerCheck = 200

// MaxMarksHard bounds a check's marks whatever the watermark (a check whose watchdog never runs
// while something marks it every minute); the oldest go first.
const MaxMarksHard = 10000

// Evaluation results of an expected time (Check.EvaluatedResult).
const (
	EvalOK     = "ok"
	EvalMissed = "missed"
)

// Delivery states of a missed alert (Check.AlertState; "" = never missed).
const (
	AlertPending   = "pending"
	AlertDelivered = "delivered"
	AlertFailed    = "failed"
)

// Check is one row of the checks table (dead-man's switch): a named expectation that something
// silent when healthy leaves a mark before each expected time (a cron in local time) plus a grace.
// Rows never expire; they are edited in place.
type Check struct {
	ID           int64     `json:"id"`
	Name         string    `json:"name"`
	Cron         string    `json:"cron"`
	GraceMinutes int       `json:"grace_minutes"`
	Enabled      bool      `json:"enabled"`
	CreatedAt    time.Time `json:"created_at"`
	UpdatedAt    time.Time `json:"updated_at"`
	// ArmedAt is when the current cron and grace took effect (creation, or the last change of the
	// cron, the grace or enabling): expected times before it never alert.
	ArmedAt time.Time `json:"armed_at"`
	// LastMarkAt and LastMarkNote are the latest mark (nil when never marked).
	LastMarkAt   *time.Time `json:"last_mark_at,omitempty"`
	LastMarkNote string     `json:"last_mark_note,omitempty"`
	LastMarkJob  string     `json:"last_mark_job,omitempty"`
	// Revision counts the edits (check_set): the watchdog's claim holds only for the revision it read.
	Revision int64 `json:"revision"`
	// EvaluatedAt is the latest expected time the watchdog judged (the evaluation watermark: never
	// judged again), EvaluatedResult its outcome (EvalOK or EvalMissed). EvaluatedLocal is that time
	// on the local wall clock, "YYYY-MM-DD HH:MM", which a later evaluation's local minute must
	// exceed (see ClaimCheckEvaluation).
	EvaluatedAt     *time.Time `json:"evaluated_at,omitempty"`
	EvaluatedResult string     `json:"evaluated_result,omitempty"`
	EvaluatedLocal  string     `json:"-"`
	// CountedMarkID is the mark that satisfied the latest expected time judged ok (0 = none): each
	// mark counts for one expected time, so later ones only look at marks after it.
	CountedMarkID int64 `json:"-"`
	// LastMissedAt is the latest expected time judged missed (nil when never).
	LastMissedAt *time.Time `json:"last_missed_at,omitempty"`
	// LastMissedLocal is that time on the local wall clock.
	LastMissedLocal string `json:"-"`
	// AlertState is the delivery of LastMissedAt's alert: AlertPending (retried each tick with
	// backoff), AlertDelivered or AlertFailed (gave up); "" when never missed. AlertAttempts counts
	// the delivery attempts, AlertNextAt is when the next may start.
	AlertState    string    `json:"alert_state,omitempty"`
	AlertAttempts int       `json:"alert_attempts,omitempty"`
	AlertNextAt   time.Time `json:"-"`
	// LastAlertError is why the last delivery attempt failed ("" = none failed, or delivered).
	LastAlertError string `json:"last_alert_error,omitempty"`
}

// MarkRef is one mark's id and time.
type MarkRef struct {
	ID int64
	At time.Time
}

// Mark is one row of the check_marks table.
type Mark struct {
	At    time.Time `json:"at"`
	Note  string    `json:"note,omitempty"`
	JobID string    `json:"job_id,omitempty"`
}

const checksSchema = `
CREATE TABLE IF NOT EXISTS checks (
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
CREATE TABLE IF NOT EXISTS check_marks (
	id       INTEGER PRIMARY KEY AUTOINCREMENT,
	check_id INTEGER NOT NULL,
	at       INTEGER NOT NULL,
	note     TEXT NOT NULL DEFAULT '',
	job_id   TEXT NOT NULL DEFAULT ''
);
CREATE INDEX IF NOT EXISTS check_marks_check_at ON check_marks(check_id, at);
`

// checksMigrations add the checks columns introduced after the first checks schema, in order (run
// after checksSchema, tolerant like migrations). Never edit or reorder a row.
var checksMigrations = []string{
	`ALTER TABLE checks ADD COLUMN revision INTEGER NOT NULL DEFAULT 0`,
	`ALTER TABLE checks ADD COLUMN eval_at INTEGER`,
	`ALTER TABLE checks ADD COLUMN eval_local TEXT NOT NULL DEFAULT ''`,
	`ALTER TABLE checks ADD COLUMN eval_result TEXT NOT NULL DEFAULT ''`,
	`ALTER TABLE checks ADD COLUMN counted_mark_id INTEGER NOT NULL DEFAULT 0`,
	`ALTER TABLE checks ADD COLUMN alert_attempts INTEGER NOT NULL DEFAULT 0`,
	`ALTER TABLE checks ADD COLUMN alert_next_at INTEGER NOT NULL DEFAULT 0`,
	checksAlertStateAlter,
}

// checksAlertStateAlter adds the alert state; its backfill (checksAlertStateFill, in
// migrationBackfills) turns a missed time claimed before the column existed into the evaluation
// watermark and a finished delivery (it was attempted once).
const checksAlertStateAlter = `ALTER TABLE checks ADD COLUMN alert_state TEXT NOT NULL DEFAULT ''`

const checksAlertStateFill = `UPDATE checks SET eval_at = last_missed_at,
	eval_local = last_missed_local, eval_result = 'missed', alert_attempts = 1,
	alert_state = CASE WHEN last_alert_error = '' THEN 'delivered' ELSE 'failed' END
	WHERE last_missed_at IS NOT NULL AND eval_at IS NULL`

const checkCols = `id, name, cron, grace_minutes, enabled, created_at, updated_at, armed_at,
	last_mark_at, last_mark_note, last_mark_job, last_missed_at, last_missed_local, last_alert_error,
	revision, eval_at, eval_local, eval_result, counted_mark_id, alert_state, alert_attempts, alert_next_at`

func scanCheck(r scanner) (*Check, error) {
	var c Check
	var created, updated, armed int64
	var mark, missed, eval sql.NullInt64
	var next int64
	if err := r.Scan(&c.ID, &c.Name, &c.Cron, &c.GraceMinutes, &c.Enabled, &created, &updated, &armed,
		&mark, &c.LastMarkNote, &c.LastMarkJob, &missed, &c.LastMissedLocal, &c.LastAlertError,
		&c.Revision, &eval, &c.EvaluatedLocal, &c.EvaluatedResult, &c.CountedMarkID, &c.AlertState,
		&c.AlertAttempts, &next); err != nil {
		return nil, err
	}
	c.CreatedAt, c.UpdatedAt, c.ArmedAt = time.Unix(created, 0), time.Unix(updated, 0), time.Unix(armed, 0)
	c.LastMarkAt, c.LastMissedAt, c.EvaluatedAt = convTS(mark), convTS(missed), convTS(eval)
	c.AlertNextAt = time.Unix(next, 0)
	return &c, nil
}

// AddCheck inserts an enabled check armed at now (ErrCheckExists when the name is taken).
// Validation is the caller's.
func (s *Store) AddCheck(name, cron string, grace int, now time.Time) (*Check, error) {
	_, err := s.db.Exec(`INSERT INTO checks (name, cron, grace_minutes, created_at, updated_at, armed_at)
		VALUES (?, ?, ?, ?, ?, ?)`, name, cron, grace, now.Unix(), now.Unix(), now.Unix())
	if err != nil {
		if strings.Contains(err.Error(), "UNIQUE") {
			return nil, ErrCheckExists
		}
		return nil, fmt.Errorf("add check: %w", err)
	}
	return s.GetCheck(name)
}

// GetCheck returns one check (ErrCheckNotFound when there is none).
func (s *Store) GetCheck(name string) (*Check, error) {
	c, err := scanCheck(s.db.QueryRow(`SELECT `+checkCols+` FROM checks WHERE name = ?`, name))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrCheckNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("get check: %w", err)
	}
	return c, nil
}

// ListChecks returns every check (only the enabled ones with enabledOnly), oldest first.
func (s *Store) ListChecks(enabledOnly bool) ([]*Check, error) {
	q := `SELECT ` + checkCols + ` FROM checks ORDER BY id`
	if enabledOnly {
		q = `SELECT ` + checkCols + ` FROM checks WHERE enabled = 1 ORDER BY id`
	}
	rows, err := s.db.Query(q)
	if err != nil {
		return nil, fmt.Errorf("list checks: %w", err)
	}
	defer rows.Close()
	out := []*Check{}
	for rows.Next() {
		c, err := scanCheck(rows)
		if err != nil {
			return nil, fmt.Errorf("scan check: %w", err)
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// SetCheck edits a check in place: a non-empty cron, a non-zero grace, a non-nil enabled. A change
// of the cron or the grace, or enabling a disabled check, re-arms it at now (no alert for expected
// times before that). Every edit bumps the revision, so a watchdog that read the check before it
// cannot claim an expected time of the old configuration.
func (s *Store) SetCheck(name, cron string, grace int, enabled *bool, now time.Time) (*Check, error) {
	en := sql.NullBool{}
	if enabled != nil {
		en = sql.NullBool{Bool: *enabled, Valid: true}
	}
	res, err := s.db.Exec(`UPDATE checks SET
		armed_at = CASE WHEN (?1 <> '' AND ?1 <> cron) OR (?2 <> 0 AND ?2 <> grace_minutes) OR (?3 = 1 AND enabled = 0)
			THEN ?4 ELSE armed_at END,
		cron          = CASE WHEN ?1 = '' THEN cron ELSE ?1 END,
		grace_minutes = CASE WHEN ?2 = 0 THEN grace_minutes ELSE ?2 END,
		enabled       = COALESCE(?3, enabled),
		revision      = revision + 1,
		updated_at    = ?4
		WHERE name = ?5`, cron, grace, en, now.Unix(), name)
	if err != nil {
		return nil, fmt.Errorf("set check: %w", err)
	}
	if n, err := res.RowsAffected(); err != nil {
		return nil, fmt.Errorf("set check: %w", err)
	} else if n == 0 {
		return nil, ErrCheckNotFound
	}
	return s.GetCheck(name)
}

// RemoveCheck deletes a check and its marks (ErrCheckNotFound when there is none).
func (s *Store) RemoveCheck(name string) error {
	c, err := s.GetCheck(name)
	if err != nil {
		return err
	}
	if _, err := s.db.Exec(`DELETE FROM check_marks WHERE check_id = ?`, c.ID); err != nil {
		return fmt.Errorf("remove check marks: %w", err)
	}
	if _, err := s.db.Exec(`DELETE FROM checks WHERE id = ?`, c.ID); err != nil {
		return fmt.Errorf("remove check: %w", err)
	}
	return nil
}

// AddMark records a completion mark for the check (jobID "" = not from a job) and returns the
// check with its new last mark. It prunes the marks below the evaluation watermark beyond the
// latest MaxMarksPerCheck, and any beyond the latest MaxMarksHard.
func (s *Store) AddMark(name, note, jobID string, at time.Time) (*Check, error) {
	c, err := s.GetCheck(name)
	if err != nil {
		return nil, err
	}
	if _, err := s.db.Exec(`INSERT INTO check_marks (check_id, at, note, job_id) VALUES (?, ?, ?, ?)`,
		c.ID, at.Unix(), note, jobID); err != nil {
		return nil, fmt.Errorf("add mark: %w", err)
	}
	if _, err := s.db.Exec(`UPDATE checks SET last_mark_at = ?, last_mark_note = ?, last_mark_job = ?
		WHERE id = ? AND (last_mark_at IS NULL OR last_mark_at <= ?)`, at.Unix(), note, jobID, c.ID, at.Unix()); err != nil {
		return nil, fmt.Errorf("add mark: %w", err)
	}
	if _, err := s.db.Exec(`DELETE FROM check_marks WHERE check_id = ?1
		AND id NOT IN (SELECT id FROM check_marks WHERE check_id = ?1 ORDER BY at DESC, id DESC LIMIT ?2)
		AND (id NOT IN (SELECT id FROM check_marks WHERE check_id = ?1 ORDER BY at DESC, id DESC LIMIT ?3)
			OR id <= (SELECT counted_mark_id FROM checks WHERE id = ?1)
			OR at <= (SELECT COALESCE(eval_at, -1) FROM checks WHERE id = ?1))`,
		c.ID, MaxMarksPerCheck, MaxMarksHard); err != nil {
		return nil, fmt.Errorf("trim marks: %w", err)
	}
	return s.GetCheck(name)
}

// FirstMark is the earliest mark of the check in (after, until] (second resolution; a zero after
// means no lower bound) whose id is above afterID, nil when there is none.
func (s *Store) FirstMark(checkID, afterID int64, after, until time.Time) (*MarkRef, error) {
	lo := int64(-1 << 62)
	if !after.IsZero() {
		lo = after.Unix()
	}
	var id, at int64
	err := s.db.QueryRow(`SELECT id, at FROM check_marks WHERE check_id = ? AND id > ? AND at > ? AND at <= ?
		ORDER BY at, id LIMIT 1`, checkID, afterID, lo, until.Unix()).Scan(&id, &at)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("check marks: %w", err)
	}
	return &MarkRef{ID: id, At: time.Unix(at, 0)}, nil
}

// ClaimCheckEvaluation records the watchdog's judgement of expected (local is its wall-clock
// minute "YYYY-MM-DD HH:MM"): ok when mark is the mark that satisfied it (that mark then counts
// for no later expected time), missed when mark is nil (a pending alert, delivered by
// ClaimCheckAlertAttempt and RecordCheckAlert). It holds only when the check is enabled, still at
// revision (no check_set since the watchdog read it), and expected and local are strictly later
// than the last evaluation's (the schedules watermark, ClaimScheduleFiring): one atomic
// statement, so two daemons or a restarted one judge each expected time once, and the second pass
// of an hour repeated when DST ends is never judged again.
func (s *Store) ClaimCheckEvaluation(id, revision int64, expected time.Time, local string, mark *MarkRef) (bool, error) {
	var markID int64
	if mark != nil {
		markID = mark.ID
	}
	res, err := s.db.Exec(`UPDATE checks SET eval_at = ?1, eval_local = ?2,
		eval_result       = CASE WHEN ?3 > 0 THEN 'ok' ELSE 'missed' END,
		counted_mark_id   = CASE WHEN ?3 > 0 THEN ?3 ELSE counted_mark_id END,
		last_missed_at    = CASE WHEN ?3 > 0 THEN last_missed_at ELSE ?1 END,
		last_missed_local = CASE WHEN ?3 > 0 THEN last_missed_local ELSE ?2 END,
		alert_state       = CASE WHEN ?3 > 0 THEN alert_state ELSE 'pending' END,
		alert_attempts    = CASE WHEN ?3 > 0 THEN alert_attempts ELSE 0 END,
		alert_next_at     = CASE WHEN ?3 > 0 THEN alert_next_at ELSE 0 END,
		last_alert_error  = CASE WHEN ?3 > 0 THEN last_alert_error ELSE '' END
		WHERE id = ?4 AND enabled = 1 AND revision = ?5 AND (eval_at IS NULL OR eval_at < ?1) AND eval_local < ?2`,
		expected.Unix(), local, markID, id, revision)
	if err != nil {
		return false, fmt.Errorf("claim check: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("claim check: %w", err)
	}
	return n == 1, nil
}

// PendingCheckAlerts are the checks whose missed alert is pending and whose next attempt may start
// at now, oldest first.
func (s *Store) PendingCheckAlerts(now time.Time) ([]*Check, error) {
	rows, err := s.db.Query(`SELECT `+checkCols+` FROM checks WHERE alert_state = ? AND alert_next_at <= ? ORDER BY id`,
		AlertPending, now.Unix())
	if err != nil {
		return nil, fmt.Errorf("pending check alerts: %w", err)
	}
	defer rows.Close()
	out := []*Check{}
	for rows.Next() {
		c, err := scanCheck(rows)
		if err != nil {
			return nil, fmt.Errorf("scan check: %w", err)
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// ClaimCheckAlertAttempt takes delivery attempt attempts+1 of the pending alert for the missed
// expected time, which must still be at attempts tries; the next attempt may start at next (so an
// attempt cut short by a crash is retried then). One atomic statement: two daemons never make the
// same attempt.
func (s *Store) ClaimCheckAlertAttempt(id int64, expected time.Time, attempts int, next time.Time) (bool, error) {
	res, err := s.db.Exec(`UPDATE checks SET alert_attempts = alert_attempts + 1, alert_next_at = ?
		WHERE id = ? AND alert_state = ? AND last_missed_at = ? AND alert_attempts = ?`,
		next.Unix(), id, AlertPending, expected.Unix(), attempts)
	if err != nil {
		return false, fmt.Errorf("claim check alert: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("claim check alert: %w", err)
	}
	return n == 1, nil
}

// RecordCheckAlert stores the outcome of a delivery attempt of the pending alert for the missed
// expected time: errText "" = delivered; otherwise the error is kept and the alert stays pending,
// or becomes failed with final. A later missed time (a new pending alert) is never overwritten.
func (s *Store) RecordCheckAlert(id int64, expected time.Time, errText string, final bool) error {
	state := AlertPending
	switch {
	case errText == "":
		state = AlertDelivered
	case final:
		state = AlertFailed
	}
	if _, err := s.db.Exec(`UPDATE checks SET alert_state = ?, last_alert_error = ?
		WHERE id = ? AND last_missed_at = ? AND alert_state = ?`,
		state, errText, id, expected.Unix(), AlertPending); err != nil {
		return fmt.Errorf("record check alert: %w", err)
	}
	return nil
}
