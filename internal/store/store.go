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

// Job statuses. Only queued and running are mutable; stopped, timeout and failed can be resumed
// (Requeue), which starts a new attempt of the same job; stopped can also be cancelled.
const (
	StatusQueued    = "queued"
	StatusRunning   = "running"
	StatusSucceeded = "succeeded"
	StatusFailed    = "failed"
	StatusTimeout   = "timeout"
	StatusCancelled = "cancelled"
	// StatusStopped is a job the orchestrator stopped on purpose (job_stop): its work dir and session
	// are kept, it is neither notified nor cleaned up, and it waits for job_resume or job_cancel.
	StatusStopped = "stopped"
)

// Run modes of an attempt: a new session, the same session resumed, or a fresh session that falls
// back to the brief plus a summary of the previous attempt (the session could not be resumed).
const (
	RunNew      = "new"
	RunResume   = "resume"
	RunFallback = "fallback"
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
	// SessionID is the claude session id assigned before launch (--session-id), so the job can be
	// resumed in the same session (--resume). Empty for rows from before session resume.
	SessionID string
	// Attempt counts launches of this job: 1 for the first run, +1 for each resume.
	Attempt int
	// RunMode is how the current attempt runs: RunNew, RunResume or RunFallback.
	RunMode string
	// PendingContext is the orchestrator update the current (or next) attempt was resumed with.
	PendingContext string
	// CostUnknown marks an attempt that ran claude but ended without its result event (stopped or
	// killed): CostUSD is then 0 and the attempt counts at its whole budget against the limits.
	CostUnknown bool
	// SessionDigest is the digest of the session transcript recorded when the attempt ended; a resume
	// continues the session only while the transcript still has it.
	SessionDigest string
	// ScopeUnit is the transient systemd user scope the wrapper was launched in
	// (agent-jobs-<agent>-<id>.scope), empty when the job runs without one (session containment only).
	ScopeUnit string
	// ScopePending is set when stopping the job's scope failed: the row is terminal, but processes of
	// the job may live on; every tick retries the stop until the unit is gone.
	ScopePending bool
}

// Attempt is one earlier attempt of a resumed job (the current attempt is the job row itself).
type Attempt struct {
	JobID          string
	N              int
	Mode           string
	Status         string
	Model          string
	MaxTurns       int
	MaxBudgetUSD   float64
	TimeoutMinutes int
	Turns          int
	CostUSD        float64
	CostUnknown    bool
	DurationSec    int
	Error          string
	Context        string
	SessionID      string
	StartedAt      *time.Time
	FinishedAt     *time.Time
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
	status          TEXT NOT NULL CHECK (status IN ('queued','running','succeeded','failed','timeout','cancelled','stopped')),
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
CREATE TABLE IF NOT EXISTS attempts (
	job_id          TEXT NOT NULL,
	n               INTEGER NOT NULL,
	mode            TEXT NOT NULL,
	status          TEXT NOT NULL,
	model           TEXT NOT NULL,
	max_turns       INTEGER NOT NULL,
	max_budget_usd  REAL NOT NULL,
	timeout_minutes INTEGER NOT NULL,
	turns           INTEGER NOT NULL,
	cost_usd        REAL NOT NULL,
	duration_sec    INTEGER NOT NULL,
	error           TEXT NOT NULL,
	context         TEXT NOT NULL,
	session_id      TEXT NOT NULL,
	started_at      INTEGER,
	finished_at     INTEGER,
	PRIMARY KEY (job_id, n)
);
CREATE TABLE IF NOT EXISTS job_notes (
	path       TEXT PRIMARY KEY,
	job_id     TEXT NOT NULL,
	created_at INTEGER NOT NULL
);
`

// Open opens (and migrates) the database at path.
func Open(path string) (*Store, error) {
	dsn := fmt.Sprintf("file:%s?_pragma=busy_timeout(10000)&_pragma=journal_mode(WAL)", path)
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("open db: %w", err)
	}
	db.SetMaxOpenConns(1)
	if _, err := db.Exec(schema + statsSchema + dailySchema); err != nil {
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
	if err := migrateStatusCheck(db); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("migrate db: %w", err)
	}
	return &Store{db: db}, nil
}

// statusCheckOld / statusCheckNew are the status CHECK of the jobs table before and after the
// stopped status. SQLite cannot alter a CHECK, so migrateStatusCheck rebuilds the table once.
const (
	statusCheckOld = "'timeout','cancelled'))"
	statusCheckNew = "'timeout','cancelled','stopped'))"
)

// migrateStatusCheck rebuilds a jobs table whose status CHECK predates the stopped status: same
// definition (all columns, in order) with the new CHECK, rows copied, index recreated, all in one
// IMMEDIATE transaction that re-checks the definition (several processes open the database).
func migrateStatusCheck(db *sql.DB) error {
	def := func() (string, error) {
		var q string
		err := db.QueryRow(`SELECT sql FROM sqlite_master WHERE type='table' AND name='jobs'`).Scan(&q)
		return q, err
	}
	q, err := def()
	if err != nil || strings.Contains(q, statusCheckNew) {
		return err
	}
	// One connection (SetMaxOpenConns(1)): the statements below run inside this transaction.
	if _, err := db.Exec(`BEGIN IMMEDIATE`); err != nil {
		return err
	}
	rollback := func(err error) error {
		_, _ = db.Exec(`ROLLBACK`)
		return err
	}
	if q, err = def(); err != nil {
		return rollback(err)
	}
	if strings.Contains(q, statusCheckNew) {
		return rollback(nil)
	}
	if strings.Count(q, statusCheckOld) != 1 {
		return rollback(fmt.Errorf("jobs table: unexpected status check in %q", q))
	}
	head, rest, ok := strings.Cut(q, "(")
	if !ok || !strings.Contains(head, "jobs") {
		return rollback(fmt.Errorf("jobs table: unexpected definition %q", q))
	}
	newDef := "CREATE TABLE jobs_rebuild (" + strings.Replace(rest, statusCheckOld, statusCheckNew, 1)
	for _, stmt := range []string{newDef, `INSERT INTO jobs_rebuild SELECT * FROM jobs`, `DROP TABLE jobs`,
		`ALTER TABLE jobs_rebuild RENAME TO jobs`, `CREATE INDEX IF NOT EXISTS jobs_status ON jobs(status)`} {
		if _, err := db.Exec(stmt); err != nil {
			return rollback(fmt.Errorf("rebuild jobs table: %w", err))
		}
	}
	_, err = db.Exec(`COMMIT`)
	return err
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
	`ALTER TABLE jobs ADD COLUMN scope_unit TEXT NOT NULL DEFAULT ''`,
	`ALTER TABLE jobs ADD COLUMN scope_pending INTEGER NOT NULL DEFAULT 0`,
	`ALTER TABLE jobs ADD COLUMN session_id TEXT NOT NULL DEFAULT ''`,
	`ALTER TABLE jobs ADD COLUMN attempt INTEGER NOT NULL DEFAULT 0`,
	`ALTER TABLE jobs ADD COLUMN run_mode TEXT NOT NULL DEFAULT ''`,
	`ALTER TABLE jobs ADD COLUMN pending_context TEXT NOT NULL DEFAULT ''`,
	`ALTER TABLE jobs ADD COLUMN cost_unknown INTEGER NOT NULL DEFAULT 0`,
	`ALTER TABLE jobs ADD COLUMN session_digest TEXT NOT NULL DEFAULT ''`,
	`ALTER TABLE attempts ADD COLUMN cost_unknown INTEGER NOT NULL DEFAULT 0`,
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

// AddJobNote records that job jobID wrote the shared-memory note at path (written through the
// daemon's brain proxy), so the note is never taken for the agent's own.
func (s *Store) AddJobNote(path, jobID string, now time.Time) error {
	if _, err := s.db.Exec(`INSERT INTO job_notes(path, job_id, created_at) VALUES(?, ?, ?)
		ON CONFLICT(path) DO NOTHING`, path, jobID, now.Unix()); err != nil {
		return fmt.Errorf("record job note: %w", err)
	}
	return nil
}

// DeleteJobNote forgets that a job wrote the note at path (the orchestrator wrote it again).
func (s *Store) DeleteJobNote(path string) error {
	if _, err := s.db.Exec(`DELETE FROM job_notes WHERE path = ?`, path); err != nil {
		return fmt.Errorf("forget job note: %w", err)
	}
	return nil
}

// JobNotes returns, for those of paths a job wrote, the job's id by path.
func (s *Store) JobNotes(paths []string) (map[string]string, error) {
	out := map[string]string{}
	for _, p := range paths {
		var id string
		err := s.db.QueryRow(`SELECT job_id FROM job_notes WHERE path = ?`, p).Scan(&id)
		if errors.Is(err, sql.ErrNoRows) {
			continue
		}
		if err != nil {
			return nil, fmt.Errorf("job notes: %w", err)
		}
		out[p] = id
	}
	return out, nil
}

// Close closes the database.
func (s *Store) Close() error { return s.db.Close() }

const cols = `id, agent, title, kind, brief, repo_path, base_commit, status, model,
	max_turns, max_budget_usd, timeout_minutes, pid, pgid, created_at, started_at,
	finished_at, notified_at, cleaned_at, turns, cost_usd, duration_sec, error, launch_token,
	complexity, runner, owner_waits, resumed_from, schedule_id, billing, session_id, attempt, run_mode,
	pending_context, cost_unknown, session_digest, scope_unit, scope_pending`

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
	_, err := s.db.Exec(`INSERT INTO jobs (`+cols+`) VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		j.ID, j.Agent, j.Title, j.Kind, j.Brief, j.RepoPath, j.BaseCommit, j.Status, j.Model,
		j.MaxTurns, j.MaxBudgetUSD, j.TimeoutMinutes, j.PID, j.PGID, j.CreatedAt.Unix(),
		ts(j.StartedAt), ts(j.FinishedAt), ts(j.NotifiedAt), ts(j.CleanedAt),
		j.Turns, j.CostUSD, j.DurationSec, j.Error, j.LaunchToken,
		j.Complexity, j.Runner, j.OwnerWaits, j.ResumedFrom, j.ScheduleID, j.Billing, j.SessionID, j.Attempt,
		j.RunMode, j.PendingContext, j.CostUnknown, j.SessionDigest, j.ScopeUnit, j.ScopePending)
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
		&j.Complexity, &j.Runner, &j.OwnerWaits, &j.ResumedFrom, &j.ScheduleID, &j.Billing, &j.SessionID, &j.Attempt,
		&j.RunMode, &j.PendingContext, &j.CostUnknown, &j.SessionDigest, &j.ScopeUnit, &j.ScopePending)
	if err != nil {
		return nil, err
	}
	j.CreatedAt = time.Unix(created, 0)
	j.StartedAt, j.FinishedAt, j.NotifiedAt, j.CleanedAt = convTS(started), convTS(finished), convTS(notified), convTS(cleaned)
	return &j, nil
}

func convTS(n sql.NullInt64) *time.Time {
	if !n.Valid {
		return nil
	}
	t := time.Unix(n.Int64, 0)
	return &t
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
	return s.query(`WHERE created_at >= ? OR finished_at >= ? OR status IN ('queued','running','stopped')
		ORDER BY created_at DESC, rowid DESC LIMIT ?`, since.Unix(), since.Unix(), limit)
}

// PendingCleanup returns finished, notified, not yet cleaned jobs (a stopped job keeps its work dir).
func (s *Store) PendingCleanup() ([]*Job, error) {
	return s.query(`WHERE status NOT IN ('queued','running','stopped') AND notified_at IS NOT NULL AND cleaned_at IS NULL ORDER BY finished_at`)
}

// PendingNotify returns finished jobs whose notification was not sent yet. A stopped job is not
// notified: the orchestrator stopped it itself.
func (s *Store) PendingNotify() ([]*Job, error) {
	return s.query(`WHERE status NOT IN ('queued','running','stopped') AND notified_at IS NULL ORDER BY finished_at`)
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
// spawned, and counts the attempt. Exactly one claimer wins; the others get ErrImmutable.
func (s *Store) Claim(id, token string, now time.Time) error {
	if token == "" {
		return errors.New("claim: empty launch token")
	}
	return affected(s.db.Exec(`UPDATE jobs SET status='running', pid=0, pgid=0, launch_token=?, started_at=?,
		attempt=attempt+1 WHERE id=? AND status='queued'`, token, now.Unix(), id))
}

// SetSession records the claude session id and run mode of the running launch that owns the job.
func (s *Store) SetSession(id, token, sessionID, mode string) error {
	return affected(s.db.Exec(`UPDATE jobs SET session_id=?, run_mode=? WHERE id=? AND status='running' AND launch_token=?`,
		sessionID, mode, id, token))
}

// Amend appends text to the brief of a queued job. The queue position (created_at, rowid) is kept.
func (s *Store) Amend(id, text string) error {
	return affected(s.db.Exec(`UPDATE jobs SET brief = brief || ? WHERE id=? AND status='queued'`, text, id))
}

// AmendContext appends text to the pending update of a queued resume (what the resumed session gets).
func (s *Store) AmendContext(id, text string) error {
	return affected(s.db.Exec(`UPDATE jobs SET pending_context = pending_context || ? WHERE id=? AND status='queued'
		AND attempt > 0`, text, id))
}

// SetCostUnknown marks the cost of a finished attempt as unknown (it ended without a result event).
func (s *Store) SetCostUnknown(id string) error {
	return affected(s.db.Exec(`UPDATE jobs SET cost_unknown=1 WHERE id=? AND status NOT IN ('queued','running')`, id))
}

// SetSessionDigest records the digest of the job's session transcript.
func (s *Store) SetSessionDigest(id, digest string) error {
	return affected(s.db.Exec(`UPDATE jobs SET session_digest=? WHERE id=?`, digest, id))
}

// AppendError appends a note to the error of a job that is no longer running.
func (s *Store) AppendError(id, note string) error {
	return affected(s.db.Exec(`UPDATE jobs SET error = CASE error WHEN '' THEN ? ELSE error || '; ' || ? END
		WHERE id=? AND status NOT IN ('queued','running')`, note, note, id))
}

// AttemptTotals sums the archived attempts that finished at or after since: how many, their turns,
// cost and duration (the current attempt of each job is its row).
func (s *Store) AttemptTotals(since time.Time) (n, turns int, cost float64, durationSec int, err error) {
	err = s.db.QueryRow(`SELECT COUNT(*), COALESCE(SUM(turns),0), COALESCE(SUM(cost_usd),0), COALESCE(SUM(duration_sec),0)
		FROM attempts WHERE finished_at >= ?`, since.Unix()).Scan(&n, &turns, &cost, &durationSec)
	return n, turns, cost, durationSec, err
}

// ResumeLimits are the limits of the next attempt.
type ResumeLimits struct {
	MaxTurns       int
	MaxBudgetUSD   float64
	TimeoutMinutes int
}

// Requeue queues the next attempt of a stopped, timed-out or failed job: the current attempt is
// archived in the attempts table when it ran (started_at set: a resume stopped while still queued
// never ran, and its attempt number is already archived), then the row goes back to queued with the new limits
// and context, its run fields reset (notify and cleanup start over). brief, when not empty, replaces
// the brief (a job stopped before it ever ran gets the update appended to its brief instead).
// It keeps created_at, so the job keeps its original queue position.
func (s *Store) Requeue(id string, l ResumeLimits, context, brief string) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	const resumable = `status IN ('stopped','timeout','failed')`
	if _, err := tx.Exec(`INSERT INTO attempts (job_id, n, mode, status, model, max_turns, max_budget_usd,
		timeout_minutes, turns, cost_usd, cost_unknown, duration_sec, error, context, session_id, started_at, finished_at)
		SELECT id, attempt, CASE run_mode WHEN '' THEN 'new' ELSE run_mode END, status, model, max_turns,
		max_budget_usd, timeout_minutes, turns, cost_usd, cost_unknown, duration_sec, error, pending_context, session_id,
		started_at, finished_at FROM jobs WHERE id=? AND attempt > 0 AND started_at IS NOT NULL AND `+resumable, id); err != nil {
		return fmt.Errorf("archive attempt: %w", err)
	}
	q := `UPDATE jobs SET status='queued', pid=0, pgid=0, launch_token='', started_at=NULL, finished_at=NULL, notified_at=NULL,
		cleaned_at=NULL, turns=0, cost_usd=0, cost_unknown=0, duration_sec=0, error='', max_turns=?, max_budget_usd=?,
		timeout_minutes=?, pending_context=?`
	args := []any{l.MaxTurns, l.MaxBudgetUSD, l.TimeoutMinutes, context}
	if brief != "" {
		q += `, brief=?`
		args = append(args, brief)
	}
	args = append(args, id)
	if err := affected(tx.Exec(q+` WHERE id=? AND `+resumable, args...)); err != nil {
		return err
	}
	return tx.Commit()
}

// Attempts returns the archived attempts of a job, oldest first (the current one is the job row).
func (s *Store) Attempts(id string) ([]Attempt, error) {
	rows, err := s.db.Query(`SELECT job_id, n, mode, status, model, max_turns, max_budget_usd, timeout_minutes,
		turns, cost_usd, cost_unknown, duration_sec, error, context, session_id, started_at, finished_at
		FROM attempts WHERE job_id=? ORDER BY n`, id)
	if err != nil {
		return nil, fmt.Errorf("query attempts: %w", err)
	}
	defer rows.Close()
	var out []Attempt
	for rows.Next() {
		var a Attempt
		var started, finished sql.NullInt64
		if err := rows.Scan(&a.JobID, &a.N, &a.Mode, &a.Status, &a.Model, &a.MaxTurns, &a.MaxBudgetUSD,
			&a.TimeoutMinutes, &a.Turns, &a.CostUSD, &a.CostUnknown, &a.DurationSec, &a.Error, &a.Context, &a.SessionID,
			&started, &finished); err != nil {
			return nil, fmt.Errorf("scan attempt: %w", err)
		}
		a.StartedAt, a.FinishedAt = convTS(started), convTS(finished)
		out = append(out, a)
	}
	return out, rows.Err()
}

// Unclaim puts a claimed job that was never spawned back into the queue (the launch that owns it found
// no cgroup scope right now and retries next tick). The attempt Claim counted is taken back.
func (s *Store) Unclaim(id, token string) error {
	return affected(s.db.Exec(`UPDATE jobs SET status='queued', pid=0, pgid=0, launch_token='', started_at=NULL,
		scope_unit='', attempt=MAX(attempt-1, 0) WHERE id=? AND status='running' AND launch_token=?`, id, token))
}

// SetPID records the wrapper's pid/pgid for the launch that owns the job.
func (s *Store) SetPID(id, token string, pid, pgid int) error {
	return affected(s.db.Exec(`UPDATE jobs SET pid=?, pgid=? WHERE id=? AND status='running' AND launch_token=?`,
		pid, pgid, id, token))
}

// SetScope records the systemd scope unit of the launch that owns the job, before the wrapper is spawned.
func (s *Store) SetScope(id, token, unit string) error {
	return affected(s.db.Exec(`UPDATE jobs SET scope_unit=? WHERE id=? AND status='running' AND launch_token=?`,
		unit, id, token))
}

// SetScopePending records whether the job's scope still has to be stopped (a failed stop is retried
// every tick until the unit is gone).
func (s *Store) SetScopePending(id string, pending bool) error {
	return affected(s.db.Exec(`UPDATE jobs SET scope_pending=? WHERE id=?`, pending, id))
}

// ScopePending returns the jobs whose scope stop is still pending, oldest first.
func (s *Store) ScopePending() ([]*Job, error) {
	return s.query(`WHERE scope_pending != 0 ORDER BY created_at, rowid`)
}

// Finish moves a job to a terminal status. With a launch token, only the running job owned by
// that launch can be finished (a tick never finishes another tick's job); with an empty token
// (explicit cancel or stop) any queued, running or stopped job can. Other terminal rows never change
// status again (only Requeue starts a new attempt).
func (s *Store) Finish(id, token, status string, turns int, cost float64, durationSec int, errMsg string, now time.Time) error {
	if status == StatusQueued || status == StatusRunning {
		return fmt.Errorf("finish: %q is not a terminal status", status)
	}
	q := `UPDATE jobs SET status=?, turns=?, cost_usd=?, duration_sec=?, error=?, finished_at=? WHERE id=? AND `
	args := []any{status, turns, cost, durationSec, errMsg, now.Unix(), id}
	if token == "" {
		q += `status IN ('queued','running','stopped')`
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
