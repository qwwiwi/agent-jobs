package store

import (
	"fmt"
	"time"
)

// OwnerPending is one row of the owner_pending table: an owner-waits job whose job-done line reached
// the orchestrator session, and the owner has not been written to yet (owner_replied clears it).
type OwnerPending struct {
	JobID string `json:"job_id"`
	Title string `json:"title"`
	// Status is the job's status when the line was delivered.
	Status    string    `json:"status"`
	CreatedAt time.Time `json:"created_at"`
}

// ownerPendingSchema is applied on every Open (CREATE … IF NOT EXISTS): an older database gains the
// table and keeps everything else.
const ownerPendingSchema = `
CREATE TABLE IF NOT EXISTS owner_pending (
	job_id     TEXT PRIMARY KEY,
	created_at INTEGER NOT NULL
);
`

// MarkNotified records that the job-done line was delivered. For an owner-waits job it also records
// «owner reply pending» (a resumed job's later line renews it), in the same transaction, so a
// delivered line of such a job never goes without its record.
func (s *Store) MarkNotified(id string, now time.Time) error {
	tx, err := s.db.Begin()
	if err != nil {
		return fmt.Errorf("mark notified: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := tx.Exec(`UPDATE jobs SET notified_at=? WHERE id=?`, now.Unix(), id); err != nil {
		return fmt.Errorf("mark notified: %w", err)
	}
	if _, err := tx.Exec(`INSERT INTO owner_pending (job_id, created_at)
		SELECT id, ? FROM jobs WHERE id = ? AND owner_waits = 1
		ON CONFLICT(job_id) DO UPDATE SET created_at = excluded.created_at`, now.Unix(), id); err != nil {
		return fmt.Errorf("mark notified: owner pending: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("mark notified: %w", err)
	}
	return nil
}

// OwnerPendingList first drops the records older than maxAge (a stale record never holds the
// session forever) and returns the rest, oldest first, with how many expired now.
func (s *Store) OwnerPendingList(now time.Time, maxAge time.Duration) ([]*OwnerPending, int, error) {
	res, err := s.db.Exec(`DELETE FROM owner_pending WHERE created_at < ?`, now.Add(-maxAge).Unix())
	if err != nil {
		return nil, 0, fmt.Errorf("expire owner pending: %w", err)
	}
	expired, err := res.RowsAffected()
	if err != nil {
		return nil, 0, fmt.Errorf("expire owner pending: %w", err)
	}
	rows, err := s.db.Query(`SELECT p.job_id, COALESCE(j.title, ''), COALESCE(j.status, ''), p.created_at
		FROM owner_pending p LEFT JOIN jobs j ON j.id = p.job_id ORDER BY p.created_at, p.job_id`)
	if err != nil {
		return nil, 0, fmt.Errorf("owner pending: %w", err)
	}
	defer rows.Close()
	out := []*OwnerPending{}
	for rows.Next() {
		var p OwnerPending
		var created int64
		if err := rows.Scan(&p.JobID, &p.Title, &p.Status, &created); err != nil {
			return nil, 0, fmt.Errorf("scan owner pending: %w", err)
		}
		p.CreatedAt = time.Unix(created, 0)
		out = append(out, &p)
	}
	return out, int(expired), rows.Err()
}

// ClearOwnerPending removes the record of job id, or every record when id is "", and returns the ids
// it removed.
func (s *Store) ClearOwnerPending(id string) ([]string, error) {
	q, args := `DELETE FROM owner_pending RETURNING job_id`, []any{}
	if id != "" {
		q, args = `DELETE FROM owner_pending WHERE job_id = ? RETURNING job_id`, []any{id}
	}
	rows, err := s.db.Query(q, args...)
	if err != nil {
		return nil, fmt.Errorf("clear owner pending: %w", err)
	}
	defer rows.Close()
	out := []string{}
	for rows.Next() {
		var jid string
		if err := rows.Scan(&jid); err != nil {
			return nil, fmt.Errorf("clear owner pending: %w", err)
		}
		out = append(out, jid)
	}
	return out, rows.Err()
}
