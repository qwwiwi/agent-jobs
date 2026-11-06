package store

import (
	"database/sql"
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
	// IdleRecovery: the recovery scan delivered an idle line of its own.
	IdleRecovery = "recovery"
	// IdleSuppressed: ready work was found but the cooldown or the daily cap held the note back; the
	// epoch stays unevaluated, so the recovery scan looks at it again.
	IdleSuppressed = "suppressed"
	// IdleScan: a recovery scan ran (whatever it found); spaces the scans.
	IdleScan = "scan"
)

// idlePickSchema is applied on every Open (CREATE … IF NOT EXISTS): an older database gains the table.
const idlePickSchema = `
CREATE TABLE IF NOT EXISTS idle_pick_events (
	id   INTEGER PRIMARY KEY AUTOINCREMENT,
	at   INTEGER NOT NULL,
	kind TEXT NOT NULL
);
CREATE INDEX IF NOT EXISTS idle_pick_events_at ON idle_pick_events(at);
`

// idlePickKeep is how long events are kept: the daily cap looks back a day, the rest a few scans.
const idlePickKeep = 7 * 24 * time.Hour

// AddIdlePickEvent records one event at now and drops the events older than a week.
func (s *Store) AddIdlePickEvent(kind string, now time.Time) error {
	if _, err := s.db.Exec(`INSERT INTO idle_pick_events (at, kind) VALUES (?, ?)`, now.Unix(), kind); err != nil {
		return fmt.Errorf("record idle pick event: %w", err)
	}
	if _, err := s.db.Exec(`DELETE FROM idle_pick_events WHERE at < ?`, now.Add(-idlePickKeep).Unix()); err != nil {
		return fmt.Errorf("prune idle pick events: %w", err)
	}
	return nil
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

// LastFinishedAt returns when the latest job finished (nil when none did).
func (s *Store) LastFinishedAt() (*time.Time, error) {
	var at sql.NullInt64
	if err := s.db.QueryRow(`SELECT MAX(finished_at) FROM jobs WHERE finished_at IS NOT NULL`).Scan(&at); err != nil {
		return nil, fmt.Errorf("last finished job: %w", err)
	}
	if !at.Valid {
		return nil, nil
	}
	t := time.Unix(at.Int64, 0)
	return &t, nil
}
