package store

import (
	"database/sql"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"
)

// Idle pick event kinds (#165). Every row is one thing the daemon did about the queue-empty
// transition of an epoch; the rows are the only state, so a restarted daemon sees what the last one did.
const (
	// IdleEval: the transition was evaluated and nothing was said (no ready work).
	IdleEval = "eval"
	// IdleNote: the idle note rode on a job-done delivery.
	IdleNote = "note"
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
CREATE TABLE IF NOT EXISTS idle_pick_delivery (
	id    INTEGER PRIMARY KEY CHECK (id = 1),
	batch TEXT NOT NULL,
	epoch INTEGER NOT NULL,
	note  TEXT NOT NULL
);
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
// recorded, or neither is and the whole line is retried. A saved idle delivery is cleared when its
// batch matches or all of its members have been notified, including through separate acknowledgements.
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
	batch := slices.Clone(ids)
	slices.Sort(batch)
	// Count whole job IDs against the comma-separated batch's member count, so partial or unrelated
	// acknowledgements preserve it and a missing job cannot count as notified.
	if _, err := tx.Exec(`DELETE FROM idle_pick_delivery WHERE batch = ? OR (
		SELECT COUNT(*) FROM jobs WHERE notified_at IS NOT NULL
		AND instr(',' || idle_pick_delivery.batch || ',', ',' || jobs.id || ',') > 0
	) = length(batch) - length(replace(batch, ',', '')) + 1`, strings.Join(batch, ",")); err != nil {
		return fmt.Errorf("clear idle delivery: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("mark notified: %w", err)
	}
	return nil
}

// IdleDelivery is the record of the idle note a job-done delivery is about to type: saved before the
// keystrokes, read back by a retry of the same delivery, deleted with its acknowledgement.
type IdleDelivery struct {
	Batch string // the delivery's job ids, sorted, comma-joined
	Epoch int64
	Note  string
}

// SaveIdleDelivery stores the record of the delivery being typed, replacing any older one (a delivery
// is typed one at a time, so at most one is in flight).
func (s *Store) SaveIdleDelivery(d IdleDelivery) error {
	if _, err := s.db.Exec(`INSERT OR REPLACE INTO idle_pick_delivery (id, batch, epoch, note) VALUES (1, ?, ?, ?)`,
		d.Batch, d.Epoch, d.Note); err != nil {
		return fmt.Errorf("save idle delivery: %w", err)
	}
	return nil
}

// IdleDeliveryFor returns the saved record when it belongs to the delivery of batch (nil: none).
func (s *Store) IdleDeliveryFor(batch string) (*IdleDelivery, error) {
	var d IdleDelivery
	err := s.db.QueryRow(`SELECT batch, epoch, note FROM idle_pick_delivery WHERE id = 1 AND batch = ?`, batch).
		Scan(&d.Batch, &d.Epoch, &d.Note)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read idle delivery: %w", err)
	}
	return &d, nil
}

// migrateIdlePickSeq starts the queue epoch counter on a database that has no counter row yet (one that
// predates idle pick, or has just been created): the counter begins at the number of jobs that already
// ended, and an evaluation event covers all of them, so history is never noted. The exception is a
// completion whose job-done line is still pending: its epoch is left open, so it gets its note on its
// own line after the upgrade. The row and the event are written in one transaction.
func migrateIdlePickSeq(db *sql.DB) error {
	tx, err := db.Begin()
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	res, err := tx.Exec(`INSERT OR IGNORE INTO idle_pick_seq (id, n)
		SELECT 1, COUNT(*) FROM jobs WHERE status NOT IN ('queued','running','stopped')`)
	if err != nil {
		return err
	}
	if inserted, err := res.RowsAffected(); err != nil || inserted == 0 {
		return err
	}
	var covered int64
	if err := tx.QueryRow(`SELECT n FROM idle_pick_seq WHERE id = 1`).Scan(&covered); err != nil {
		return err
	}
	var pending bool
	if err := tx.QueryRow(`SELECT EXISTS (SELECT 1 FROM jobs WHERE status NOT IN ('queued','running','stopped')
		AND notified_at IS NULL)`).Scan(&pending); err != nil {
		return err
	}
	if pending {
		covered--
	}
	if covered > 0 {
		if err := addIdlePickEvent(tx, IdleEval, time.Now(), covered); err != nil {
			return err
		}
	}
	return tx.Commit()
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
