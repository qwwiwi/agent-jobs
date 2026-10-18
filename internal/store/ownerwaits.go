package store

import (
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// States of an «owner reply pending» record: how far the job-done line got.
const (
	// OwnerDelivering: the job finished and its line is being delivered (or a failed delivery is
	// retried). The record is written before the line goes out, so the session never sees the line
	// without the record.
	OwnerDelivering = "delivering"
	// OwnerDelivered: tmux accepted the job-done line.
	OwnerDelivered = "delivered"
	// OwnerNotifyOff: notify is off (no notify_tmux_target), no line reaches the session.
	OwnerNotifyOff = "notify_off"
)

// OwnerPending is one open row of the owner_pending table: a finished owner-waits job the owner has
// not been written to yet (owner_replied clears it).
type OwnerPending struct {
	JobID string `json:"job_id"`
	Title string `json:"title"`
	// Status is the job's status.
	Status string `json:"status"`
	// State is how far the job-done line got: delivering, delivered or notify_off.
	State     string    `json:"state"`
	CreatedAt time.Time `json:"created_at"`
}

// ownerPendingSchema is applied on every Open (CREATE … IF NOT EXISTS): an older database gains the
// tables and keeps everything else. A row is kept after owner_replied (replied=1) until it expires,
// so a retried line of the same attempt does not bring it back; a new attempt (resume) reopens it.
// hook_session holds the one orchestrator session the Stop hook may hold (`hook bind`).
const ownerPendingSchema = `
CREATE TABLE IF NOT EXISTS owner_pending (
	job_id     TEXT PRIMARY KEY,
	created_at INTEGER NOT NULL,
	attempt    INTEGER NOT NULL DEFAULT 0,
	state      TEXT NOT NULL DEFAULT 'delivered',
	replied    INTEGER NOT NULL DEFAULT 0
);
CREATE TABLE IF NOT EXISTS hook_session (
	id         INTEGER PRIMARY KEY CHECK (id = 1),
	session_id TEXT NOT NULL,
	bound_at   INTEGER NOT NULL
);
`

// ownerPendingMigrations add the columns an owner_pending table of the first owner-waits build lacks.
var ownerPendingMigrations = []string{
	`ALTER TABLE owner_pending ADD COLUMN attempt INTEGER NOT NULL DEFAULT 0`,
	`ALTER TABLE owner_pending ADD COLUMN state TEXT NOT NULL DEFAULT 'delivered'`,
	`ALTER TABLE owner_pending ADD COLUMN replied INTEGER NOT NULL DEFAULT 0`,
}

// execer is a *sql.DB or a *sql.Tx.
type execer interface {
	Exec(query string, args ...any) (sql.Result, error)
}

// upsertOwnerPending writes the record of owner-waits job id (nothing for another job) for the job's
// current attempt in state. A record of an older attempt is reopened. A record of the same attempt is
// updated only when also is true for it (a SQL condition on the row), so one statement decides
// atomically against a concurrent owner_replied.
func upsertOwnerPending(db execer, id, state string, now time.Time, also string) error {
	_, err := db.Exec(`INSERT INTO owner_pending (job_id, attempt, state, replied, created_at)
		SELECT id, attempt, ?, 0, ? FROM jobs WHERE id = ? AND owner_waits = 1
		ON CONFLICT(job_id) DO UPDATE SET attempt = excluded.attempt, state = excluded.state,
			replied = 0, created_at = excluded.created_at
		WHERE owner_pending.attempt <> excluded.attempt OR (`+also+`)`, state, now.Unix(), id)
	return err
}

// BeginOwnerPending records «owner reply pending» for owner-waits job id before its job-done line is
// delivered (state OwnerDelivering), or in place of the line when notify is off (OwnerNotifyOff). A
// retry of the same attempt leaves the record as it is: an owner_replied in between stays.
func (s *Store) BeginOwnerPending(id, state string, now time.Time) error {
	if state != OwnerDelivering && state != OwnerNotifyOff {
		return fmt.Errorf("begin owner pending: bad state %q", state)
	}
	if err := upsertOwnerPending(s.db, id, state, now, "0"); err != nil {
		return fmt.Errorf("begin owner pending: %w", err)
	}
	return nil
}

// MarkNotified records that the job-done line was delivered. For an owner-waits job the open
// «delivering» record becomes «delivered» (its age counts from now); a record already cleared by
// owner_replied stays cleared; a job without a record (nothing began it) gets a delivered one. One
// transaction, so a delivered line of such a job never goes without its record.
func (s *Store) MarkNotified(id string, now time.Time) error {
	tx, err := s.db.Begin()
	if err != nil {
		return fmt.Errorf("mark notified: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := tx.Exec(`UPDATE jobs SET notified_at=? WHERE id=?`, now.Unix(), id); err != nil {
		return fmt.Errorf("mark notified: %w", err)
	}
	if err := upsertOwnerPending(tx, id, OwnerDelivered, now,
		`owner_pending.replied = 0 AND owner_pending.state = '`+OwnerDelivering+`'`); err != nil {
		return fmt.Errorf("mark notified: owner pending: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("mark notified: %w", err)
	}
	return nil
}

// OwnerPendingList first drops the records older than maxAge (a stale record never holds the
// session forever) and returns the open rest, oldest first, with how many open ones expired now.
func (s *Store) OwnerPendingList(now time.Time, maxAge time.Duration) ([]*OwnerPending, int, error) {
	var expired int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM owner_pending WHERE created_at < ? AND replied = 0`,
		now.Add(-maxAge).Unix()).Scan(&expired); err != nil {
		return nil, 0, fmt.Errorf("expire owner pending: %w", err)
	}
	if _, err := s.db.Exec(`DELETE FROM owner_pending WHERE created_at < ?`, now.Add(-maxAge).Unix()); err != nil {
		return nil, 0, fmt.Errorf("expire owner pending: %w", err)
	}
	rows, err := s.db.Query(`SELECT p.job_id, COALESCE(j.title, ''), COALESCE(j.status, ''), p.state, p.created_at
		FROM owner_pending p LEFT JOIN jobs j ON j.id = p.job_id WHERE p.replied = 0
		ORDER BY p.created_at, p.job_id`)
	if err != nil {
		return nil, 0, fmt.Errorf("owner pending: %w", err)
	}
	defer rows.Close()
	out := []*OwnerPending{}
	for rows.Next() {
		var p OwnerPending
		var created int64
		if err := rows.Scan(&p.JobID, &p.Title, &p.Status, &p.State, &created); err != nil {
			return nil, 0, fmt.Errorf("scan owner pending: %w", err)
		}
		p.CreatedAt = time.Unix(created, 0)
		out = append(out, &p)
	}
	return out, expired, rows.Err()
}

// ClearOwnerPending closes the open record of job id (delivered or not), or every open record when id
// is "", and returns the ids it closed. A given id of a finished owner-waits job whose line has not
// begun yet gets a closed record for its attempt, so the later line holds nothing.
func (s *Store) ClearOwnerPending(id string, now time.Time) ([]string, error) {
	tx, err := s.db.Begin()
	if err != nil {
		return nil, fmt.Errorf("clear owner pending: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	q, args := `UPDATE owner_pending SET replied = 1 WHERE replied = 0 RETURNING job_id`, []any{}
	if id != "" {
		q, args = `UPDATE owner_pending SET replied = 1 WHERE job_id = ? AND replied = 0 RETURNING job_id`, []any{id}
	}
	out, err := scanIDs(tx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("clear owner pending: %w", err)
	}
	if id != "" && len(out) == 0 {
		out, err = scanIDs(tx, `INSERT INTO owner_pending (job_id, attempt, state, replied, created_at)
			SELECT id, attempt, ?, 1, ? FROM jobs WHERE id = ? AND owner_waits = 1
				AND status NOT IN ('queued','running','stopped') AND notified_at IS NULL
			ON CONFLICT(job_id) DO UPDATE SET attempt = excluded.attempt, state = excluded.state,
				replied = 1, created_at = excluded.created_at
			WHERE owner_pending.attempt <> excluded.attempt
			RETURNING job_id`, OwnerDelivering, now.Unix(), id)
		if err != nil {
			return nil, fmt.Errorf("clear owner pending: %w", err)
		}
	}
	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("clear owner pending: %w", err)
	}
	return out, nil
}

func scanIDs(tx *sql.Tx, q string, args ...any) ([]string, error) {
	rows, err := tx.Query(q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []string{}
	for rows.Next() {
		var jid string
		if err := rows.Scan(&jid); err != nil {
			return nil, err
		}
		out = append(out, jid)
	}
	return out, rows.Err()
}

// BindHookSession records the orchestrator session the Stop hook holds (the last bind wins).
func (s *Store) BindHookSession(sessionID string, now time.Time) error {
	if sessionID == "" {
		return errors.New("bind hook session: empty session id")
	}
	_, err := s.db.Exec(`INSERT INTO hook_session (id, session_id, bound_at) VALUES (1, ?, ?)
		ON CONFLICT(id) DO UPDATE SET session_id = excluded.session_id, bound_at = excluded.bound_at`,
		sessionID, now.Unix())
	if err != nil {
		return fmt.Errorf("bind hook session: %w", err)
	}
	return nil
}

// HookSession returns the bound orchestrator session and when it was bound; "" when none is bound.
func (s *Store) HookSession() (string, time.Time, error) {
	var id string
	var at int64
	err := s.db.QueryRow(`SELECT session_id, bound_at FROM hook_session WHERE id = 1`).Scan(&id, &at)
	if errors.Is(err, sql.ErrNoRows) {
		return "", time.Time{}, nil
	}
	if err != nil {
		return "", time.Time{}, fmt.Errorf("hook session: %w", err)
	}
	return id, time.Unix(at, 0), nil
}
