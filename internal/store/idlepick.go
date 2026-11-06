package store

import (
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// Idle pick event kinds (#165). Every row is one thing the daemon did about the queue-empty
// transition of an epoch; the rows are the only state, so a restarted daemon sees what the last one did.
const (
	// IdleEval: the transition was evaluated and nothing was said (no ready work).
	IdleEval = "eval"
	// IdleNote: the idle note rode on a job-done delivery.
	IdleNote = "note"
	// IdleSuppressed: ready work was found but the cooldown or the daily cap held the note back; the
	// epoch stays unevaluated, so the recovery scan looks at it again.
	IdleSuppressed = "suppressed"
	// IdleScan: a recovery scan ran (whatever it found); spaces the scans. A scan that finds ready work
	// puts the last finished job's line back on the job-done delivery (RearmLastNotified); it never types.
	IdleScan = "scan"
)

// idlePickSchema is applied on every Open (CREATE … IF NOT EXISTS): an older database gains the tables
// and the trigger. idle_pick_seq counts the jobs that ended (left queued, running or stopped), kept by
// a trigger so every process that ends a job advances it: it is the queue epoch, which Unix-second
// stamps cannot tell apart (a job enqueued and cancelled in the second its predecessor's epoch was
// evaluated is another epoch). An event records the epoch it covered.
const idlePickSchema = `
CREATE TABLE IF NOT EXISTS idle_pick_events (
	id    INTEGER PRIMARY KEY AUTOINCREMENT,
	at    INTEGER NOT NULL,
	kind  TEXT NOT NULL,
	epoch INTEGER NOT NULL DEFAULT 0
);
CREATE INDEX IF NOT EXISTS idle_pick_events_at ON idle_pick_events(at);
CREATE TABLE IF NOT EXISTS idle_pick_seq (
	id INTEGER PRIMARY KEY CHECK (id = 1),
	n  INTEGER NOT NULL
);
INSERT OR IGNORE INTO idle_pick_seq (id, n) VALUES (1, 0);
CREATE TRIGGER IF NOT EXISTS idle_pick_job_ended AFTER UPDATE OF status ON jobs
WHEN OLD.status IN ('queued','running','stopped') AND NEW.status NOT IN ('queued','running','stopped')
BEGIN
	UPDATE idle_pick_seq SET n = n + 1 WHERE id = 1;
END;
`

// idlePickMigrations add columns introduced after the table first shipped, in order.
var idlePickMigrations = []string{
	`ALTER TABLE idle_pick_events ADD COLUMN epoch INTEGER NOT NULL DEFAULT 0`,
}

// IdleMark is an idle pick event recorded in the same transaction as the job-done acknowledgement it
// belongs to.
type IdleMark struct {
	Kind  string
	Epoch int64
}

// idlePickKeep is how long events are kept: the daily cap looks back a day, the rest a few scans.
const idlePickKeep = 7 * 24 * time.Hour

// AddIdlePickEvent records one event at now, covering queue epoch epoch, and drops the events older
// than a week.
func (s *Store) AddIdlePickEvent(kind string, now time.Time, epoch int64) error {
	tx, err := s.db.Begin()
	if err != nil {
		return fmt.Errorf("record idle pick event: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	if err := addIdlePickEvent(tx, kind, now, epoch); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("record idle pick event: %w", err)
	}
	return nil
}

func addIdlePickEvent(tx *sql.Tx, kind string, now time.Time, epoch int64) error {
	if _, err := tx.Exec(`INSERT INTO idle_pick_events (at, kind, epoch) VALUES (?, ?, ?)`, now.Unix(), kind, epoch); err != nil {
		return fmt.Errorf("record idle pick event: %w", err)
	}
	if _, err := tx.Exec(`DELETE FROM idle_pick_events WHERE at < ?`, now.Add(-idlePickKeep).Unix()); err != nil {
		return fmt.Errorf("prune idle pick events: %w", err)
	}
	return nil
}

// FinishSeq is the queue epoch: how many jobs have ended so far. A new value means a job ended since.
func (s *Store) FinishSeq() (int64, error) {
	var n int64
	if err := s.db.QueryRow(`SELECT n FROM idle_pick_seq WHERE id = 1`).Scan(&n); err != nil {
		return 0, fmt.Errorf("idle pick epoch: %w", err)
	}
	return n, nil
}

// LastIdlePickEpoch returns the highest epoch covered by an event of any of the kinds (0 when none).
func (s *Store) LastIdlePickEpoch(kinds ...string) (int64, error) {
	if len(kinds) == 0 {
		return 0, nil
	}
	args := make([]any, 0, len(kinds))
	for _, k := range kinds {
		args = append(args, k)
	}
	var n sql.NullInt64
	if err := s.db.QueryRow(`SELECT MAX(epoch) FROM idle_pick_events WHERE kind IN (?`+repeatPlaceholders(len(kinds)-1)+`)`, args...).Scan(&n); err != nil {
		return 0, fmt.Errorf("last idle pick epoch: %w", err)
	}
	return n.Int64, nil
}

// MarkNotifiedBatch is MarkNotified for every job of one delivered line, plus the idle event of the
// note that rode on it (nil: none), in one transaction: either the line is acknowledged and its note
// recorded, or neither is and the whole line is retried.
func (s *Store) MarkNotifiedBatch(ids []string, now time.Time, idle *IdleMark) error {
	tx, err := s.db.Begin()
	if err != nil {
		return fmt.Errorf("mark notified: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	for _, id := range ids {
		if err := markNotifiedTx(tx, id, now); err != nil {
			return err
		}
	}
	if idle != nil {
		if err := addIdlePickEvent(tx, idle.Kind, now, idle.Epoch); err != nil {
			return err
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("mark notified: %w", err)
	}
	return nil
}

// RearmLastNotified puts the line of the latest delivered job back on the job-done delivery (the job is
// pending again) and returns its id; "" when there is none. An owner-waits job is never re-armed: its
// line is tied to an owner reply. The recovery scan uses it so that a missed idle note rides a job-done
// line instead of a line of its own.
func (s *Store) RearmLastNotified() (string, error) {
	tx, err := s.db.Begin()
	if err != nil {
		return "", fmt.Errorf("rearm notify: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	var id string
	err = tx.QueryRow(`SELECT id FROM jobs WHERE finished_at IS NOT NULL AND notified_at IS NOT NULL AND owner_waits = 0
		AND status NOT IN ('queued','running','stopped') ORDER BY finished_at DESC, rowid DESC LIMIT 1`).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("rearm notify: %w", err)
	}
	if _, err := tx.Exec(`UPDATE jobs SET notified_at = NULL WHERE id = ?`, id); err != nil {
		return "", fmt.Errorf("rearm notify: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return "", fmt.Errorf("rearm notify: %w", err)
	}
	return id, nil
}

// LastIdlePickEvent returns the time of the latest event of any of the kinds (nil when none).
func (s *Store) LastIdlePickEvent(kinds ...string) (*time.Time, error) {
	if len(kinds) == 0 {
		return nil, nil
	}
	q, args := `SELECT MAX(at) FROM idle_pick_events WHERE kind IN (?`+repeatPlaceholders(len(kinds)-1)+`)`, make([]any, 0, len(kinds))
	for _, k := range kinds {
		args = append(args, k)
	}
	var at sql.NullInt64
	if err := s.db.QueryRow(q, args...).Scan(&at); err != nil {
		return nil, fmt.Errorf("last idle pick event: %w", err)
	}
	if !at.Valid {
		return nil, nil
	}
	t := time.Unix(at.Int64, 0)
	return &t, nil
}

// CountIdlePickEvents counts the events of the kinds at or after since.
func (s *Store) CountIdlePickEvents(since time.Time, kinds ...string) (int, error) {
	if len(kinds) == 0 {
		return 0, nil
	}
	args := []any{since.Unix()}
	for _, k := range kinds {
		args = append(args, k)
	}
	var n int
	err := s.db.QueryRow(`SELECT COUNT(*) FROM idle_pick_events WHERE at >= ? AND kind IN (?`+repeatPlaceholders(len(kinds)-1)+`)`, args...).Scan(&n)
	if err != nil {
		return 0, fmt.Errorf("count idle pick events: %w", err)
	}
	return n, nil
}

func repeatPlaceholders(n int) string {
	out := ""
	for i := 0; i < n; i++ {
		out += ", ?"
	}
	return out
}

// CountActive returns the jobs that are not over: queued, running or stopped (a stopped job can be resumed).
func (s *Store) CountActive() (int, error) {
	var n int
	err := s.db.QueryRow(`SELECT COUNT(*) FROM jobs WHERE status IN ('queued','running','stopped')`).Scan(&n)
	return n, err
}
