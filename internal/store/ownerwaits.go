package store

import (
	"database/sql"
	"errors"
	"fmt"
	"slices"
	"time"
)

// States of an owner_pending row: one row per (job, attempt). Open states (delivering, delivered,
// notify_off) hold the orchestrator's Stop hook; acknowledged and expired are final for the attempt.
const (
	// OwnerDelivering: the job finished and its line is being delivered (or a failed delivery is
	// retried). The row is written before the line goes out, so the session never sees the line
	// without the row.
	OwnerDelivering = "delivering"
	// OwnerDelivered: tmux accepted the job-done line.
	OwnerDelivered = "delivered"
	// OwnerNotifyOff: notify is off (no notify_tmux_target), no line reaches the session.
	OwnerNotifyOff = "notify_off"
	// OwnerAcknowledged: owner_replied closed the attempt (also ahead of its line). Final.
	OwnerAcknowledged = "acknowledged"
	// OwnerExpired: the attempt stayed open longer than the max age. Final: a retried delivery of
	// the same attempt never reopens it.
	OwnerExpired = "expired"
)

// ownerOpenStates is the SQL list of the states that hold the Stop hook.
const ownerOpenStates = `('` + OwnerDelivering + `','` + OwnerDelivered + `','` + OwnerNotifyOff + `')`

// OwnerPending is one open owner-waits job: finished, and the owner has not been written to yet
// (owner_replied clears it). A job with several open attempts is listed once, by its latest one.
type OwnerPending struct {
	JobID string `json:"job_id"`
	Title string `json:"title"`
	// Status is the job's status.
	Status string `json:"status"`
	// State is how far the job-done line got: delivering, delivered or notify_off.
	State     string    `json:"state"`
	CreatedAt time.Time `json:"created_at"`
}

// ownerPendingSchema is applied on every Open (CREATE … IF NOT EXISTS). owner_pending keeps one row
// per (job, attempt): retries of the same attempt reuse it, acknowledged and expired rows are never
// reopened, and rows are pruned only once the job can no longer deliver that attempt's line.
// hook_session holds the one orchestrator session the Stop hook may hold (`hook bind`).
const ownerPendingSchema = `
CREATE TABLE IF NOT EXISTS owner_pending (
	job_id     TEXT NOT NULL,
	attempt    INTEGER NOT NULL,
	state      TEXT NOT NULL,
	created_at INTEGER NOT NULL,
	PRIMARY KEY (job_id, attempt)
);
CREATE TABLE IF NOT EXISTS hook_session (
	id         INTEGER PRIMARY KEY CHECK (id = 1),
	session_id TEXT NOT NULL,
	bound_at   INTEGER NOT NULL
);
`

// ownerPendingLegacyColumns are the columns an owner_pending table of the first owner-waits builds
// (one row per job, job_id the primary key) may lack; they are added before its rows are copied.
var ownerPendingLegacyColumns = []string{
	`ALTER TABLE owner_pending ADD COLUMN attempt INTEGER NOT NULL DEFAULT 0`,
	`ALTER TABLE owner_pending ADD COLUMN state TEXT NOT NULL DEFAULT 'delivered'`,
	`ALTER TABLE owner_pending ADD COLUMN replied INTEGER NOT NULL DEFAULT 0`,
}

// migrateOwnerPending rebuilds an owner_pending table keyed by job_id alone into the (job, attempt)
// table: a replied row becomes acknowledged, the rest keep their state. A missing table or the new
// one is left alone.
func migrateOwnerPending(db *sql.DB) error {
	var pk int
	if err := db.QueryRow(`SELECT COUNT(*) FROM pragma_table_info('owner_pending') WHERE pk > 0`).Scan(&pk); err != nil {
		return err
	}
	if pk != 1 {
		return nil
	}
	for _, alter := range ownerPendingLegacyColumns {
		if err := migrate(db, alter); err != nil {
			return err
		}
	}
	tx, err := db.Begin()
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	for _, q := range []string{
		`ALTER TABLE owner_pending RENAME TO owner_pending_legacy`,
		ownerPendingSchema,
		`INSERT OR IGNORE INTO owner_pending (job_id, attempt, state, created_at)
			SELECT job_id, attempt, CASE WHEN replied = 1 THEN '` + OwnerAcknowledged + `' ELSE state END, created_at
			FROM owner_pending_legacy`,
		`DROP TABLE owner_pending_legacy`,
	} {
		if _, err := tx.Exec(q); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// BeginOwnerPending records «owner reply pending» for the current attempt of owner-waits job id
// (nothing for another job) before its job-done line is delivered (state OwnerDelivering), or in
// place of the line when notify is off (OwnerNotifyOff). An existing row of the attempt is kept as it
// is: a retry never reopens an acknowledged or expired attempt, and a pre-acknowledged attempt
// starts acknowledged.
func (s *Store) BeginOwnerPending(id, state string, now time.Time) error {
	if state != OwnerDelivering && state != OwnerNotifyOff {
		return fmt.Errorf("begin owner pending: bad state %q", state)
	}
	if _, err := s.db.Exec(`INSERT INTO owner_pending (job_id, attempt, state, created_at)
		SELECT id, attempt, ?, ? FROM jobs WHERE id = ? AND owner_waits = 1
		ON CONFLICT(job_id, attempt) DO NOTHING`, state, now.Unix(), id); err != nil {
		return fmt.Errorf("begin owner pending: %w", err)
	}
	return nil
}

// MarkNotified records that the job-done line was delivered. For an owner-waits job the attempt's
// «delivering» row becomes «delivered» (its age counts from now); an acknowledged, expired or
// notify_off row stays as it is; an attempt without a row (nothing began it) gets a delivered one.
// One transaction, so a delivered line of such a job never goes without its row.
func (s *Store) MarkNotified(id string, now time.Time) error {
	tx, err := s.db.Begin()
	if err != nil {
		return fmt.Errorf("mark notified: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := tx.Exec(`UPDATE jobs SET notified_at=? WHERE id=?`, now.Unix(), id); err != nil {
		return fmt.Errorf("mark notified: %w", err)
	}
	if _, err := tx.Exec(`INSERT INTO owner_pending (job_id, attempt, state, created_at)
		SELECT id, attempt, ?, ? FROM jobs WHERE id = ? AND owner_waits = 1
		ON CONFLICT(job_id, attempt) DO UPDATE SET state = excluded.state, created_at = excluded.created_at
		WHERE owner_pending.state = ?`, OwnerDelivered, now.Unix(), id, OwnerDelivering); err != nil {
		return fmt.Errorf("mark notified: owner pending: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("mark notified: %w", err)
	}
	return nil
}

// OwnerPendingList first expires the open rows older than maxAge (a stale row never holds the
// session forever, and an expired attempt stays expired), prunes the rows past maxAge whose job can
// no longer deliver their attempt's line (the job is gone, has a newer attempt, or is finished and
// notified), and returns the open jobs, oldest first, with how many rows expired now.
func (s *Store) OwnerPendingList(now time.Time, maxAge time.Duration) ([]*OwnerPending, int, error) {
	cutoff := now.Add(-maxAge).Unix()
	tx, err := s.db.Begin()
	if err != nil {
		return nil, 0, fmt.Errorf("owner pending: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	res, err := tx.Exec(`UPDATE owner_pending SET state = ? WHERE created_at < ? AND state IN `+ownerOpenStates,
		OwnerExpired, cutoff)
	if err != nil {
		return nil, 0, fmt.Errorf("expire owner pending: %w", err)
	}
	expired, err := res.RowsAffected()
	if err != nil {
		return nil, 0, fmt.Errorf("expire owner pending: %w", err)
	}
	if _, err := tx.Exec(`DELETE FROM owner_pending WHERE created_at < ? AND NOT EXISTS (
		SELECT 1 FROM jobs j WHERE j.id = owner_pending.job_id AND j.attempt = owner_pending.attempt
			AND (j.status IN ('queued','running','stopped') OR j.notified_at IS NULL))`, cutoff); err != nil {
		return nil, 0, fmt.Errorf("prune owner pending: %w", err)
	}
	// One line per job: its latest open attempt.
	rows, err := tx.Query(`SELECT p.job_id, COALESCE(j.title, ''), COALESCE(j.status, ''), p.state, p.created_at
		FROM owner_pending p LEFT JOIN jobs j ON j.id = p.job_id
		WHERE p.state IN ` + ownerOpenStates + ` AND p.attempt = (SELECT MAX(q.attempt) FROM owner_pending q
			WHERE q.job_id = p.job_id AND q.state IN ` + ownerOpenStates + `)
		ORDER BY p.created_at, p.job_id`)
	if err != nil {
		return nil, 0, fmt.Errorf("owner pending: %w", err)
	}
	out := []*OwnerPending{}
	for rows.Next() {
		var p OwnerPending
		var created int64
		if err := rows.Scan(&p.JobID, &p.Title, &p.Status, &p.State, &created); err != nil {
			_ = rows.Close()
			return nil, 0, fmt.Errorf("scan owner pending: %w", err)
		}
		p.CreatedAt = time.Unix(created, 0)
		out = append(out, &p)
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return nil, 0, fmt.Errorf("owner pending: %w", err)
	}
	_ = rows.Close()
	if err := tx.Commit(); err != nil {
		return nil, 0, fmt.Errorf("owner pending: %w", err)
	}
	return out, int(expired), nil
}

// ClearOwnerPending acknowledges, in one transaction, every attempt of job id that has a row and
// records a pre-acknowledgement for the job's latest finished attempt (so its line, delivered later,
// holds nothing); with id "" it acknowledges every open row. It returns the ids of the jobs it
// changed, sorted.
func (s *Store) ClearOwnerPending(id string, now time.Time) ([]string, error) {
	tx, err := s.db.Begin()
	if err != nil {
		return nil, fmt.Errorf("clear owner pending: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	var out []string
	if id == "" {
		out, err = scanIDs(tx, `UPDATE owner_pending SET state = ? WHERE state IN `+ownerOpenStates+
			` RETURNING job_id`, OwnerAcknowledged)
	} else {
		var acked, ahead []string
		acked, err = scanIDs(tx, `UPDATE owner_pending SET state = ? WHERE job_id = ? AND state <> ?
			RETURNING job_id`, OwnerAcknowledged, id, OwnerAcknowledged)
		if err == nil {
			ahead, err = scanIDs(tx, `INSERT INTO owner_pending (job_id, attempt, state, created_at)
				SELECT id, attempt, ?, ? FROM jobs WHERE id = ? AND owner_waits = 1
					AND status NOT IN ('queued','running','stopped')
				ON CONFLICT(job_id, attempt) DO UPDATE SET state = excluded.state
				WHERE owner_pending.state <> excluded.state
				RETURNING job_id`, OwnerAcknowledged, now.Unix(), id)
		}
		out = append(acked, ahead...)
	}
	if err != nil {
		return nil, fmt.Errorf("clear owner pending: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("clear owner pending: %w", err)
	}
	return uniqueSorted(out), nil
}

// uniqueSorted returns ids sorted without repeats (never nil).
func uniqueSorted(ids []string) []string {
	seen := map[string]bool{}
	out := []string{}
	for _, id := range ids {
		if !seen[id] {
			seen[id] = true
			out = append(out, id)
		}
	}
	slices.Sort(out)
	return out
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

// BindHookSession records the orchestrator session the Stop hook holds (the last bind wins). The
// provenance checks (tmux pane, orchestrator directory, not a job) are the caller's
// (jobs.Manager.BindHookSession).
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
