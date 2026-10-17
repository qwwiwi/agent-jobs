// Package store keeps the jobs table in SQLite (one database per agent).
package store

import (
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	_ "modernc.org/sqlite" // pure-Go SQLite driver, no cgo
)

// Job statuses. Only queued and running are mutable.
const (
	StatusQueued    = "queued"
	StatusRunning   = "running"
	StatusSucceeded = "succeeded"
	StatusFailed    = "failed"
	StatusTimeout   = "timeout"
	StatusCancelled = "cancelled"
)

// ErrNotFound is returned when a job id does not exist.
var ErrNotFound = errors.New("job not found")

// ErrImmutable is returned when trying to change a job in a terminal status.
var ErrImmutable = errors.New("job status is final and cannot change")

// Job is one row of the jobs table.
type Job struct {
	ID             string
	Agent          string
	Title          string
	Kind           string
	Brief          string
	RepoPath       string
	BaseCommit     string
	Status         string
	Model          string
	MaxTurns       int
	MaxBudgetUSD   float64
	TimeoutMinutes int
	PID            int
	PGID           int
	CreatedAt      time.Time
	StartedAt      *time.Time
	FinishedAt     *time.Time
	NotifiedAt     *time.Time
	CleanedAt      *time.Time
	Turns          int
	CostUSD        float64
	DurationSec    int
	Error          string
	// LaunchToken identifies the tick that claimed the job; only that launch may finish it.
	LaunchToken string
	// Complexity is the stated light|normal|hard (picks the model when none was given), empty when only a model was given.
	Complexity string
	// Runner is the program that runs the job ("claude"; "codex" arrives with S3-M1).
	Runner string
	// OwnerWaits marks a job the owner asked for (the Stop-hook that enforces it is S3-M5).
	OwnerWaits bool
	// ResumedFrom is the id of the job this one continues (S2-M5), empty otherwise.
	ResumedFrom string
	// ScheduleID is the schedule that created the job (S2-M4), empty otherwise.
	ScheduleID string
	// Billing is how the run is paid: "subscription" (default) or "api" (S3-M6 fallback).
	Billing string
}

// Runners and billing modes.
const (
	RunnerClaude        = "claude"
	BillingSubscription = "subscription"
)

// Store wraps the SQLite handle.
type Store struct {
	db *sql.DB
}

const schema = `
CREATE TABLE IF NOT EXISTS jobs (
	id              TEXT PRIMARY KEY,
	agent           TEXT NOT NULL,
	title           TEXT NOT NULL,
	kind            TEXT NOT NULL CHECK (kind IN ('review','fix','research','general')),
	brief           TEXT NOT NULL,
	repo_path       TEXT NOT NULL DEFAULT '',
	base_commit     TEXT NOT NULL DEFAULT '',
	status          TEXT NOT NULL CHECK (status IN ('queued','running','succeeded','failed','timeout','cancelled')),
	model           TEXT NOT NULL,
	max_turns       INTEGER NOT NULL,
	max_budget_usd  REAL NOT NULL,
	timeout_minutes INTEGER NOT NULL,
	pid             INTEGER NOT NULL DEFAULT 0,
	pgid            INTEGER NOT NULL DEFAULT 0,
	created_at      INTEGER NOT NULL,
	started_at      INTEGER,
	finished_at     INTEGER,
	notified_at     INTEGER,
	cleaned_at      INTEGER,
	turns           INTEGER NOT NULL DEFAULT 0,
	cost_usd        REAL NOT NULL DEFAULT 0,
	duration_sec    INTEGER NOT NULL DEFAULT 0,
	error           TEXT NOT NULL DEFAULT '',
	launch_token    TEXT NOT NULL DEFAULT ''
);
CREATE INDEX IF NOT EXISTS jobs_status ON jobs(status);
`

// Open opens (and migrates) the database at path.
func Open(path string) (*Store, error) {
	dsn := fmt.Sprintf("file:%s?_pragma=busy_timeout(10000)&_pragma=journal_mode(WAL)", path)
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("open db: %w", err)
	}
	db.SetMaxOpenConns(1)
	if _, err := db.Exec(schema + statsSchema); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("migrate db: %w", err)
	}
	// Tolerant migrations: databases created before a column existed get it with its default;
	// "duplicate column" means the column is already there.
	for _, alter := range migrations {
		if _, err := db.Exec(alter); err != nil && !strings.Contains(err.Error(), "duplicate column") {
			_ = db.Close()
			return nil, fmt.Errorf("migrate db: %w", err)
		}
	}
	return &Store{db: db}, nil
}

// migrations add columns introduced after the first schema, in order. Never edit or reorder a row.
var migrations = []string{
	`ALTER TABLE jobs ADD COLUMN launch_token TEXT NOT NULL DEFAULT ''`,
	`ALTER TABLE jobs ADD COLUMN complexity TEXT NOT NULL DEFAULT ''`,
	`ALTER TABLE jobs ADD COLUMN runner TEXT NOT NULL DEFAULT 'claude'`,
	`ALTER TABLE jobs ADD COLUMN owner_waits INTEGER NOT NULL DEFAULT 0`,
	`ALTER TABLE jobs ADD COLUMN resumed_from TEXT NOT NULL DEFAULT ''`,
	`ALTER TABLE jobs ADD COLUMN schedule_id TEXT NOT NULL DEFAULT ''`,
	`ALTER TABLE jobs ADD COLUMN billing TEXT NOT NULL DEFAULT 'subscription'`,
	`ALTER TABLE jobs ADD COLUMN subagents INTEGER NOT NULL DEFAULT 0`,
}

// ReserveSubagent counts one more subagent for running job id unless max are already used. It
// returns the count before this call and whether the call is allowed. The count only ever grows
// (one UPDATE, guarded by the WHERE clause), so it is the job's monotonic subagent counter: the
// guard compares its own log with it and refuses when the log was shortened.
func (s *Store) ReserveSubagent(id string, max int) (prev int, allowed bool, err error) {
	var cur int
	err = s.db.QueryRow(`UPDATE jobs SET subagents = subagents + 1 WHERE id = ? AND status = ? AND subagents < ?
		RETURNING subagents`, id, StatusRunning, max).Scan(&cur)
	if err == nil {
		return cur - 1, true, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return 0, false, fmt.Errorf("reserve subagent: %w", err)
	}
	var status string
	if err := s.db.QueryRow(`SELECT subagents, status FROM jobs WHERE id = ?`, id).Scan(&cur, &status); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return 0, false, ErrNotFound
		}
		return 0, false, fmt.Errorf("reserve subagent: %w", err)
	}
	if status != StatusRunning {
		return cur, false, fmt.Errorf("job %s is %s, not running", id, status)
	}
	return cur, false, nil
}

// SubagentCount returns how many subagents job id has used.
func (s *Store) SubagentCount(id string) (int, error) {
	var n int
	err := s.db.QueryRow(`SELECT subagents FROM jobs WHERE id = ?`, id).Scan(&n)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, ErrNotFound
	}
	return n, err
}

// Close closes the database.
func (s *Store) Close() error { return s.db.Close() }

const cols = `id, agent, title, kind, brief, repo_path, base_commit, status, model,
	max_turns, max_budget_usd, timeout_minutes, pid, pgid, created_at, started_at,
	finished_at, notified_at, cleaned_at, turns, cost_usd, duration_sec, error, launch_token,
	complexity, runner, owner_waits, resumed_from, schedule_id, billing`

func ts(t *time.Time) any {
	if t == nil {
		return nil
	}
	return t.Unix()
}

// Insert adds a new job row. Empty Runner / Billing get their defaults.
func (s *Store) Insert(j *Job) error {
	if j.Runner == "" {
		j.Runner = RunnerClaude
	}
	if j.Billing == "" {
		j.Billing = BillingSubscription
	}
	_, err := s.db.Exec(`INSERT INTO jobs (`+cols+`) VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		j.ID, j.Agent, j.Title, j.Kind, j.Brief, j.RepoPath, j.BaseCommit, j.Status, j.Model,
		j.MaxTurns, j.MaxBudgetUSD, j.TimeoutMinutes, j.PID, j.PGID, j.CreatedAt.Unix(),
		ts(j.StartedAt), ts(j.FinishedAt), ts(j.NotifiedAt), ts(j.CleanedAt),
		j.Turns, j.CostUSD, j.DurationSec, j.Error, j.LaunchToken,
		j.Complexity, j.Runner, j.OwnerWaits, j.ResumedFrom, j.ScheduleID, j.Billing)
	if err != nil {
		return fmt.Errorf("insert job: %w", err)
	}
	return nil
}

type scanner interface{ Scan(dest ...any) error }

func scanJob(r scanner) (*Job, error) {
	var j Job
	var created int64
	var started, finished, notified, cleaned sql.NullInt64
	err := r.Scan(&j.ID, &j.Agent, &j.Title, &j.Kind, &j.Brief, &j.RepoPath, &j.BaseCommit, &j.Status,
		&j.Model, &j.MaxTurns, &j.MaxBudgetUSD, &j.TimeoutMinutes, &j.PID, &j.PGID, &created,
		&started, &finished, &notified, &cleaned, &j.Turns, &j.CostUSD, &j.DurationSec, &j.Error, &j.LaunchToken,
		&j.Complexity, &j.Runner, &j.OwnerWaits, &j.ResumedFrom, &j.ScheduleID, &j.Billing)
	if err != nil {
		return nil, err
	}
	j.CreatedAt = time.Unix(created, 0)
	conv := func(n sql.NullInt64) *time.Time {
		if !n.Valid {
			return nil
		}
		t := time.Unix(n.Int64, 0)
		return &t
	}
	j.StartedAt, j.FinishedAt, j.NotifiedAt, j.CleanedAt = conv(started), conv(finished), conv(notified), conv(cleaned)
	return &j, nil
}

// Get returns one job by id.
func (s *Store) Get(id string) (*Job, error) {
	j, err := scanJob(s.db.QueryRow(`SELECT `+cols+` FROM jobs WHERE id = ?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("get job: %w", err)
	}
	return j, nil
}

func (s *Store) query(q string, args ...any) ([]*Job, error) {
	rows, err := s.db.Query(`SELECT `+cols+` FROM jobs `+q, args...)
	if err != nil {
		return nil, fmt.Errorf("query jobs: %w", err)
	}
	defer rows.Close()
	var out []*Job
	for rows.Next() {
		j, err := scanJob(rows)
		if err != nil {
			return nil, fmt.Errorf("scan job: %w", err)
		}
		out = append(out, j)
	}
	return out, rows.Err()
}

// ListByStatus returns jobs with the given status, oldest first.
func (s *Store) ListByStatus(status string) ([]*Job, error) {
	return s.query(`WHERE status = ? ORDER BY created_at, rowid`, status)
}

// List returns the most recent jobs, newest first.
func (s *Store) List(limit int) ([]*Job, error) {
	return s.query(`ORDER BY created_at DESC, rowid DESC LIMIT ?`, limit)
}

// ListRecentByStatus returns the most recent jobs with the given status, newest first.
func (s *Store) ListRecentByStatus(status string, limit int) ([]*Job, error) {
	return s.query(`WHERE status = ? ORDER BY created_at DESC, rowid DESC LIMIT ?`, status, limit)
}

// ListActivity returns jobs created or finished at or after since, plus every queued and running
// job, newest first, at most limit rows.
func (s *Store) ListActivity(since time.Time, limit int) ([]*Job, error) {
	return s.query(`WHERE created_at >= ? OR finished_at >= ? OR status IN ('queued','running')
		ORDER BY created_at DESC, rowid DESC LIMIT ?`, since.Unix(), since.Unix(), limit)
}

// PendingCleanup returns finished, notified, not yet cleaned jobs.
func (s *Store) PendingCleanup() ([]*Job, error) {
	return s.query(`WHERE status NOT IN ('queued','running') AND notified_at IS NOT NULL AND cleaned_at IS NULL ORDER BY finished_at`)
}

// PendingNotify returns finished jobs whose notification was not sent yet.
func (s *Store) PendingNotify() ([]*Job, error) {
	return s.query(`WHERE status NOT IN ('queued','running') AND notified_at IS NULL ORDER BY finished_at`)
}

// CountRunning returns the number of running jobs.
func (s *Store) CountRunning() (int, error) {
	var n int
	err := s.db.QueryRow(`SELECT COUNT(*) FROM jobs WHERE status = 'running'`).Scan(&n)
	return n, err
}

func affected(res sql.Result, err error) error {
	if err != nil {
		return err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n == 0 {
		return ErrImmutable
	}
	return nil
}

// Claim atomically moves a queued job to running under a launch token, BEFORE anything is
// spawned. Exactly one claimer wins; the others get ErrImmutable.
func (s *Store) Claim(id, token string, now time.Time) error {
	if token == "" {
		return errors.New("claim: empty launch token")
	}
	return affected(s.db.Exec(`UPDATE jobs SET status='running', pid=0, pgid=0, launch_token=?, started_at=?
		WHERE id=? AND status='queued'`, token, now.Unix(), id))
}

// SetPID records the wrapper's pid/pgid for the launch that owns the job.
func (s *Store) SetPID(id, token string, pid, pgid int) error {
	return affected(s.db.Exec(`UPDATE jobs SET pid=?, pgid=? WHERE id=? AND status='running' AND launch_token=?`,
		pid, pgid, id, token))
}

// Finish moves a job to a terminal status. With a launch token, only the running job owned by
// that launch can be finished (a tick never finishes another tick's job); with an empty token
// (explicit cancel) any queued or running job can. Terminal rows never change status again.
func (s *Store) Finish(id, token, status string, turns int, cost float64, durationSec int, errMsg string, now time.Time) error {
	if status == StatusQueued || status == StatusRunning {
		return fmt.Errorf("finish: %q is not a terminal status", status)
	}
	q := `UPDATE jobs SET status=?, turns=?, cost_usd=?, duration_sec=?, error=?, finished_at=? WHERE id=? AND `
	args := []any{status, turns, cost, durationSec, errMsg, now.Unix(), id}
	if token == "" {
		q += `status IN ('queued','running')`
	} else {
		q += `status='running' AND launch_token=?`
		args = append(args, token)
	}
	return affected(s.db.Exec(q, args...))
}

// MarkNotified records that the job-done line was delivered.
func (s *Store) MarkNotified(id string, now time.Time) error {
	_, err := s.db.Exec(`UPDATE jobs SET notified_at=? WHERE id=?`, now.Unix(), id)
	return err
}

// MarkCleaned records that the worktree cleanup ran.
func (s *Store) MarkCleaned(id string, now time.Time) error {
	_, err := s.db.Exec(`UPDATE jobs SET cleaned_at=? WHERE id=?`, now.Unix(), id)
	return err
}
