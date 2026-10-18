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

// MaxMarksPerCheck bounds the marks kept per check (the oldest go first): the watchdog only looks
// at the window before the latest expected time.
const MaxMarksPerCheck = 200

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
	// LastMissedAt is the latest expected time alerted as missed (claimed; nil when never).
	LastMissedAt *time.Time `json:"last_missed_at,omitempty"`
	// LastMissedLocal is that time on the local wall clock, "YYYY-MM-DD HH:MM": the watermark a later
	// alert's local minute must exceed (see ClaimCheckMissed).
	LastMissedLocal string `json:"-"`
	// LastAlertError is why the last missed alert did not reach the session ("" = delivered).
	LastAlertError string `json:"last_alert_error,omitempty"`
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

const checkCols = `id, name, cron, grace_minutes, enabled, created_at, updated_at, armed_at,
	last_mark_at, last_mark_note, last_mark_job, last_missed_at, last_missed_local, last_alert_error`

func scanCheck(r scanner) (*Check, error) {
	var c Check
	var created, updated, armed int64
	var mark, missed sql.NullInt64
	if err := r.Scan(&c.ID, &c.Name, &c.Cron, &c.GraceMinutes, &c.Enabled, &created, &updated, &armed,
		&mark, &c.LastMarkNote, &c.LastMarkJob, &missed, &c.LastMissedLocal, &c.LastAlertError); err != nil {
		return nil, err
	}
	c.CreatedAt, c.UpdatedAt, c.ArmedAt = time.Unix(created, 0), time.Unix(updated, 0), time.Unix(armed, 0)
	c.LastMarkAt, c.LastMissedAt = convTS(mark), convTS(missed)
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
// times before that).
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

// AddMark records a completion mark for the check (jobID "" = not from a job), keeps the latest
// MaxMarksPerCheck marks and returns the check with its new last mark.
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
	if _, err := s.db.Exec(`DELETE FROM check_marks WHERE check_id = ?1 AND id NOT IN
		(SELECT id FROM check_marks WHERE check_id = ?1 ORDER BY at DESC, id DESC LIMIT ?2)`, c.ID, MaxMarksPerCheck); err != nil {
		return nil, fmt.Errorf("trim marks: %w", err)
	}
	return s.GetCheck(name)
}

// HasMark reports whether the check has a mark in (after, until] (second resolution); a zero
// after means no lower bound.
func (s *Store) HasMark(checkID int64, after, until time.Time) (bool, error) {
	lo := int64(-1 << 62)
	if !after.IsZero() {
		lo = after.Unix()
	}
	var n int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM check_marks WHERE check_id = ? AND at > ? AND at <= ?`,
		checkID, lo, until.Unix()).Scan(&n); err != nil {
		return false, fmt.Errorf("check marks: %w", err)
	}
	return n > 0, nil
}

// ClaimCheckMissed records expected (and local, its wall-clock minute "YYYY-MM-DD HH:MM") as the
// check's latest missed expected time, only when both are strictly later than the last one's (the
// schedules watermark, ClaimScheduleFiring): one atomic statement, so two daemons or a restarted
// one alert at most once per expected time, and the second pass of an hour repeated when DST ends
// never alerts again.
func (s *Store) ClaimCheckMissed(id int64, expected time.Time, local string) (bool, error) {
	res, err := s.db.Exec(`UPDATE checks SET last_missed_at = ?1, last_missed_local = ?2, last_alert_error = ''
		WHERE id = ?3 AND enabled = 1 AND (last_missed_at IS NULL OR last_missed_at < ?1) AND last_missed_local < ?2`,
		expected.Unix(), local, id)
	if err != nil {
		return false, fmt.Errorf("claim check: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("claim check: %w", err)
	}
	return n == 1, nil
}

// RecordCheckAlert stores why the alert for the missed expected time did not reach the session
// ("" = delivered; a later claim is never overwritten).
func (s *Store) RecordCheckAlert(id int64, expected time.Time, errText string) error {
	if _, err := s.db.Exec(`UPDATE checks SET last_alert_error = ? WHERE id = ? AND last_missed_at = ?`,
		errText, id, expected.Unix()); err != nil {
		return fmt.Errorf("record check alert: %w", err)
	}
	return nil
}
