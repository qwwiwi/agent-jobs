// Package store keeps the jobs table in SQLite (one database per agent).
package store

import (
	"context"
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
	// Runner is the program that runs the job: "claude" (claude -p) or "codex" (codex exec, S3-M1).
	Runner string
	// ReasoningEffort is the job's reasoning effort: low|medium|high on codex, low|medium|high|xhigh|max
	// on claude (passed as --effort); empty for claude rows from before it was recorded.
	ReasoningEffort string
	// OwnerWaits marks a job the owner asked for: its delivered job-done line records «owner reply
	// pending» (owner_pending), which holds the orchestrator's Stop hook until owner_replied.
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
	// AcceptanceCmd is the orchestrator's shell command a job must pass after claude exits 0 (empty = none).
	AcceptanceCmd string
	// AcceptanceStartedAt is when the wrapper started the acceptance command of the current attempt
	// (the daemon then allows it the acceptance timeout on top of the job's), nil before.
	AcceptanceStartedAt *time.Time
	// AcceptanceExit is the acceptance command's exit code of the current attempt (AcceptanceTimedOut for
	// a timeout), nil when it did not run.
	AcceptanceExit *int
	// AcceptanceDurationSec is how long the acceptance command ran.
	AcceptanceDurationSec int
	// AcceptanceDeadline is the daemon's deadline for the job once its acceptance command started: the
	// wrapper records it with the start, from the acceptance timeout it applies (nil before).
	AcceptanceDeadline *time.Time
	// AcceptancePGID is the process group of the running acceptance command (0 when none runs); cancel,
	// stop and timeout kill it along with the wrapper.
	AcceptancePGID int
	// TimedOutAt is when the daemon decided to time the running attempt out (nil before). Set by
	// ClaimTimeout; once set, the acceptance command can no longer start.
	TimedOutAt *time.Time
	// PGIDSpawned marks a PGID the daemon recorded itself when it spawned the wrapper (SetSpawnedPID),
	// not one adopted from the job-writable wrapper.json (SetPID). Only such a group may be signalled
	// once its wrapper is gone, and only its members shown to be the job's: the id may have been reused.
	PGIDSpawned bool
	// DirBytes is the size of the job directory last measured (when the attempt ended, again after a
	// prune; the worktree of a repo job left out; symlinks not followed), 0 before. DirTmpBytes is the
	// part in its temp dir. DirPartial: the bounded walk stopped early, both are lower bounds.
	DirBytes    int64
	DirTmpBytes int64
	DirPartial  bool
	// Skills are the catalog skills selected for the job at job_start (E7), sorted by name.
	Skills []string
	// SkillsDelivered are the skills linked into the job's work dir for the current attempt (removed
	// when the attempt ends; the record stays). A selected skill the repository already has is not
	// linked (the repository wins); a codex job gets none.
	SkillsDelivered []string
	// SkillsNote says why a selected skill was not delivered (repository wins, codex runner), "" otherwise.
	SkillsNote string
	// Outcome is what the orchestrator did with the result (job_outcome): accepted, extended, reworked,
	// rerun or discarded; "" until recorded. OutcomeNote is its optional note (at most 300 characters).
	Outcome     string
	OutcomeNote string
	OutcomeAt   *time.Time
	// BaseBranch is the branch the job's commits will be landed on: the full ref (refs/heads/<b> or
	// refs/remotes/<r>/<b>) the job was given, or the branch checked out in the repository when it was
	// queued; "" when the base was a tag, a commit or a detached HEAD.
	BaseBranch string
	// MergeNote is the merge check of the job's commits onto BaseBranch, made when its job-done line
	// was sent ("" = none: no repository or no commits of its own).
	MergeNote string
}

// AcceptanceTimedOut is the AcceptanceExit of an acceptance command ended by its timeout.
const AcceptanceTimedOut = -1

// CostIsUnknown reports whether the job's cost is not known: a codex job always (codex reports no
// price, so even a queued or running one has no cost to show), a claude job when CostUnknown is set.
func (j *Job) CostIsUnknown() bool { return j.CostUnknown || j.Runner == RunnerCodex }

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
	RunnerCodex         = "codex"
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
CREATE TABLE IF NOT EXISTS launch_authorizations (
	job_id TEXT PRIMARY KEY,
	attempt INTEGER NOT NULL,
	launch_token TEXT NOT NULL,
	codex_write_allowed INTEGER NOT NULL DEFAULT 0,
	acceptance_allowed INTEGER NOT NULL DEFAULT 0
);
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
		if err := migrate(db, alter); err != nil {
			_ = db.Close()
			return nil, fmt.Errorf("migrate db: %w", err)
		}
	}
	if err := migrateStatusCheck(db); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("migrate db: %w", err)
	}
	if _, err := db.Exec(intentionsSchema); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("migrate db: intentions: %w", err)
	}
	if _, err := db.Exec(schedulesSchema); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("migrate db: schedules: %w", err)
	}
	for _, alter := range scheduleMigrations {
		if err := migrate(db, alter); err != nil {
			_ = db.Close()
			return nil, fmt.Errorf("migrate db: schedules: %w", err)
		}
	}
	if _, err := db.Exec(checksSchema); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("migrate db: checks: %w", err)
	}
	for _, alter := range checksMigrations {
		if err := migrate(db, alter); err != nil {
			_ = db.Close()
			return nil, fmt.Errorf("migrate db: checks: %w", err)
		}
	}
	if _, err := db.Exec(notificationsSchema); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("migrate db: owner notifications: %w", err)
	}
	if err := migrateOwnerPending(db); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("migrate db: owner pending: %w", err)
	}
	if _, err := db.Exec(ownerPendingSchema); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("migrate db: owner pending: %w", err)
	}
	if _, err := db.Exec(heartbeatsSchema); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("migrate db: heartbeats: %w", err)
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
	`ALTER TABLE jobs ADD COLUMN reasoning_effort TEXT NOT NULL DEFAULT ''`,
	`ALTER TABLE job_stats ADD COLUMN cost_unknown INTEGER NOT NULL DEFAULT 0`,
	`ALTER TABLE jobs ADD COLUMN redaction_pending INTEGER NOT NULL DEFAULT 0`,
	`ALTER TABLE jobs ADD COLUMN skills TEXT NOT NULL DEFAULT ''`,
	`ALTER TABLE jobs ADD COLUMN skills_delivered TEXT NOT NULL DEFAULT ''`,
	`ALTER TABLE jobs ADD COLUMN skills_note TEXT NOT NULL DEFAULT ''`,
	`ALTER TABLE job_stats ADD COLUMN skills_delivered TEXT NOT NULL DEFAULT ''`,
	`ALTER TABLE jobs ADD COLUMN acceptance_cmd TEXT NOT NULL DEFAULT ''`,
	`ALTER TABLE jobs ADD COLUMN acceptance_started_at INTEGER`,
	`ALTER TABLE jobs ADD COLUMN acceptance_exit INTEGER`,
	`ALTER TABLE jobs ADD COLUMN acceptance_duration_sec INTEGER NOT NULL DEFAULT 0`,
	`ALTER TABLE jobs ADD COLUMN acceptance_deadline INTEGER`,
	`ALTER TABLE jobs ADD COLUMN acceptance_pgid INTEGER NOT NULL DEFAULT 0`,
	`ALTER TABLE jobs ADD COLUMN timed_out_at INTEGER`,
	jobStatsEffortAlter,
	`ALTER TABLE job_stats ADD COLUMN report_off_contract INTEGER NOT NULL DEFAULT 0`,
	`ALTER TABLE jobs ADD COLUMN pgid_spawned INTEGER NOT NULL DEFAULT 0`,
	`ALTER TABLE jobs ADD COLUMN dir_bytes INTEGER NOT NULL DEFAULT 0`,
	`ALTER TABLE job_stats ADD COLUMN dir_bytes INTEGER NOT NULL DEFAULT 0`,
	`ALTER TABLE jobs ADD COLUMN dir_tmp_bytes INTEGER NOT NULL DEFAULT 0`,
	`ALTER TABLE jobs ADD COLUMN dir_partial INTEGER NOT NULL DEFAULT 0`,
	`ALTER TABLE jobs ADD COLUMN outcome TEXT NOT NULL DEFAULT ''`,
	`ALTER TABLE jobs ADD COLUMN outcome_note TEXT NOT NULL DEFAULT ''`,
	`ALTER TABLE jobs ADD COLUMN outcome_at INTEGER`,
	`ALTER TABLE job_stats ADD COLUMN outcome TEXT NOT NULL DEFAULT ''`,
	`ALTER TABLE job_stats ADD COLUMN outcome_note TEXT NOT NULL DEFAULT ''`,
	`ALTER TABLE job_stats ADD COLUMN cost_estimated_usd REAL NOT NULL DEFAULT 0`,
	`ALTER TABLE job_stats ADD COLUMN cost_source TEXT NOT NULL DEFAULT ''`,
	`ALTER TABLE job_stats ADD COLUMN input_tokens INTEGER NOT NULL DEFAULT 0`,
	`ALTER TABLE job_stats ADD COLUMN output_tokens INTEGER NOT NULL DEFAULT 0`,
	`ALTER TABLE job_stats ADD COLUMN cache_read_tokens INTEGER NOT NULL DEFAULT 0`,
	`ALTER TABLE job_stats ADD COLUMN cache_write_tokens INTEGER NOT NULL DEFAULT 0`,
	`ALTER TABLE jobs ADD COLUMN base_branch TEXT NOT NULL DEFAULT ''`,
	`ALTER TABLE jobs ADD COLUMN merge_note TEXT NOT NULL DEFAULT ''`,
	`ALTER TABLE jobs ADD COLUMN cleanup_skipped_at INTEGER`,
	`ALTER TABLE jobs ADD COLUMN fallback_session TEXT NOT NULL DEFAULT ''`,
	`ALTER TABLE jobs ADD COLUMN wrapper_result TEXT NOT NULL DEFAULT ''`,
	`ALTER TABLE jobs ADD COLUMN wrapper_token TEXT NOT NULL DEFAULT ''`,
	`ALTER TABLE attempts ADD COLUMN wrapper_result TEXT NOT NULL DEFAULT ''`,
}

const jobStatsEffortAlter = `ALTER TABLE job_stats ADD COLUMN effort TEXT NOT NULL DEFAULT ''`

// migrationBackfills fill a column added by a migration (the key) for the rows that existed before it:
// they run in the same transaction as the ALTER, so only once, by the process that added the column.
var migrationBackfills = map[string]string{
	// The stats of a finished job whose cost is unknown (a codex job, or a job whose attempts all ended
	// without a cost, as RecordStats computes it) must not count as a known $0.00. A stats row whose
	// job was pruned keeps the default.
	`ALTER TABLE job_stats ADD COLUMN cost_unknown INTEGER NOT NULL DEFAULT 0`: `UPDATE job_stats SET cost_unknown=1
		WHERE id IN (SELECT j.id FROM jobs j WHERE j.runner='codex' OR (j.cost_unknown=1 AND NOT EXISTS
			(SELECT 1 FROM attempts a WHERE a.job_id=j.id AND a.cost_unknown=0)))`,
	// Every job that existed before the column may hold an unredacted report: a queued or running
	// attempt writes one later, a finished one was settled by a build that did not redact (or did
	// and died before the rewrite). All are marked; redaction is idempotent, and a job without a
	// regular report.md is just cleared on the next tick.
	`ALTER TABLE jobs ADD COLUMN redaction_pending INTEGER NOT NULL DEFAULT 0`: `UPDATE jobs SET redaction_pending=1`,
	// Existing stats rows take the effort recorded on their job (codex jobs; claude rows from before the
	// effort was recorded stay empty). A stats row whose job was pruned keeps the default.
	jobStatsEffortAlter: `UPDATE job_stats SET effort = COALESCE((SELECT j.reasoning_effort FROM jobs j
		WHERE j.id = job_stats.id), '')`,
	checksAlertStateAlter:   checksAlertStateFill,
	checksAlertsMoved:       checksAlertsFill,
	checksMarkerPolicyAlter: checksMarkerProvenanceFill,
}

// migrate runs one migration and its backfill, if any, in one transaction; a column that is already
// there ("duplicate column") is no error and is not backfilled again.
func migrate(db *sql.DB, alter string) error {
	tx, err := db.Begin()
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := tx.Exec(alter); err != nil {
		if strings.Contains(err.Error(), "duplicate column") {
			return nil
		}
		return err
	}
	if fill, ok := migrationBackfills[alter]; ok {
		if _, err := tx.Exec(fill); err != nil {
			return fmt.Errorf("backfill: %w", err)
		}
	}
	return tx.Commit()
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
	pending_context, cost_unknown, session_digest, scope_unit, scope_pending, reasoning_effort,
	skills, skills_delivered, skills_note,
	acceptance_cmd, acceptance_started_at, acceptance_exit, acceptance_duration_sec, acceptance_deadline,
	acceptance_pgid, timed_out_at, pgid_spawned, dir_bytes, dir_tmp_bytes, dir_partial, outcome, outcome_note, outcome_at,
	base_branch, merge_note`

func ts(t *time.Time) any {
	if t == nil {
		return nil
	}
	return t.Unix()
}

// Insert adds a new job row. Empty Runner / Billing get their defaults.
func (s *Store) Insert(j *Job) error {
	_, err := s.db.Exec(`INSERT INTO jobs (`+cols+`) VALUES (`+insertMarks+`)`, insertArgs(j)...)
	if err != nil {
		return fmt.Errorf("insert job: %w", err)
	}
	return nil
}

// InsertCapped adds a new job row unless agent already has limit codex jobs created at or after since
// (the codex daily cap): the count and the insert are one statement, so two processes starting codex
// jobs at once cannot both pass the cap. It returns false, nothing inserted, at the cap.
func (s *Store) InsertCapped(j *Job, since time.Time, limit int) (bool, error) {
	args := append(insertArgs(j), j.Agent, RunnerCodex, since.Unix(), limit)
	res, err := s.db.Exec(`INSERT INTO jobs (`+cols+`) SELECT `+insertMarks+` WHERE (SELECT COUNT(*) FROM jobs
		WHERE agent = ? AND runner = ? AND created_at >= ?) < ?`, args...)
	if err != nil {
		return false, fmt.Errorf("insert job: %w", err)
	}
	n, err := res.RowsAffected()
	return n > 0, err
}

// CountCodexSince returns how many codex jobs of agent were created at or after since, whatever became
// of them.
func (s *Store) CountCodexSince(agent string, since time.Time) (int, error) {
	var n int
	err := s.db.QueryRow(`SELECT COUNT(*) FROM jobs WHERE agent = ? AND runner = ? AND created_at >= ?`,
		agent, RunnerCodex, since.Unix()).Scan(&n)
	if err != nil {
		return 0, fmt.Errorf("count codex jobs: %w", err)
	}
	return n, nil
}

// insertMarks are the placeholders of one jobs row (one per column of cols).
var insertMarks = strings.TrimSuffix(strings.Repeat("?,", strings.Count(cols, ",")+1), ",")

// insertArgs are the values of j for cols; empty Runner / Billing get their defaults.
func insertArgs(j *Job) []any {
	if j.Runner == "" {
		j.Runner = RunnerClaude
	}
	if j.Billing == "" {
		j.Billing = BillingSubscription
	}
	return []any{j.ID, j.Agent, j.Title, j.Kind, j.Brief, j.RepoPath, j.BaseCommit, j.Status, j.Model,
		j.MaxTurns, j.MaxBudgetUSD, j.TimeoutMinutes, j.PID, j.PGID, j.CreatedAt.Unix(),
		ts(j.StartedAt), ts(j.FinishedAt), ts(j.NotifiedAt), ts(j.CleanedAt),
		j.Turns, j.CostUSD, j.DurationSec, j.Error, j.LaunchToken,
		j.Complexity, j.Runner, j.OwnerWaits, j.ResumedFrom, j.ScheduleID, j.Billing, j.SessionID, j.Attempt,
		j.RunMode, j.PendingContext, j.CostUnknown, j.SessionDigest, j.ScopeUnit, j.ScopePending, j.ReasoningEffort,
		JoinNames(j.Skills), JoinNames(j.SkillsDelivered), j.SkillsNote,
		j.AcceptanceCmd, ts(j.AcceptanceStartedAt), j.AcceptanceExit, j.AcceptanceDurationSec, ts(j.AcceptanceDeadline),
		j.AcceptancePGID, ts(j.TimedOutAt), j.PGIDSpawned, j.DirBytes, j.DirTmpBytes, j.DirPartial,
		j.Outcome, j.OutcomeNote, ts(j.OutcomeAt), j.BaseBranch, j.MergeNote}
}

type scanner interface{ Scan(dest ...any) error }

func scanJob(r scanner) (*Job, error) {
	var j Job
	var created int64
	var started, finished, notified, cleaned, accStarted, accExit, accDeadline, timedOut, outcomeAt sql.NullInt64
	var skills, delivered string
	err := r.Scan(&j.ID, &j.Agent, &j.Title, &j.Kind, &j.Brief, &j.RepoPath, &j.BaseCommit, &j.Status,
		&j.Model, &j.MaxTurns, &j.MaxBudgetUSD, &j.TimeoutMinutes, &j.PID, &j.PGID, &created,
		&started, &finished, &notified, &cleaned, &j.Turns, &j.CostUSD, &j.DurationSec, &j.Error, &j.LaunchToken,
		&j.Complexity, &j.Runner, &j.OwnerWaits, &j.ResumedFrom, &j.ScheduleID, &j.Billing, &j.SessionID, &j.Attempt,
		&j.RunMode, &j.PendingContext, &j.CostUnknown, &j.SessionDigest, &j.ScopeUnit, &j.ScopePending, &j.ReasoningEffort,
		&skills, &delivered, &j.SkillsNote,
		&j.AcceptanceCmd, &accStarted, &accExit, &j.AcceptanceDurationSec, &accDeadline, &j.AcceptancePGID, &timedOut,
		&j.PGIDSpawned, &j.DirBytes, &j.DirTmpBytes, &j.DirPartial,
		&j.Outcome, &j.OutcomeNote, &outcomeAt, &j.BaseBranch, &j.MergeNote)
	if err != nil {
		return nil, err
	}
	j.Skills, j.SkillsDelivered = SplitNames(skills), SplitNames(delivered)
	j.OutcomeAt = convTS(outcomeAt)
	j.AcceptanceStartedAt, j.AcceptanceDeadline, j.TimedOutAt = convTS(accStarted), convTS(accDeadline), convTS(timedOut)
	if accExit.Valid {
		e := int(accExit.Int64)
		j.AcceptanceExit = &e
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
func (s *Store) Get(id string) (*Job, error) { return s.GetContext(context.Background(), id) }

// GetContext is Get, given up when ctx ends.
func (s *Store) GetContext(ctx context.Context, id string) (*Job, error) {
	j, err := scanJob(s.db.QueryRowContext(ctx, `SELECT `+cols+` FROM jobs WHERE id = ?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("get job: %w", err)
	}
	return j, nil
}

func (s *Store) query(q string, args ...any) ([]*Job, error) {
	return s.queryContext(context.Background(), q, args...)
}

func (s *Store) queryContext(ctx context.Context, q string, args ...any) ([]*Job, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT `+cols+` FROM jobs `+q, args...)
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
func (s *Store) List(limit int) ([]*Job, error) { return s.ListContext(context.Background(), limit) }

// ListContext is List, given up when ctx ends.
func (s *Store) ListContext(ctx context.Context, limit int) ([]*Job, error) {
	return s.queryContext(ctx, `ORDER BY created_at DESC, rowid DESC LIMIT ?`, limit)
}

// ListRecentByStatus returns the most recent jobs with the given status, newest first.
func (s *Store) ListRecentByStatus(status string, limit int) ([]*Job, error) {
	return s.ListRecentByStatusContext(context.Background(), status, limit)
}

// ListRecentByStatusContext is ListRecentByStatus, given up when ctx ends.
func (s *Store) ListRecentByStatusContext(ctx context.Context, status string, limit int) ([]*Job, error) {
	return s.queryContext(ctx, `WHERE status = ? ORDER BY created_at DESC, rowid DESC LIMIT ?`, status, limit)
}

// CountByStatusContext returns the number of jobs of agent with the given status.
func (s *Store) CountByStatusContext(ctx context.Context, agent, status string) (int, error) {
	var n int
	err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM jobs WHERE agent = ? AND status = ?`, agent, status).Scan(&n)
	if err != nil {
		return 0, fmt.Errorf("count jobs: %w", err)
	}
	return n, nil
}

// ListActivity returns jobs created or finished at or after since, plus every queued and running
// job, newest first, at most limit rows.
func (s *Store) ListActivity(since time.Time, limit int) ([]*Job, error) {
	return s.query(`WHERE created_at >= ? OR finished_at >= ? OR status IN ('queued','running','stopped')
		ORDER BY created_at DESC, rowid DESC LIMIT ?`, since.Unix(), since.Unix(), limit)
}

// PendingCleanup returns finished, notified, not yet cleaned jobs (a stopped job keeps its work dir)
// whose cleanup was not given up (MarkCleanupSkipped).
func (s *Store) PendingCleanup() ([]*Job, error) {
	return s.query(`WHERE status NOT IN ('queued','running','stopped') AND notified_at IS NOT NULL AND cleaned_at IS NULL AND cleanup_skipped_at IS NULL ORDER BY finished_at`)
}

// PendingNotify returns finished jobs whose notification was not sent yet. A stopped job is not
// notified: the orchestrator stopped it itself.
func (s *Store) PendingNotify() ([]*Job, error) {
	return s.query(`WHERE status NOT IN ('queued','running','stopped') AND notified_at IS NULL ORDER BY finished_at`)
}

// ListActiveOnRepo returns the queued and running jobs on repository repo other than id, oldest first.
func (s *Store) ListActiveOnRepo(repo, id string) ([]*Job, error) {
	return s.query(`WHERE repo_path = ? AND id != ? AND status IN ('queued','running') ORDER BY created_at, rowid`, repo, id)
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
	// redaction_pending: from now on the attempt may write report.md; it is cleared only once the
	// report of the ended attempt has been redacted (ClearRedactionPending).
	return affected(s.db.Exec(`UPDATE jobs SET status='running', pid=0, pgid=0, pgid_spawned=0, launch_token=?, started_at=?,
		attempt=attempt+1, redaction_pending=1, fallback_session='', wrapper_result='', wrapper_token='' WHERE id=? AND status='queued'`, token, now.Unix(), id))
}

// RedactionPending returns the ids of the jobs that are no longer queued or running and whose
// report.md has not been redacted yet (the rewrite failed, or the daemon died before it), oldest first.
func (s *Store) RedactionPending() ([]string, error) {
	rows, err := s.db.Query(`SELECT id FROM jobs WHERE redaction_pending != 0 AND status NOT IN ('queued','running')
		ORDER BY created_at, rowid`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}

// ClearRedactionPending records that the report of the ended attempt is redacted. A job that was
// requeued or claimed again meanwhile keeps the mark (its new attempt may write a new report).
func (s *Store) ClearRedactionPending(id string) error {
	_, err := s.db.Exec(`UPDATE jobs SET redaction_pending=0 WHERE id=? AND status NOT IN ('queued','running')`, id)
	return err
}

// SetSession records the claude session id and run mode of the running launch that owns the job.
// It clears the launch's fallback session (SetFallbackSession sets it again for a resume launch).
func (s *Store) SetSession(id, token, sessionID, mode string) error {
	return affected(s.db.Exec(`UPDATE jobs SET session_id=?, run_mode=?, fallback_session='' WHERE id=? AND status='running'
		AND launch_token=?`, sessionID, mode, id, token))
}

// SetFallbackSession records the fresh session id the daemon generated for a resume launch's
// fallback: the only one the wrapper's fallback marker may name.
func (s *Store) SetFallbackSession(id, token, sessionID string) error {
	return affected(s.db.Exec(`UPDATE jobs SET fallback_session=? WHERE id=? AND status='running' AND run_mode='resume'
		AND launch_token=? AND launch_token!=''`,
		sessionID, id, token))
}

// FallbackSession returns the fallback session id recorded for the launch with token ("" if none).
func (s *Store) FallbackSession(id, token string) (string, error) {
	var sid string
	err := s.db.QueryRow(`SELECT fallback_session FROM jobs WHERE id=? AND launch_token=?`, id, token).Scan(&sid)
	if errors.Is(err, sql.ErrNoRows) {
		return "", ErrNotFound
	}
	return sid, err
}

// SetWrapperResult records, for the running launch with token, the result the wrapper read from
// the runner's stdout itself (an opaque JSON text): the daemon trusts it, not stream.jsonl in the job dir.
func (s *Store) SetWrapperResult(id, token, result string) error {
	return affected(s.db.Exec(`UPDATE jobs SET wrapper_result=?, wrapper_token=? WHERE id=? AND status='running'
		AND launch_token=? AND launch_token!=''`, result, token, id, token))
}

// WrapperResult returns the result the wrapper of the launch with token recorded; ok is false when
// that wrapper recorded none.
func (s *Store) WrapperResult(id, token string) (result string, ok bool, err error) {
	var tok, current string
	err = s.db.QueryRow(`SELECT wrapper_result, wrapper_token, launch_token FROM jobs WHERE id=?`, id).Scan(&result, &tok, &current)
	if errors.Is(err, sql.ErrNoRows) {
		return "", false, ErrNotFound
	}
	if err != nil || token == "" || tok != token || current != token {
		return "", false, err
	}
	return result, true, nil
}

// AttemptWrapperResult is the trusted wrapper snapshot archived transactionally at requeue.
func (s *Store) AttemptWrapperResult(id string, n int) (string, error) {
	var raw string
	err := s.db.QueryRow(`SELECT wrapper_result FROM attempts WHERE job_id=? AND n=?`, id, n).Scan(&raw)
	if errors.Is(err, sql.ErrNoRows) {
		return "", ErrNotFound
	}
	return raw, err
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

// SetMergeNote records the merge check of a job that is no longer queued or running.
func (s *Store) SetMergeNote(id, note string) error {
	return affected(s.db.Exec(`UPDATE jobs SET merge_note=? WHERE id=? AND status NOT IN ('queued','running')`, note, id))
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
// It keeps created_at, so the job keeps its original queue position. A recorded outcome (job_outcome)
// was about the previous attempt's result and is cleared.
func (s *Store) Requeue(id string, l ResumeLimits, context, brief string) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	const resumable = `status IN ('stopped','timeout','failed')`
	if _, err := tx.Exec(`INSERT INTO attempts (job_id, n, mode, status, model, max_turns, max_budget_usd,
		timeout_minutes, turns, cost_usd, cost_unknown, duration_sec, error, context, session_id, started_at, finished_at, wrapper_result)
		SELECT id, attempt, CASE run_mode WHEN '' THEN 'new' ELSE run_mode END, status, model, max_turns,
		max_budget_usd, timeout_minutes, turns, cost_usd, cost_unknown, duration_sec, error, pending_context, session_id,
		started_at, finished_at, CASE WHEN wrapper_token=launch_token AND launch_token!='' THEN wrapper_result ELSE '' END FROM jobs WHERE id=? AND attempt > 0 AND started_at IS NOT NULL AND `+resumable, id); err != nil {
		return fmt.Errorf("archive attempt: %w", err)
	}
	q := `UPDATE jobs SET status='queued', pid=0, pgid=0, pgid_spawned=0, launch_token='', started_at=NULL, finished_at=NULL, notified_at=NULL,
		merge_note='', cleaned_at=NULL, cleanup_skipped_at=NULL, turns=0, cost_usd=0, cost_unknown=0, duration_sec=0, dir_bytes=0, dir_tmp_bytes=0, dir_partial=0, error='',
		acceptance_started_at=NULL,
		acceptance_exit=NULL, acceptance_duration_sec=0, acceptance_deadline=NULL, acceptance_pgid=0, timed_out_at=NULL, max_turns=?, max_budget_usd=?,
		timeout_minutes=?, pending_context=?, outcome='', outcome_note='', outcome_at=NULL`
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
	return affected(s.db.Exec(`UPDATE jobs SET status='queued', pid=0, pgid=0, pgid_spawned=0, launch_token='', started_at=NULL,
		scope_unit='', attempt=MAX(attempt-1, 0) WHERE id=? AND status='running' AND launch_token=?`, id, token))
}

// SetAcceptanceStarted records, for the running launch that owns the job, that its acceptance command
// starts now and the deadline the daemon must then allow the job. It is the acceptance side of the
// timeout race: it fails (ErrImmutable) once ClaimTimeout has decided to time the attempt out, and a
// claim after it sees the recorded deadline. SQLite serializes the two updates, so exactly one wins.
func (s *Store) SetAcceptanceStarted(id, token string, now, deadline time.Time) error {
	return affected(s.db.Exec(`UPDATE jobs SET acceptance_started_at=?, acceptance_deadline=? WHERE id=? AND
		status='running' AND launch_token=? AND timed_out_at IS NULL`, now.Unix(), deadline.Unix(), id, token))
}

// SetAcceptancePGID records the process group of the running acceptance command (0 = it ended).
func (s *Store) SetAcceptancePGID(id, token string, pgid int) error {
	return affected(s.db.Exec(`UPDATE jobs SET acceptance_pgid=? WHERE id=? AND status='running' AND launch_token=?`,
		pgid, id, token))
}

// ClearAcceptancePGID forgets a finished attempt's acceptance group once it is gone. Match the
// launch and group so a stale row cannot clear a later attempt's group.
func (s *Store) ClearAcceptancePGID(id, token string, pgid int) error {
	return affected(s.db.Exec(`UPDATE jobs SET acceptance_pgid=0 WHERE id=? AND launch_token=? AND acceptance_pgid=?
		AND status NOT IN ('queued','running')`, id, token, pgid))
}

// SetAcceptanceResult records the acceptance command's exit code (AcceptanceTimedOut for a timeout)
// and duration for the running launch that owns the job; the command's process group is gone by then.
func (s *Store) SetAcceptanceResult(id, token string, exit, durationSec int) error {
	return affected(s.db.Exec(`UPDATE jobs SET acceptance_exit=?, acceptance_duration_sec=?, acceptance_pgid=0 WHERE id=?
		AND status='running' AND launch_token=?`, exit, durationSec, id, token))
}

// ClaimTimeout decides, in one conditional update, that the running attempt under token has timed out
// at now: past started_at + timeout_minutes and, when its acceptance command started, past the
// recorded acceptance deadline. It returns false when the attempt has not timed out by the row as it
// is now (the acceptance command started after the caller read the row) or is no longer running under
// token. A claim already made stays (true again): the daemon may have died before finishing the row.
func (s *Store) ClaimTimeout(id, token string, now time.Time) (bool, error) {
	res, err := s.db.Exec(`UPDATE jobs SET timed_out_at=COALESCE(timed_out_at, ?) WHERE id=? AND status='running'
		AND launch_token=? AND (timed_out_at IS NOT NULL OR (started_at IS NOT NULL AND ? > started_at + timeout_minutes*60
		AND (acceptance_deadline IS NULL OR ? > acceptance_deadline)))`, now.Unix(), id, token, now.Unix(), now.Unix())
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	return n > 0, err
}

// SetPID records the wrapper's pid/pgid, adopted from wrapper.json, for the launch that owns the job.
func (s *Store) SetPID(id, token string, pid, pgid int) error {
	return affected(s.db.Exec(`UPDATE jobs SET pid=?, pgid=?, pgid_spawned=0 WHERE id=? AND status='running' AND launch_token=?`,
		pid, pgid, id, token))
}

// SetSpawnedPID records the pid of the wrapper the daemon itself just spawned (in a new session, so
// its process group is its pid) for the launch that owns the job; the group is marked PGIDSpawned.
func (s *Store) SetSpawnedPID(id, token string, pid int) error {
	return affected(s.db.Exec(`UPDATE jobs SET pid=?, pgid=?, pgid_spawned=1 WHERE id=? AND status='running' AND launch_token=?`,
		pid, pid, id, token))
}

// ClearPGIDSpawned forgets that a finished job's recorded pgid is the one its wrapper was spawned
// with: called once that group is gone, after which the id may be reused by an unrelated group.
func (s *Store) ClearPGIDSpawned(id string) error {
	return affected(s.db.Exec(`UPDATE jobs SET pgid_spawned=0 WHERE id=? AND status NOT IN ('queued','running')`, id))
}

// SetDirBytes records the size of the job directory as just measured: total bytes, the part in its
// temp dir, and whether the bounded walk stopped early (both then lower bounds).
func (s *Store) SetDirBytes(id string, n, tmp int64, partial bool) error {
	return affected(s.db.Exec(`UPDATE jobs SET dir_bytes=?, dir_tmp_bytes=?, dir_partial=? WHERE id=?`, n, tmp, partial, id))
}

// SetScope records the systemd scope unit of the launch that owns the job, before the wrapper is spawned.
func (s *Store) SetScope(id, token, unit string) error {
	return affected(s.db.Exec(`UPDATE jobs SET scope_unit=? WHERE id=? AND status='running' AND launch_token=?`,
		unit, id, token))
}

// SetSkillsDelivered records the skills linked into the work dir of the running attempt (token) and
// why a selected one was not (note).
func (s *Store) SetSkillsDelivered(id, token string, delivered []string, note string) error {
	return affected(s.db.Exec(`UPDATE jobs SET skills_delivered=?, skills_note=? WHERE id=? AND status='running' AND launch_token=?`,
		JoinNames(delivered), note, id, token))
}

// JoinNames stores a list of skill names (names never contain a comma) as one column.
func JoinNames(names []string) string { return strings.Join(names, ",") }

// SplitNames is the inverse of JoinNames ("" = none).
func SplitNames(s string) []string {
	if s == "" {
		return nil
	}
	return strings.Split(s, ",")
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
	// redaction_pending is set in the same update: the report of the ended attempt counts as
	// unredacted until it was redacted (ClearRedactionPending), whatever set or cleared it before.
	q := `UPDATE jobs SET status=?, turns=?, cost_usd=?, duration_sec=?, error=?, finished_at=?, redaction_pending=1
		WHERE id=? AND `
	args := []any{status, turns, cost, durationSec, errMsg, now.Unix(), id}
	if token == "" {
		q += `status IN ('queued','running','stopped')`
	} else {
		q += `status='running' AND launch_token=?`
		args = append(args, token)
	}
	return affected(s.db.Exec(q, args...))
}

// MarkCleanupSkipped records that the worktree cleanup was given up (its repository is gone): the
// job leaves PendingCleanup, and its folder stays (cleaned_at stays NULL, so prune keeps it).
func (s *Store) MarkCleanupSkipped(id string, now time.Time) error {
	_, err := s.db.Exec(`UPDATE jobs SET cleanup_skipped_at=? WHERE id=?`, now.Unix(), id)
	return err
}

// MarkCleaned records that the worktree cleanup ran.
func (s *Store) MarkCleaned(id string, now time.Time) error {
	_, err := s.db.Exec(`UPDATE jobs SET cleaned_at=? WHERE id=?`, now.Unix(), id)
	return err
}
