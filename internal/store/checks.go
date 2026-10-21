package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"time"
)

// ErrCheckNotFound is returned when a check name does not exist.
var ErrCheckNotFound = errors.New("check not found")

// ErrCheckExists is returned when a check with the same name already exists.
var ErrCheckExists = errors.New("check already exists")

// ErrCheckMarkForbidden means the job is not a permitted marker of this check.
var ErrCheckMarkForbidden = errors.New("job is not permitted to mark check")

// MaxMarksPerWindow is how many marks one owner window (the marks of one expected time) stores;
// later marks of that window are accepted but not stored (one is enough to satisfy it).
const MaxMarksPerWindow = 20

// MaxPendingAlerts bounds a check's pending alert rows; beyond it the oldest are merged into the
// newest pending row's counter (its attempts untouched).
const MaxPendingAlerts = 50

// MaxFailedAlerts bounds a check's failed alert rows not acknowledged yet; beyond it the oldest
// are merged into the newest failed row's counter.
const MaxFailedAlerts = 50

// MaxDoneAlerts is how many delivered or acknowledged alert rows a check keeps (history).
const MaxDoneAlerts = 50

// AddMark's results (Check.MarkResult).
const (
	MarkStored    = "stored"
	MarkNotStored = "ok, already marked"
)

// Evaluation results of an expected time (Check.EvaluatedResult).
const (
	EvalOK     = "ok"
	EvalMissed = "missed"
)

// Delivery states of a missed alert (CheckAlert.State, Check.AlertState; "" = never missed).
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
	// cron, the grace or enabling): expected times before it are never judged and marks before it
	// never count (the configuration epoch).
	ArmedAt time.Time `json:"armed_at"`
	// AllowedScheduleIDs restricts job markers to these schedules; empty permits every job.
	AllowedScheduleIDs []int64 `json:"allowed_schedule_ids"`
	// LastMarkAt and LastMarkUntrustedNote are the latest mark (nil when never marked), stored or not.
	LastMarkAt            *time.Time `json:"last_mark_at,omitempty"`
	LastMarkUntrustedNote string     `json:"last_mark_untrusted_note,omitempty"`
	LastMarkJob           string     `json:"last_mark_job,omitempty"`
	// Revision counts the edits (check_set): the watchdog's claim holds only for the revision it read.
	Revision int64 `json:"revision"`
	// EvaluatedAt is the latest expected time the watchdog judged (the evaluation watermark: every
	// expected time up to it is judged, never again), EvaluatedResult its outcome (EvalOK or
	// EvalMissed). EvaluatedLocal is that time on the local wall clock, "YYYY-MM-DD HH:MM", which a
	// later evaluation's local minute must exceed (see ClaimCheckEvaluations).
	EvaluatedAt     *time.Time `json:"evaluated_at,omitempty"`
	EvaluatedResult string     `json:"evaluated_result,omitempty"`
	EvaluatedLocal  string     `json:"-"`
	// LastMissedAt is the expected time of the newest alert row (nil when never missed), AlertState
	// its delivery (AlertPending, AlertDelivered or AlertFailed), AlertAttempts its delivery
	// attempts, LastAlertError why its last attempt failed, AlertMerged how many older missed times
	// were merged into it, AlertAcknowledged whether check_set acknowledged it (failed only).
	LastMissedAt      *time.Time `json:"last_missed_at,omitempty"`
	AlertState        string     `json:"alert_state,omitempty"`
	AlertAttempts     int        `json:"alert_attempts,omitempty"`
	LastAlertError    string     `json:"last_alert_error,omitempty"`
	AlertMerged       int        `json:"alert_merged,omitempty"`
	AlertAcknowledged bool       `json:"alert_acknowledged,omitempty"`
	// AlertsPending and AlertsFailed count the missed times whose alert is pending, and failed and
	// not acknowledged (merged ones included), over every alert row of the check.
	AlertsPending int `json:"alerts_pending,omitempty"`
	AlertsFailed  int `json:"alerts_failed,omitempty"`
	// MarkResult is set by AddMark only: MarkStored, or MarkNotStored when the mark was accepted but
	// not stored (its window already holds MaxMarksPerWindow marks, or it has no owner).
	MarkResult string `json:"mark,omitempty"`
}

// CheckAlert is one row of the check_alerts table: the alert of one missed expected time (plus
// Merged older ones). Rows are never replaced by a later miss; each keeps its own attempts.
type CheckAlert struct {
	ID       int64
	CheckID  int64
	Name     string
	Grace    int
	Expected time.Time
	State    string
	Attempts int
	Token    string
	Error    string
	Merged   int
	// Latest mark context is joined when reading alerts, for the session notification.
	LastMarkAt            *time.Time
	LastMarkJob           string
	LastMarkUntrustedNote string
}

// EvalOutcome is the watchdog's judgement of one expected time (Local its wall-clock minute).
type EvalOutcome struct {
	Expected time.Time
	Local    string
	OK       bool
}

// MarkWindow is the owner window of a mark, [Lo, Hi] in unix seconds: the marks of one expected
// time. OK false = the mark has no owner (before the arm time): it is not stored.
type MarkWindow struct {
	Lo, Hi time.Time
	OK     bool
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
CREATE TABLE IF NOT EXISTS check_alerts (
	id             INTEGER PRIMARY KEY AUTOINCREMENT,
	check_id       INTEGER NOT NULL,
	expected       INTEGER NOT NULL,
	expected_local TEXT NOT NULL DEFAULT '',
	state          TEXT NOT NULL,
	attempts       INTEGER NOT NULL DEFAULT 0,
	next_at        INTEGER NOT NULL DEFAULT 0,
	token          TEXT NOT NULL DEFAULT '',
	error          TEXT NOT NULL DEFAULT '',
	merged         INTEGER NOT NULL DEFAULT 0,
	acked          INTEGER NOT NULL DEFAULT 0
);
CREATE INDEX IF NOT EXISTS check_alerts_check_state ON check_alerts(check_id, state);
`

// checksMigrations add the checks columns introduced after the first checks schema, in order (run
// after checksSchema, tolerant like migrations). Never edit or reorder a row. The per-check alert
// columns (counted_mark_id, alert_*, last_missed_*, last_alert_error) are legacy: the alerts live in
// check_alerts since checksAlertsMoved.
var checksMigrations = []string{
	`ALTER TABLE checks ADD COLUMN revision INTEGER NOT NULL DEFAULT 0`,
	`ALTER TABLE checks ADD COLUMN eval_at INTEGER`,
	`ALTER TABLE checks ADD COLUMN eval_local TEXT NOT NULL DEFAULT ''`,
	`ALTER TABLE checks ADD COLUMN eval_result TEXT NOT NULL DEFAULT ''`,
	`ALTER TABLE checks ADD COLUMN counted_mark_id INTEGER NOT NULL DEFAULT 0`,
	`ALTER TABLE checks ADD COLUMN alert_attempts INTEGER NOT NULL DEFAULT 0`,
	`ALTER TABLE checks ADD COLUMN alert_next_at INTEGER NOT NULL DEFAULT 0`,
	checksAlertStateAlter,
	checksAlertsMoved,
	checksMarkerPolicyAlter,
}

const checksMarkerPolicyAlter = `ALTER TABLE checks ADD COLUMN allowed_schedule_ids TEXT NOT NULL DEFAULT '[]'`

// Prior empty job ids meant the owner/CLI. Backfill once with the policy migration.
const checksMarkerProvenanceFill = `UPDATE checks SET last_mark_job = 'owner'
 WHERE last_mark_at IS NOT NULL AND last_mark_job = '';
 UPDATE check_marks SET job_id = 'owner' WHERE job_id = ''`

// checksAlertStateAlter adds the alert state; its backfill (checksAlertStateFill, in
// migrationBackfills) turns a missed time claimed before the column existed into the evaluation
// watermark. That code stored the claim before typing the alert and left last_alert_error empty
// until a delivery failed, so an empty error does not prove a delivery: such a row becomes a
// pending alert (delivered again, at-least-once); one with an error failed.
const checksAlertStateAlter = `ALTER TABLE checks ADD COLUMN alert_state TEXT NOT NULL DEFAULT ''`

const checksAlertStateFill = `UPDATE checks SET eval_at = last_missed_at,
	eval_local = last_missed_local, eval_result = 'missed',
	alert_attempts = CASE WHEN last_alert_error = '' THEN 0 ELSE 1 END,
	alert_state = CASE WHEN last_alert_error = '' THEN 'pending' ELSE 'failed' END
	WHERE last_missed_at IS NOT NULL AND eval_at IS NULL`

// checksAlertsMoved marks the move of the per-check alert into check_alerts; its backfill
// (checksAlertsFill) copies each check's alert there once.
const checksAlertsMoved = `ALTER TABLE checks ADD COLUMN alerts_moved INTEGER NOT NULL DEFAULT 1`

const checksAlertsFill = `INSERT INTO check_alerts (check_id, expected, expected_local, state, attempts, next_at, error)
	SELECT id, last_missed_at, last_missed_local, alert_state, alert_attempts, alert_next_at, last_alert_error
	FROM checks WHERE last_missed_at IS NOT NULL AND alert_state IN ('pending', 'delivered', 'failed') ORDER BY id`

// checkCols reads a check with its newest alert row (alias a) and the alert counts.
const checkCols = `c.id, c.name, c.cron, c.grace_minutes, c.enabled, c.created_at, c.updated_at, c.armed_at,
	c.last_mark_at, c.last_mark_note, c.last_mark_job, c.allowed_schedule_ids, c.revision, c.eval_at, c.eval_local, c.eval_result,
	a.expected, COALESCE(a.state, ''), COALESCE(a.attempts, 0), COALESCE(a.error, ''), COALESCE(a.merged, 0),
	COALESCE(a.acked, 0),
	(SELECT COALESCE(SUM(1 + p.merged), 0) FROM check_alerts p WHERE p.check_id = c.id AND p.state = 'pending'),
	(SELECT COALESCE(SUM(1 + f.merged), 0) FROM check_alerts f WHERE f.check_id = c.id AND f.state = 'failed' AND f.acked = 0)
	FROM checks c LEFT JOIN check_alerts a ON a.id = (SELECT MAX(l.id) FROM check_alerts l WHERE l.check_id = c.id)`

func scanCheck(r scanner) (*Check, error) {
	var c Check
	var created, updated, armed int64
	var mark, eval, missed sql.NullInt64
	var allowed string
	if err := r.Scan(&c.ID, &c.Name, &c.Cron, &c.GraceMinutes, &c.Enabled, &created, &updated, &armed,
		&mark, &c.LastMarkUntrustedNote, &c.LastMarkJob, &allowed, &c.Revision, &eval, &c.EvaluatedLocal, &c.EvaluatedResult,
		&missed, &c.AlertState, &c.AlertAttempts, &c.LastAlertError, &c.AlertMerged, &c.AlertAcknowledged,
		&c.AlertsPending, &c.AlertsFailed); err != nil {
		return nil, err
	}
	if err := json.Unmarshal([]byte(allowed), &c.AllowedScheduleIDs); err != nil {
		return nil, fmt.Errorf("check marker policy: %w", err)
	}
	c.CreatedAt, c.UpdatedAt, c.ArmedAt = time.Unix(created, 0), time.Unix(updated, 0), time.Unix(armed, 0)
	c.LastMarkAt, c.LastMissedAt, c.EvaluatedAt = convTS(mark), convTS(missed), convTS(eval)
	return &c, nil
}

// querier is what the checks statements run on: the database or one connection inside a
// transaction (immediate).
type querier interface {
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

// immediate runs fn in one BEGIN IMMEDIATE transaction (the write lock taken first, so two daemons
// serialize instead of failing a read-then-write upgrade): committed when fn returns nil.
func (s *Store) immediate(fn func(q querier) error) error {
	ctx := context.Background()
	conn, err := s.db.Conn(ctx)
	if err != nil {
		return err
	}
	defer conn.Close()
	if _, err := conn.ExecContext(ctx, `BEGIN IMMEDIATE`); err != nil {
		return err
	}
	if err := fn(conn); err != nil {
		_, _ = conn.ExecContext(ctx, `ROLLBACK`)
		return err
	}
	if _, err := conn.ExecContext(ctx, `COMMIT`); err != nil {
		_, _ = conn.ExecContext(ctx, `ROLLBACK`)
		return err
	}
	return nil
}

// AddCheck inserts an enabled check armed at now (ErrCheckExists when the name is taken).
// Validation is the caller's.
func (s *Store) AddCheck(name, cron string, grace int, now time.Time, allowedScheduleIDs ...[]int64) (*Check, error) {
	allowed := "[]"
	if len(allowedScheduleIDs) > 0 && len(allowedScheduleIDs[0]) > 0 {
		b, err := json.Marshal(allowedScheduleIDs[0])
		if err != nil {
			return nil, err
		}
		allowed = string(b)
	}
	_, err := s.db.Exec(`INSERT INTO checks (name, cron, grace_minutes, created_at, updated_at, armed_at, allowed_schedule_ids)
		VALUES (?, ?, ?, ?, ?, ?, ?)`, name, cron, grace, now.Unix(), now.Unix(), now.Unix(), allowed)
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
	return getCheck(context.Background(), s.db, name)
}

func getCheck(ctx context.Context, q querier, name string) (*Check, error) {
	c, err := scanCheck(q.QueryRowContext(ctx, `SELECT `+checkCols+` WHERE c.name = ?`, name))
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
	q := `SELECT ` + checkCols + ` ORDER BY c.id`
	if enabledOnly {
		q = `SELECT ` + checkCols + ` WHERE c.enabled = 1 ORDER BY c.id`
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
// of the cron or the grace, or enabling a disabled check, re-arms it at now (no expected time
// before that is judged, no mark before that counts). Every edit bumps the revision, so a watchdog
// that read the check before it cannot claim an expected time of the old configuration, and
// acknowledges the check's failed alerts (they leave status and system_doctor). Pending alerts are
// still delivered.
func (s *Store) SetCheck(name, cron string, grace int, enabled *bool, now time.Time, allowedScheduleIDs ...[]int64) (*Check, error) {
	var allowed any
	if len(allowedScheduleIDs) > 0 {
		ids := allowedScheduleIDs[0]
		if ids == nil {
			ids = []int64{}
		}
		b, err := json.Marshal(ids)
		if err != nil {
			return nil, err
		}
		allowed = string(b)
	}
	en := sql.NullBool{}
	if enabled != nil {
		en = sql.NullBool{Bool: *enabled, Valid: true}
	}
	var out *Check
	err := s.immediate(func(q querier) error {
		ctx := context.Background()
		res, err := q.ExecContext(ctx, `UPDATE checks SET
			armed_at = CASE WHEN (?1 <> '' AND ?1 <> cron) OR (?2 <> 0 AND ?2 <> grace_minutes) OR (?3 = 1 AND enabled = 0)
				THEN ?4 ELSE armed_at END,
			cron          = CASE WHEN ?1 = '' THEN cron ELSE ?1 END,
			grace_minutes = CASE WHEN ?2 = 0 THEN grace_minutes ELSE ?2 END,
			enabled       = COALESCE(?3, enabled),
			allowed_schedule_ids = COALESCE(?6, allowed_schedule_ids),
			revision      = revision + 1,
			updated_at    = ?4
			WHERE name = ?5`, cron, grace, en, now.Unix(), name, allowed)
		if err != nil {
			return fmt.Errorf("set check: %w", err)
		}
		if n, err := res.RowsAffected(); err != nil {
			return fmt.Errorf("set check: %w", err)
		} else if n == 0 {
			return ErrCheckNotFound
		}
		if _, err := q.ExecContext(ctx, `UPDATE check_alerts SET acked = 1
			WHERE check_id = (SELECT id FROM checks WHERE name = ?) AND state = ?`, name, AlertFailed); err != nil {
			return fmt.Errorf("acknowledge check alerts: %w", err)
		}
		out, err = getCheck(ctx, q, name)
		return err
	})
	return out, err
}

// RemoveCheck deletes a check, its marks and its alerts (ErrCheckNotFound when there is none).
func (s *Store) RemoveCheck(name string) error {
	return s.immediate(func(q querier) error {
		ctx := context.Background()
		c, err := getCheck(ctx, q, name)
		if err != nil {
			return err
		}
		for _, stmt := range []string{`DELETE FROM check_marks WHERE check_id = ?`,
			`DELETE FROM check_alerts WHERE check_id = ?`, `DELETE FROM checks WHERE id = ?`} {
			if _, err := q.ExecContext(ctx, stmt, c.ID); err != nil {
				return fmt.Errorf("remove check: %w", err)
			}
		}
		return nil
	})
}

// pruneMarks deletes the check's marks that can never matter again: those before the arm time
// (no owner in the current configuration) and those whose owner expected time is evaluated, i.e.
// at most eval_at + grace when eval_at is an expected time of the current configuration (after
// armed_at; an older watermark belongs to a previous configuration and proves nothing).
func pruneMarks(ctx context.Context, q querier, checkID int64) error {
	_, err := q.ExecContext(ctx, `DELETE FROM check_marks WHERE check_id = ?1 AND (
		at < (SELECT armed_at FROM checks WHERE id = ?1) OR
		at <= (SELECT CASE WHEN eval_at IS NOT NULL AND eval_at > armed_at THEN eval_at + grace_minutes * 60 ELSE -1 END
			FROM checks WHERE id = ?1))`, checkID)
	if err != nil {
		return fmt.Errorf("prune marks: %w", err)
	}
	return nil
}

// AddMark records a completion mark for the check (jobID "" = not from a job) at at and returns
// the check with its new last mark. window gives the mark's owner window from the check as read
// inside the transaction (so a concurrent check_set cannot skew it): when the window already
// holds MaxMarksPerWindow marks, or the mark has no owner, the mark is accepted but not stored
// (MarkResult MarkNotStored); the last mark is updated either way. Marks that can never matter again are
// pruned (pruneMarks).
func (s *Store) AddMark(name, note, jobID string, at time.Time, window func(*Check) (MarkWindow, error)) (*Check, error) {
	var out *Check
	err := s.immediate(func(q querier) error {
		ctx := context.Background()
		c, err := getCheck(ctx, q, name)
		if err != nil {
			return err
		}
		// Authorize under the same write lock as the mark, so check_set cannot race it.
		if jobID != "" && len(c.AllowedScheduleIDs) > 0 {
			var schedule string
			err := q.QueryRowContext(ctx, `SELECT schedule_id FROM jobs WHERE id = ? AND status = ?`, jobID, StatusRunning).Scan(&schedule)
			if err != nil && !errors.Is(err, sql.ErrNoRows) {
				return fmt.Errorf("mark authorization: %w", err)
			}
			id, parseErr := strconv.ParseInt(schedule, 10, 64)
			if err != nil || parseErr != nil || id <= 0 || !slices.Contains(c.AllowedScheduleIDs, id) {
				return fmt.Errorf("%w %q: job %q must be running and started by an allowed schedule", ErrCheckMarkForbidden, name, jobID)
			}
		}
		marker := jobID
		if marker == "" {
			marker = "owner"
		}
		w, err := window(c)
		if err != nil {
			return err
		}
		stored := false
		if w.OK {
			var n int
			if err := q.QueryRowContext(ctx, `SELECT COUNT(*) FROM check_marks WHERE check_id = ? AND at >= ? AND at <= ?`,
				c.ID, w.Lo.Unix(), w.Hi.Unix()).Scan(&n); err != nil {
				return fmt.Errorf("add mark: %w", err)
			}
			if n < MaxMarksPerWindow {
				if _, err := q.ExecContext(ctx, `INSERT INTO check_marks (check_id, at, note, job_id) VALUES (?, ?, ?, ?)`,
					c.ID, at.Unix(), note, marker); err != nil {
					return fmt.Errorf("add mark: %w", err)
				}
				stored = true
			}
		}
		if _, err := q.ExecContext(ctx, `UPDATE checks SET last_mark_at = ?, last_mark_note = ?, last_mark_job = ?
			WHERE id = ? AND (last_mark_at IS NULL OR last_mark_at <= ?)`, at.Unix(), note, marker, c.ID, at.Unix()); err != nil {
			return fmt.Errorf("add mark: %w", err)
		}
		if err := pruneMarks(ctx, q, c.ID); err != nil {
			return err
		}
		if out, err = getCheck(ctx, q, name); err != nil {
			return err
		}
		out.MarkResult = MarkNotStored
		if stored {
			out.MarkResult = MarkStored
		}
		return nil
	})
	return out, err
}

// MarkTimes are the times of the check's stored marks in [lo, hi] (unix seconds), ascending.
func (s *Store) MarkTimes(checkID int64, lo, hi time.Time) ([]time.Time, error) {
	rows, err := s.db.Query(`SELECT at FROM check_marks WHERE check_id = ? AND at >= ? AND at <= ? ORDER BY at`,
		checkID, lo.Unix(), hi.Unix())
	if err != nil {
		return nil, fmt.Errorf("check marks: %w", err)
	}
	defer rows.Close()
	var out []time.Time
	for rows.Next() {
		var at int64
		if err := rows.Scan(&at); err != nil {
			return nil, fmt.Errorf("check marks: %w", err)
		}
		out = append(out, time.Unix(at, 0))
	}
	return out, rows.Err()
}

// mergeAlerts keeps at most keep alert rows of the check in state (not acknowledged): the older
// ones are deleted and their count (1 + merged each) added to the newest row's merged counter,
// whose attempts and backoff stay as they are.
func mergeAlerts(ctx context.Context, q querier, checkID int64, state string, keep int) error {
	const old = `SELECT id FROM check_alerts WHERE check_id = ?1 AND state = ?2 AND acked = 0
		ORDER BY id DESC LIMIT -1 OFFSET ?3`
	if _, err := q.ExecContext(ctx, `UPDATE check_alerts SET merged = merged +
		(SELECT COALESCE(SUM(1 + merged), 0) FROM check_alerts WHERE id IN (`+old+`))
		WHERE id = (SELECT MAX(id) FROM check_alerts WHERE check_id = ?1 AND state = ?2 AND acked = 0)`,
		checkID, state, keep); err != nil {
		return fmt.Errorf("merge check alerts: %w", err)
	}
	if _, err := q.ExecContext(ctx, `DELETE FROM check_alerts WHERE id IN (`+old+`)`, checkID, state, keep); err != nil {
		return fmt.Errorf("merge check alerts: %w", err)
	}
	return nil
}

// trimDoneAlerts keeps the newest MaxDoneAlerts delivered or acknowledged alert rows of the check.
func trimDoneAlerts(ctx context.Context, q querier, checkID int64) error {
	const done = `check_id = ?1 AND (state = 'delivered' OR acked = 1)`
	if _, err := q.ExecContext(ctx, `DELETE FROM check_alerts WHERE `+done+` AND id NOT IN
		(SELECT id FROM check_alerts WHERE `+done+` ORDER BY id DESC LIMIT ?2)`, checkID, MaxDoneAlerts); err != nil {
		return fmt.Errorf("trim check alerts: %w", err)
	}
	return nil
}

// ClaimCheckEvaluations records the watchdog's judgement of consecutive expected times (outs, in
// chronological order): the watermark moves to the last one, each missed one becomes a pending
// alert row (at most MaxPendingAlerts per check, see mergeAlerts), and the marks whose owner is now
// evaluated are pruned. It holds only when the check is enabled, still at revision (no check_set
// since the watchdog read it), and the first expected time and its local minute are strictly
// later than the last evaluation's (the schedules watermark, ClaimScheduleFiring): one immediate
// transaction, so two daemons or a restarted one judge each expected time once, and the second
// pass of an hour repeated when DST ends is never judged again.
func (s *Store) ClaimCheckEvaluations(id, revision int64, outs []EvalOutcome) (bool, error) {
	if len(outs) == 0 {
		return false, nil
	}
	first, last := outs[0], outs[len(outs)-1]
	claimed := false
	err := s.immediate(func(q querier) error {
		ctx := context.Background()
		res, err := q.ExecContext(ctx, `UPDATE checks SET eval_at = ?1, eval_local = ?2, eval_result = ?3
			WHERE id = ?4 AND enabled = 1 AND revision = ?5 AND (eval_at IS NULL OR eval_at < ?6) AND eval_local < ?7`,
			last.Expected.Unix(), last.Local, evalResult(last.OK), id, revision, first.Expected.Unix(), first.Local)
		if err != nil {
			return fmt.Errorf("claim check: %w", err)
		}
		if n, err := res.RowsAffected(); err != nil || n != 1 {
			return err
		}
		claimed = true
		for _, o := range outs {
			if o.OK {
				continue
			}
			if _, err := q.ExecContext(ctx, `INSERT INTO check_alerts (check_id, expected, expected_local, state)
				VALUES (?, ?, ?, ?)`, id, o.Expected.Unix(), o.Local, AlertPending); err != nil {
				return fmt.Errorf("claim check: %w", err)
			}
		}
		if err := mergeAlerts(ctx, q, id, AlertPending, MaxPendingAlerts); err != nil {
			return err
		}
		if err := trimDoneAlerts(ctx, q, id); err != nil {
			return err
		}
		return pruneMarks(ctx, q, id)
	})
	if err != nil {
		return false, err
	}
	return claimed, nil
}

func evalResult(ok bool) string {
	if ok {
		return EvalOK
	}
	return EvalMissed
}

const alertCols = `a.id, a.check_id, c.name, c.grace_minutes, a.expected, a.state, a.attempts, a.token, a.error, a.merged, c.last_mark_at, c.last_mark_job, c.last_mark_note
	FROM check_alerts a JOIN checks c ON c.id = a.check_id`

func queryAlerts(ctx context.Context, q querier, where string, args ...any) ([]*CheckAlert, error) {
	rows, err := q.QueryContext(ctx, `SELECT `+alertCols+` WHERE `+where+` ORDER BY a.check_id, a.id`, args...)
	if err != nil {
		return nil, fmt.Errorf("check alerts: %w", err)
	}
	defer rows.Close()
	out := []*CheckAlert{}
	for rows.Next() {
		var a CheckAlert
		var exp int64
		var mark sql.NullInt64
		if err := rows.Scan(&a.ID, &a.CheckID, &a.Name, &a.Grace, &exp, &a.State, &a.Attempts, &a.Token,
			&a.Error, &a.Merged, &mark, &a.LastMarkJob, &a.LastMarkUntrustedNote); err != nil {
			return nil, fmt.Errorf("check alerts: %w", err)
		}
		a.Expected = time.Unix(exp, 0)
		a.LastMarkAt = convTS(mark)
		out = append(out, &a)
	}
	return out, rows.Err()
}

// DueCheckAlerts are the pending alert rows whose next attempt may start at now, by check and
// age. A disabled check's pending alerts are delivered too.
func (s *Store) DueCheckAlerts(now time.Time) ([]*CheckAlert, error) {
	return queryAlerts(context.Background(), s.db, `a.state = ? AND a.next_at <= ?`, AlertPending, now.Unix())
}

// CheckAlerts are every alert row of the check, oldest first.
func (s *Store) CheckAlerts(checkID int64) ([]*CheckAlert, error) {
	return queryAlerts(context.Background(), s.db, `a.check_id = ?`, checkID)
}

// ClaimCheckAlertAttempt takes delivery attempt attempts+1 of a pending alert row that is still at
// attempts tries and due at now, under token (the fence: only the holder of the row's current
// token may type it); the next attempt may start at next, so an attempt cut short by a crash is
// retried then. One conditional UPDATE: two daemons never make the same attempt.
func (s *Store) ClaimCheckAlertAttempt(alertID int64, attempts int, token string, now, next time.Time) (bool, error) {
	res, err := s.db.Exec(`UPDATE check_alerts SET attempts = attempts + 1, next_at = ?, token = ?
		WHERE id = ? AND state = ? AND attempts = ? AND next_at <= ?`,
		next.Unix(), token, alertID, AlertPending, attempts, now.Unix())
	if err != nil {
		return false, fmt.Errorf("claim check alert: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("claim check alert: %w", err)
	}
	return n == 1, nil
}

// FailCheckAlert gives up a pending alert row still at attempts tries (its last attempt was cut
// short before its outcome was stored).
func (s *Store) FailCheckAlert(alertID int64, attempts int, errText string) error {
	return s.immediate(func(q querier) error {
		ctx := context.Background()
		var checkID int64
		err := q.QueryRowContext(ctx, `UPDATE check_alerts SET state = ?, error = ? WHERE id = ? AND state = ? AND attempts = ?
			RETURNING check_id`, AlertFailed, errText, alertID, AlertPending, attempts).Scan(&checkID)
		if errors.Is(err, sql.ErrNoRows) {
			return nil
		}
		if err != nil {
			return fmt.Errorf("fail check alert: %w", err)
		}
		return mergeAlerts(ctx, q, checkID, AlertFailed, MaxFailedAlerts)
	})
}

// TokenCheckAlerts are the pending alert rows still held by token (the fence's revalidation).
func (s *Store) TokenCheckAlerts(token string) ([]*CheckAlert, error) {
	return queryAlerts(context.Background(), s.db, `a.state = ? AND a.token = ?`, AlertPending, token)
}

// DeliverCheckAlerts records the alert rows ids as delivered (the line holding them was typed);
// rows no longer pending are left alone.
func (s *Store) DeliverCheckAlerts(ids []int64) error {
	return s.immediate(func(q querier) error {
		ctx := context.Background()
		for _, id := range ids {
			if _, err := q.ExecContext(ctx, `UPDATE check_alerts SET state = ?, error = '' WHERE id = ? AND state = ?`,
				AlertDelivered, id, AlertPending); err != nil {
				return fmt.Errorf("record check alert: %w", err)
			}
		}
		return nil
	})
}

// FailCheckAlertAttempt records a failed delivery attempt of the alert rows still held by token:
// the error is kept and a row stays pending, or becomes failed once it made maxAttempts attempts.
func (s *Store) FailCheckAlertAttempt(token, errText string, maxAttempts int) error {
	return s.immediate(func(q querier) error {
		ctx := context.Background()
		rows, err := queryAlerts(ctx, q, `a.state = ? AND a.token = ?`, AlertPending, token)
		if err != nil {
			return err
		}
		checks := map[int64]bool{}
		for _, a := range rows {
			state := AlertPending
			if a.Attempts >= maxAttempts {
				state, checks[a.CheckID] = AlertFailed, true
			}
			if _, err := q.ExecContext(ctx, `UPDATE check_alerts SET state = ?, error = ? WHERE id = ? AND token = ? AND state = ?`,
				state, errText, a.ID, token, AlertPending); err != nil {
				return fmt.Errorf("record check alert: %w", err)
			}
		}
		for id := range checks {
			if err := mergeAlerts(ctx, q, id, AlertFailed, MaxFailedAlerts); err != nil {
				return err
			}
		}
		return nil
	})
}
