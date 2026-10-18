package store

import (
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// ErrScheduleNotFound is returned when a schedule id does not exist.
var ErrScheduleNotFound = errors.New("schedule not found")

// Schedule is one row of the schedules table: a cron expression in local time and a prompt,
// delivered into the live session, or run as a job when Kind is set (with the model and limits
// resolved and stored at creation). Rows never expire; they are edited in place.
type Schedule struct {
	ID      int64  `json:"id"`
	Cron    string `json:"cron"`
	Prompt  string `json:"prompt"`
	Enabled bool   `json:"enabled"`
	// Kind is "" for delivery into the session, else the job kind the schedule starts.
	Kind            string  `json:"kind,omitempty"`
	Title           string  `json:"title,omitempty"`
	Model           string  `json:"model,omitempty"`
	Complexity      string  `json:"complexity,omitempty"`
	Runner          string  `json:"runner,omitempty"`
	ReasoningEffort string  `json:"reasoning_effort,omitempty"`
	MaxTurns        int     `json:"max_turns,omitempty"`
	MaxBudgetUSD    float64 `json:"max_budget_usd,omitempty"`
	TimeoutMinutes  int     `json:"timeout_minutes,omitempty"`
	RepoPath        string  `json:"repo,omitempty"`
	// Base is the ref resolved in RepoPath at each firing ("" = HEAD).
	Base      string    `json:"base,omitempty"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
	// LastFiredAt is the minute the schedule last fired (claimed), nil when never.
	LastFiredAt *time.Time `json:"last_fired_at,omitempty"`
	// LastFiredLocal is that minute on the local wall clock, "YYYY-MM-DD HH:MM": the watermark a
	// later firing's local minute must exceed (see ClaimScheduleFiring).
	LastFiredLocal string `json:"-"`
	// LastJobID is the job the last firing started (job schedules).
	LastJobID string `json:"last_job_id,omitempty"`
	// LastError is why the last firing was not delivered ("" = delivered).
	LastError string `json:"last_error,omitempty"`
}

const schedulesSchema = `
CREATE TABLE IF NOT EXISTS schedules (
	id               INTEGER PRIMARY KEY AUTOINCREMENT,
	cron             TEXT NOT NULL,
	prompt           TEXT NOT NULL,
	enabled          INTEGER NOT NULL DEFAULT 1,
	kind             TEXT NOT NULL DEFAULT '',
	title            TEXT NOT NULL DEFAULT '',
	model            TEXT NOT NULL DEFAULT '',
	complexity       TEXT NOT NULL DEFAULT '',
	runner           TEXT NOT NULL DEFAULT '',
	reasoning_effort TEXT NOT NULL DEFAULT '',
	max_turns        INTEGER NOT NULL DEFAULT 0,
	max_budget_usd   REAL NOT NULL DEFAULT 0,
	timeout_minutes  INTEGER NOT NULL DEFAULT 0,
	repo_path        TEXT NOT NULL DEFAULT '',
	base             TEXT NOT NULL DEFAULT '',
	created_at       INTEGER NOT NULL,
	updated_at       INTEGER NOT NULL,
	last_fired_at    INTEGER,
	last_fired_local TEXT NOT NULL DEFAULT '',
	last_job_id      TEXT NOT NULL DEFAULT '',
	last_error       TEXT NOT NULL DEFAULT ''
);
`

const scheduleCols = `id, cron, prompt, enabled, kind, title, model, complexity, runner, reasoning_effort,
	max_turns, max_budget_usd, timeout_minutes, repo_path, base, created_at, updated_at,
	last_fired_at, last_fired_local, last_job_id, last_error`

// AddSchedule inserts s (enabled as given) and returns it with its id. Validation is the caller's.
func (s *Store) AddSchedule(sc *Schedule, now time.Time) (*Schedule, error) {
	res, err := s.db.Exec(`INSERT INTO schedules (cron, prompt, enabled, kind, title, model, complexity, runner,
		reasoning_effort, max_turns, max_budget_usd, timeout_minutes, repo_path, base, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		sc.Cron, sc.Prompt, sc.Enabled, sc.Kind, sc.Title, sc.Model, sc.Complexity, sc.Runner, sc.ReasoningEffort,
		sc.MaxTurns, sc.MaxBudgetUSD, sc.TimeoutMinutes, sc.RepoPath, sc.Base, now.Unix(), now.Unix())
	if err != nil {
		return nil, fmt.Errorf("add schedule: %w", err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		return nil, fmt.Errorf("add schedule: %w", err)
	}
	return s.GetSchedule(id)
}

func scanSchedule(r scanner) (*Schedule, error) {
	var sc Schedule
	var created, updated int64
	var fired sql.NullInt64
	if err := r.Scan(&sc.ID, &sc.Cron, &sc.Prompt, &sc.Enabled, &sc.Kind, &sc.Title, &sc.Model, &sc.Complexity,
		&sc.Runner, &sc.ReasoningEffort, &sc.MaxTurns, &sc.MaxBudgetUSD, &sc.TimeoutMinutes, &sc.RepoPath, &sc.Base,
		&created, &updated, &fired, &sc.LastFiredLocal, &sc.LastJobID, &sc.LastError); err != nil {
		return nil, err
	}
	sc.CreatedAt, sc.UpdatedAt = time.Unix(created, 0), time.Unix(updated, 0)
	sc.LastFiredAt = convTS(fired)
	return &sc, nil
}

// GetSchedule returns one schedule (ErrScheduleNotFound when there is none).
func (s *Store) GetSchedule(id int64) (*Schedule, error) {
	sc, err := scanSchedule(s.db.QueryRow(`SELECT `+scheduleCols+` FROM schedules WHERE id = ?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrScheduleNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("get schedule: %w", err)
	}
	return sc, nil
}

// ListSchedules returns every schedule (only the enabled ones with enabledOnly), oldest first.
func (s *Store) ListSchedules(enabledOnly bool) ([]*Schedule, error) {
	q := `SELECT ` + scheduleCols + ` FROM schedules ORDER BY id`
	if enabledOnly {
		q = `SELECT ` + scheduleCols + ` FROM schedules WHERE enabled = 1 ORDER BY id`
	}
	rows, err := s.db.Query(q)
	if err != nil {
		return nil, fmt.Errorf("list schedules: %w", err)
	}
	defer rows.Close()
	out := []*Schedule{}
	for rows.Next() {
		sc, err := scanSchedule(rows)
		if err != nil {
			return nil, fmt.Errorf("scan schedule: %w", err)
		}
		out = append(out, sc)
	}
	return out, rows.Err()
}

// SetSchedule edits a schedule in place: a non-empty cron or prompt replaces the old one, a
// non-nil enabled sets the flag.
func (s *Store) SetSchedule(id int64, cron, prompt string, enabled *bool, now time.Time) (*Schedule, error) {
	en := sql.NullBool{}
	if enabled != nil {
		en = sql.NullBool{Bool: *enabled, Valid: true}
	}
	res, err := s.db.Exec(`UPDATE schedules SET
		cron       = CASE WHEN ?1 = '' THEN cron ELSE ?1 END,
		prompt     = CASE WHEN ?2 = '' THEN prompt ELSE ?2 END,
		enabled    = COALESCE(?3, enabled),
		updated_at = ?4
		WHERE id = ?5`, cron, prompt, en, now.Unix(), id)
	if err != nil {
		return nil, fmt.Errorf("set schedule: %w", err)
	}
	if n, err := res.RowsAffected(); err != nil {
		return nil, fmt.Errorf("set schedule: %w", err)
	} else if n == 0 {
		return nil, ErrScheduleNotFound
	}
	return s.GetSchedule(id)
}

// RemoveSchedule deletes a schedule (ErrScheduleNotFound when there is none).
func (s *Store) RemoveSchedule(id int64) error {
	res, err := s.db.Exec(`DELETE FROM schedules WHERE id = ?`, id)
	if err != nil {
		return fmt.Errorf("remove schedule: %w", err)
	}
	if n, err := res.RowsAffected(); err != nil {
		return fmt.Errorf("remove schedule: %w", err)
	} else if n == 0 {
		return ErrScheduleNotFound
	}
	return nil
}

// ClaimScheduleFiring records minute (truncated to the minute) and local, its wall-clock minute
// "YYYY-MM-DD HH:MM", as the schedule's last firing, only when both are strictly later than the
// last firing's: the absolute time and the local minute (compared as strings, the layout sorts) are
// increasing watermarks. In the hour repeated when DST ends the absolute time goes on while the
// local minute goes back, so nothing fires again until the wall clock passes the last firing; a
// clock stepped back never re-fires either. One atomic statement: two daemons, or a restarted one,
// never fire a schedule twice; a claim made before a crash still counts (a lost firing, never a
// repeated one).
func (s *Store) ClaimScheduleFiring(id int64, minute time.Time, local string) (bool, error) {
	res, err := s.db.Exec(`UPDATE schedules SET last_fired_at = ?1, last_fired_local = ?2, last_job_id = '', last_error = ''
		WHERE id = ?3 AND enabled = 1 AND (last_fired_at IS NULL OR last_fired_at < ?1) AND last_fired_local < ?2`,
		minute.Unix(), local, id)
	if err != nil {
		return false, fmt.Errorf("claim schedule: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("claim schedule: %w", err)
	}
	return n == 1, nil
}

// RecordScheduleFiring stores the outcome of the firing claimed for minute: the job it started
// and/or why it was not delivered (a later firing is never overwritten).
func (s *Store) RecordScheduleFiring(id int64, minute time.Time, jobID, errText string) error {
	_, err := s.db.Exec(`UPDATE schedules SET last_job_id = ?, last_error = ? WHERE id = ? AND last_fired_at = ?`,
		jobID, errText, id, minute.Unix())
	if err != nil {
		return fmt.Errorf("record schedule firing: %w", err)
	}
	return nil
}
