package store

import (
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// DailyRun is the one row of the daily table: the last local day the daily automation (prune and
// doctor procs inside the daemon tick) was claimed for, when it started and finished, and its
// result as JSON (written by the jobs package).
type DailyRun struct {
	Day        string // local date YYYY-MM-DD the run was claimed for
	StartedAt  time.Time
	FinishedAt *time.Time
	Result     string // JSON; "" until the run finished
}

const dailySchema = `
CREATE TABLE IF NOT EXISTS daily (
	name        TEXT PRIMARY KEY,
	day         TEXT NOT NULL,
	started_at  INTEGER NOT NULL,
	finished_at INTEGER,
	result      TEXT NOT NULL DEFAULT ''
);
`

// dailyName is the key of the only row (one daily automation per agent store).
const dailyName = "daily"

// ClaimDaily records day as the day of the daily run, unless a run was already claimed for it. It
// is atomic (one statement), so two daemons, or a restarted one, never run the same day twice; a
// claim made before a crash still counts (the day is not repeated).
func (s *Store) ClaimDaily(day string, now time.Time) (bool, error) {
	res, err := s.db.Exec(`INSERT INTO daily (name, day, started_at) VALUES (?, ?, ?)
		ON CONFLICT(name) DO UPDATE SET day = excluded.day, started_at = excluded.started_at,
			finished_at = NULL, result = ''
		WHERE daily.day <> excluded.day`, dailyName, day, now.Unix())
	if err != nil {
		return false, fmt.Errorf("claim daily: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("claim daily: %w", err)
	}
	return n == 1, nil
}

// FinishDaily stores the result of the run claimed for day (a later claim is never overwritten).
func (s *Store) FinishDaily(day string, now time.Time, result string) error {
	_, err := s.db.Exec(`UPDATE daily SET finished_at = ?, result = ? WHERE name = ? AND day = ?`,
		now.Unix(), result, dailyName, day)
	if err != nil {
		return fmt.Errorf("finish daily: %w", err)
	}
	return nil
}

// GetDaily returns the last daily run, or nil when none was ever claimed.
func (s *Store) GetDaily() (*DailyRun, error) {
	var d DailyRun
	var started int64
	var finished sql.NullInt64
	err := s.db.QueryRow(`SELECT day, started_at, finished_at, result FROM daily WHERE name = ?`, dailyName).
		Scan(&d.Day, &started, &finished, &d.Result)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("get daily: %w", err)
	}
	d.StartedAt = time.Unix(started, 0)
	d.FinishedAt = convTS(finished)
	return &d, nil
}
