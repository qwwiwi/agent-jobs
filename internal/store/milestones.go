package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// Milestone statuses. A milestone is never deleted: it ends done or dropped.
const (
	MilestoneOpen    = "open"
	MilestoneDone    = "done"
	MilestoneDropped = "dropped"
)

// ErrMilestoneNotFound is returned when a milestone id does not exist.
var ErrMilestoneNotFound = errors.New("milestone not found")

// Milestone is one row of the milestones table: a stage of work, split into tasks that jobs work on.
type Milestone struct {
	ID    int64
	Title string
	// Goal is one optional line; Repo the main worktree of the repository ("" = none); Branch the
	// integration branch job_start takes as the default base ("" = none).
	Goal   string
	Repo   string
	Branch string
	// ChecklistID ties the milestone to the repository's acceptance checklist ("" = none): task covers
	// must then be live rows of it.
	ChecklistID string
	// Order sorts the milestones of one repository (a project view), lowest first.
	Order  int
	Status string
	// MaxParallel caps the running jobs of the milestone (nil = no cap of its own).
	MaxParallel *int
	CreatedAt   time.Time
	// UpdatedAt moves on any change to the milestone, its tasks, or a job attached to it (the change
	// cursor of milestone_list updated_since).
	UpdatedAt time.Time
	ClosedAt  *time.Time
	CloseNote string
}

// MilestoneTask is one task of a milestone, keyed T1, T2, … inside it. Its status is derived from its
// jobs and dependencies, never stored; a task is dropped, never deleted.
type MilestoneTask struct {
	MilestoneID int64
	Key         string
	Title       string
	DependsOn   []string
	Covers      []string
	Dropped     bool
	CreatedAt   time.Time
	UpdatedAt   time.Time
}

// milestonesSchema is applied at the end of every Open, after the jobs columns and the gate tables exist:
// the tables, the index of the jobs of a milestone, and the triggers that move a milestone's updated_at
// when a job attached to it is added or changes status or outcome (whichever code path changes the row),
// and when a judge verdict of its repository and checklist is stored (acceptance_met reads the latest).
const milestonesSchema = `
CREATE TABLE IF NOT EXISTS milestones (
	id           INTEGER PRIMARY KEY AUTOINCREMENT,
	title        TEXT NOT NULL,
	goal         TEXT NOT NULL DEFAULT '',
	repo         TEXT NOT NULL DEFAULT '',
	branch       TEXT NOT NULL DEFAULT '',
	checklist_id TEXT,
	ord          INTEGER NOT NULL DEFAULT 0,
	status       TEXT NOT NULL CHECK (status IN ('open','done','dropped')),
	max_parallel INTEGER,
	created_at   INTEGER NOT NULL,
	updated_at   INTEGER NOT NULL,
	closed_at    INTEGER,
	close_note   TEXT NOT NULL DEFAULT ''
);
CREATE INDEX IF NOT EXISTS milestones_order ON milestones(repo, ord, created_at);
CREATE TABLE IF NOT EXISTS milestone_tasks (
	milestone_id INTEGER NOT NULL,
	key          TEXT NOT NULL,
	title        TEXT NOT NULL,
	depends_on   TEXT NOT NULL DEFAULT '',
	covers       TEXT NOT NULL DEFAULT '',
	dropped      INTEGER NOT NULL DEFAULT 0,
	created_at   INTEGER NOT NULL,
	updated_at   INTEGER NOT NULL,
	PRIMARY KEY (milestone_id, key)
);
CREATE INDEX IF NOT EXISTS jobs_milestone ON jobs(milestone_id);
CREATE TRIGGER IF NOT EXISTS jobs_milestone_insert AFTER INSERT ON jobs WHEN NEW.milestone_id IS NOT NULL
BEGIN
	UPDATE milestones SET updated_at = MAX(updated_at, CAST(strftime('%s','now') AS INTEGER)) WHERE id = NEW.milestone_id;
END;
CREATE TRIGGER IF NOT EXISTS jobs_milestone_update AFTER UPDATE OF status, outcome ON jobs
WHEN NEW.milestone_id IS NOT NULL AND (OLD.status IS NOT NEW.status OR OLD.outcome IS NOT NEW.outcome)
BEGIN
	UPDATE milestones SET updated_at = MAX(updated_at, CAST(strftime('%s','now') AS INTEGER)) WHERE id = NEW.milestone_id;
END;
CREATE TRIGGER IF NOT EXISTS gate_verdicts_milestone AFTER INSERT ON gate_verdicts
BEGIN
	UPDATE milestones SET updated_at = MAX(updated_at, CAST(strftime('%s','now') AS INTEGER))
		WHERE repo = NEW.repo AND checklist_id = NEW.checklist_id;
END;
`

// sqlNow is the database's clock in Unix seconds. updated_at is stamped with it inside the writing
// statement, so under the write lock: a write that waited for the lock is not stamped earlier than a
// write committed before it, and a poller's cursor never passes it.
const sqlNow = `CAST(strftime('%s','now') AS INTEGER)`

const milestoneCols = `id, title, goal, repo, branch, checklist_id, ord, status, max_parallel, created_at, updated_at, closed_at, close_note`

func scanMilestone(r scanner) (*Milestone, error) {
	var ms Milestone
	var checklistID sql.NullString
	var maxPar, closed sql.NullInt64
	var created, updated int64
	if err := r.Scan(&ms.ID, &ms.Title, &ms.Goal, &ms.Repo, &ms.Branch, &checklistID, &ms.Order, &ms.Status, &maxPar,
		&created, &updated, &closed, &ms.CloseNote); err != nil {
		return nil, err
	}
	ms.ChecklistID = checklistID.String
	if maxPar.Valid {
		n := int(maxPar.Int64)
		ms.MaxParallel = &n
	}
	ms.CreatedAt, ms.UpdatedAt, ms.ClosedAt = time.Unix(created, 0), time.Unix(updated, 0), convTS(closed)
	return &ms, nil
}

// nullString is s, or NULL for "".
func nullString(s string) any {
	if s == "" {
		return nil
	}
	return s
}

// nullInt is *n, or NULL for nil.
func nullInt(n *int) any {
	if n == nil {
		return nil
	}
	return *n
}

// nullID is id, or NULL for 0.
func nullID(id int64) any {
	if id == 0 {
		return nil
	}
	return id
}

// CreateMilestone inserts an open milestone with its tasks in one transaction and returns its id.
// Validation (titles, keys, dependencies, covers) is the caller's.
func (s *Store) CreateMilestone(ms *Milestone, tasks []MilestoneTask, now time.Time) (int64, error) {
	tx, err := s.db.Begin()
	if err != nil {
		return 0, err
	}
	defer func() { _ = tx.Rollback() }()
	res, err := tx.Exec(`INSERT INTO milestones (title, goal, repo, branch, checklist_id, ord, status, max_parallel,
		created_at, updated_at) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, MAX(?, `+sqlNow+`))`, ms.Title, ms.Goal, ms.Repo, ms.Branch,
		nullString(ms.ChecklistID), ms.Order, MilestoneOpen, nullInt(ms.MaxParallel), now.Unix(), now.Unix())
	if err != nil {
		return 0, fmt.Errorf("create milestone: %w", err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		return 0, fmt.Errorf("create milestone: %w", err)
	}
	for _, t := range tasks {
		if err := upsertTask(tx, id, t, now); err != nil {
			return 0, err
		}
	}
	if err := tx.Commit(); err != nil {
		return 0, fmt.Errorf("create milestone: %w", err)
	}
	return id, nil
}

func upsertTask(tx *sql.Tx, id int64, t MilestoneTask, now time.Time) error {
	if _, err := tx.Exec(`INSERT INTO milestone_tasks (milestone_id, key, title, depends_on, covers, dropped, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?) ON CONFLICT(milestone_id, key) DO UPDATE SET title=excluded.title,
		depends_on=excluded.depends_on, covers=excluded.covers, dropped=excluded.dropped, updated_at=excluded.updated_at`,
		id, t.Key, t.Title, JoinNames(t.DependsOn), JoinNames(t.Covers), t.Dropped, now.Unix(), now.Unix()); err != nil {
		return fmt.Errorf("write task %s: %w", t.Key, err)
	}
	return nil
}

// GetMilestone returns one milestone (ErrMilestoneNotFound when there is none).
func (s *Store) GetMilestone(ctx context.Context, id int64) (*Milestone, error) {
	return getMilestone(ctx, s.db, id)
}

type queryer interface {
	QueryRowContext(ctx context.Context, q string, args ...any) *sql.Row
	QueryContext(ctx context.Context, q string, args ...any) (*sql.Rows, error)
}

func getMilestone(ctx context.Context, q queryer, id int64) (*Milestone, error) {
	ms, err := scanMilestone(q.QueryRowContext(ctx, `SELECT `+milestoneCols+` FROM milestones WHERE id = ?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrMilestoneNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("get milestone: %w", err)
	}
	return ms, nil
}

// MilestoneFilter selects milestones for ListMilestones; zero fields do not filter.
type MilestoneFilter struct {
	Status      string
	Repo        string
	ChecklistID string
	// UpdatedSince keeps the milestones whose updated_at is at or after it (the change cursor; equal
	// seconds are included, so a poller sees a change made in the second it last read).
	UpdatedSince *time.Time
}

// ListMilestones returns the milestones matching f, ordered by repo, then order, then creation.
func (s *Store) ListMilestones(ctx context.Context, f MilestoneFilter) ([]*Milestone, error) {
	q, args := `SELECT `+milestoneCols+` FROM milestones WHERE 1=1`, []any{}
	if f.Status != "" {
		q, args = q+` AND status = ?`, append(args, f.Status)
	}
	if f.Repo != "" {
		q, args = q+` AND repo = ?`, append(args, f.Repo)
	}
	if f.ChecklistID != "" {
		q, args = q+` AND checklist_id = ?`, append(args, f.ChecklistID)
	}
	if f.UpdatedSince != nil {
		q, args = q+` AND updated_at >= ?`, append(args, f.UpdatedSince.Unix())
	}
	rows, err := s.db.QueryContext(ctx, q+` ORDER BY repo, ord, created_at, id`, args...)
	if err != nil {
		return nil, fmt.Errorf("list milestones: %w", err)
	}
	defer rows.Close()
	out := []*Milestone{}
	for rows.Next() {
		ms, err := scanMilestone(rows)
		if err != nil {
			return nil, fmt.Errorf("scan milestone: %w", err)
		}
		out = append(out, ms)
	}
	return out, rows.Err()
}

// MilestoneCursor is the change cursor of milestone_list: the highest updated_at of all milestones (0
// without one). It is read before the list: a write not committed yet holds the write lock and is
// stamped at or after it, so updated_since = cursor never misses it (rows of the cursor's second come
// again).
func (s *Store) MilestoneCursor(ctx context.Context) (int64, error) {
	var n int64
	if err := s.db.QueryRowContext(ctx, `SELECT COALESCE(MAX(updated_at), 0) FROM milestones`).Scan(&n); err != nil {
		return 0, fmt.Errorf("milestone cursor: %w", err)
	}
	return n, nil
}

// MilestoneRepos returns the distinct repositories of the milestones (main worktrees, "" left out).
func (s *Store) MilestoneRepos(ctx context.Context) ([]string, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT DISTINCT repo FROM milestones WHERE repo != '' ORDER BY repo`)
	if err != nil {
		return nil, fmt.Errorf("milestone repos: %w", err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var r string
		if err := rows.Scan(&r); err != nil {
			return nil, fmt.Errorf("milestone repos: %w", err)
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// TouchChecklistMilestones moves updated_at of the milestones tied to checklist id of repo (its main
// worktree): their uncovered rows and acceptance follow the checklist file, which checklist_set wrote.
func (s *Store) TouchChecklistMilestones(repo, id string) error {
	if _, err := s.db.Exec(`UPDATE milestones SET updated_at = MAX(updated_at, `+sqlNow+`) WHERE repo = ? AND checklist_id = ?`,
		repo, id); err != nil {
		return fmt.Errorf("touch milestones: %w", err)
	}
	return nil
}

// MilestoneTasks returns the tasks of milestone id in the order they were created.
func (s *Store) MilestoneTasks(ctx context.Context, id int64) ([]MilestoneTask, error) {
	return milestoneTasks(ctx, s.db, id)
}

func milestoneTasks(ctx context.Context, q queryer, id int64) ([]MilestoneTask, error) {
	rows, err := q.QueryContext(ctx, `SELECT milestone_id, key, title, depends_on, covers, dropped, created_at, updated_at
		FROM milestone_tasks WHERE milestone_id = ? ORDER BY rowid`, id)
	if err != nil {
		return nil, fmt.Errorf("milestone tasks: %w", err)
	}
	defer rows.Close()
	var out []MilestoneTask
	for rows.Next() {
		var t MilestoneTask
		var deps, covers string
		var created, updated int64
		if err := rows.Scan(&t.MilestoneID, &t.Key, &t.Title, &deps, &covers, &t.Dropped, &created, &updated); err != nil {
			return nil, fmt.Errorf("scan task: %w", err)
		}
		t.DependsOn, t.Covers = SplitNames(deps), SplitNames(covers)
		t.CreatedAt, t.UpdatedAt = time.Unix(created, 0), time.Unix(updated, 0)
		out = append(out, t)
	}
	return out, rows.Err()
}

// MilestoneJobs returns the jobs attached to milestone id, oldest first.
func (s *Store) MilestoneJobs(ctx context.Context, id int64) ([]*Job, error) {
	return s.queryContext(ctx, `WHERE milestone_id = ? ORDER BY created_at, rowid`, id)
}

// MilestoneFull reports whether milestone id has a max_parallel and at least that many running jobs
// (the daemon then leaves its queued jobs queued this tick).
func (s *Store) MilestoneFull(id int64) (bool, error) {
	var full bool
	err := s.db.QueryRow(`SELECT max_parallel IS NOT NULL AND
		(SELECT COUNT(*) FROM jobs WHERE milestone_id = milestones.id AND status = 'running') >= max_parallel
		FROM milestones WHERE id = ?`, id).Scan(&full)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("milestone slots: %w", err)
	}
	return full, nil
}

// MilestoneEdit computes a milestone's new state from its current one: given the row, its tasks and
// its jobs (read in the same transaction), it returns the row to write and the tasks to upsert (by
// key), or an error that leaves everything unchanged.
type MilestoneEdit func(ms *Milestone, tasks []MilestoneTask, jobs []*Job) (*Milestone, []MilestoneTask, error)

// UpdateMilestone applies edit to milestone id in one transaction: the row's editable fields (title,
// goal, branch, order, status, max_parallel, closed_at, close_note) and the tasks edit returns are
// written, and updated_at moves to now (the database's clock, read under the write lock; now when later).
func (s *Store) UpdateMilestone(ctx context.Context, id int64, now time.Time, edit MilestoneEdit) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	cur, err := getMilestone(ctx, tx, id)
	if err != nil {
		return err
	}
	tasks, err := milestoneTasks(ctx, tx, id)
	if err != nil {
		return err
	}
	rows, err := tx.QueryContext(ctx, `SELECT `+cols+` FROM jobs WHERE milestone_id = ? ORDER BY created_at, rowid`, id)
	if err != nil {
		return fmt.Errorf("milestone jobs: %w", err)
	}
	var jobs []*Job
	for rows.Next() {
		j, err := scanJob(rows)
		if err != nil {
			rows.Close()
			return fmt.Errorf("scan job: %w", err)
		}
		jobs = append(jobs, j)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	next, upserts, err := edit(cur, tasks, jobs)
	if err != nil {
		return err
	}
	if _, err := tx.Exec(`UPDATE milestones SET title=?, goal=?, branch=?, ord=?, status=?, max_parallel=?, closed_at=?,
		close_note=?, updated_at=MAX(updated_at, ?, `+sqlNow+`) WHERE id=?`, next.Title, next.Goal, next.Branch, next.Order, next.Status,
		nullInt(next.MaxParallel), ts(next.ClosedAt), next.CloseNote, now.Unix(), id); err != nil {
		return fmt.Errorf("set milestone: %w", err)
	}
	for _, t := range upserts {
		if err := upsertTask(tx, id, t, now); err != nil {
			return err
		}
	}
	return tx.Commit()
}
