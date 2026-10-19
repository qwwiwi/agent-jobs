package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"
)

// JobStats is one row of the job_stats table: the summary of a finished job, written when the job
// reaches a terminal status (or by a backfill from its job dir or the log archive).
type JobStats struct {
	ID    string `json:"id"`
	Kind  string `json:"kind"`
	Model string `json:"model"`
	// Effort is the reasoning effort the job ran at ("" when it was not recorded).
	Effort  string  `json:"effort,omitempty"`
	Status  string  `json:"status"`
	Turns   int     `json:"turns"`
	CostUSD float64 `json:"cost_usd"`
	// CostUnknown: no attempt of the job reported its cost (a codex job); CostUSD is then not its cost,
	// and the summaries leave it out of the cost figures.
	CostUnknown bool       `json:"cost_unknown,omitempty"`
	DurationSec int        `json:"duration_sec"`
	CreatedAt   time.Time  `json:"created_at"`
	StartedAt   *time.Time `json:"started_at,omitempty"`
	FinishedAt  *time.Time `json:"finished_at,omitempty"`
	// Repo is the basename of the repository path only ("" for a job without a repo).
	Repo string `json:"repo,omitempty"`
	// ToolCalls counts tool calls by tool name (subagents' calls included).
	ToolCalls map[string]int `json:"tool_calls"`
	// Skills counts uses by skill name: Skill tool calls and reads under a skills/<name>/ dir.
	Skills map[string]int `json:"skills"`
	// SkillsDelivered are the catalog skills linked into the job's work dir (E7), used or not.
	SkillsDelivered []string `json:"skills_delivered,omitempty"`
	// GuardRefusals is the number of tool calls the guard refused; GuardRules counts them by rule.
	GuardRefusals int            `json:"guard_refusals"`
	GuardRules    map[string]int `json:"guard_rules"`
	Subagents     int            `json:"subagents"`
	HasReport     bool           `json:"has_report"`
	// ReportOffContract: the job's report lacks the Summary or the Not done / uncertain section.
	ReportOffContract bool `json:"report_off_contract,omitempty"`
	// ResultSubtype is the subtype of the last `result` event ("" when there was none).
	ResultSubtype string `json:"result_subtype,omitempty"`
	// Source is where the stream was read from: "job_dir", "archive" or "none".
	Source     string    `json:"source"`
	RecordedAt time.Time `json:"recorded_at"`
	// DirBytes is the size of the job directory when the job ended (worktree of a repo job left out).
	DirBytes int64 `json:"dir_bytes"`
}

const statsSchema = `
CREATE TABLE IF NOT EXISTS job_stats (
	id             TEXT PRIMARY KEY,
	kind           TEXT NOT NULL,
	model          TEXT NOT NULL,
	status         TEXT NOT NULL,
	turns          INTEGER NOT NULL DEFAULT 0,
	cost_usd       REAL NOT NULL DEFAULT 0,
	duration_sec   INTEGER NOT NULL DEFAULT 0,
	created_at     INTEGER NOT NULL,
	started_at     INTEGER,
	finished_at    INTEGER,
	repo           TEXT NOT NULL DEFAULT '',
	tool_calls     TEXT NOT NULL DEFAULT '{}',
	skills         TEXT NOT NULL DEFAULT '{}',
	guard_refusals INTEGER NOT NULL DEFAULT 0,
	guard_rules    TEXT NOT NULL DEFAULT '{}',
	subagents      INTEGER NOT NULL DEFAULT 0,
	has_report     INTEGER NOT NULL DEFAULT 0,
	result_subtype TEXT NOT NULL DEFAULT '',
	source         TEXT NOT NULL DEFAULT '',
	recorded_at    INTEGER NOT NULL
);
CREATE INDEX IF NOT EXISTS job_stats_finished ON job_stats(finished_at);
`

const statsCols = `id, kind, model, status, turns, cost_usd, duration_sec, created_at, started_at, finished_at,
	repo, tool_calls, skills, guard_refusals, guard_rules, subagents, has_report, result_subtype, source, recorded_at,
	cost_unknown, skills_delivered, effort, report_off_contract, dir_bytes`

func countsJSON(m map[string]int) (string, error) {
	if m == nil {
		m = map[string]int{}
	}
	b, err := json.Marshal(m)
	return string(b), err
}

// PutStats writes (or replaces) the stats row of a job.
func (s *Store) PutStats(st *JobStats) error {
	var cols [3]string
	for i, m := range []map[string]int{st.ToolCalls, st.Skills, st.GuardRules} {
		v, err := countsJSON(m)
		if err != nil {
			return fmt.Errorf("put stats: %w", err)
		}
		cols[i] = v
	}
	_, err := s.db.Exec(`INSERT OR REPLACE INTO job_stats (`+statsCols+`) VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		st.ID, st.Kind, st.Model, st.Status, st.Turns, st.CostUSD, st.DurationSec, st.CreatedAt.Unix(),
		ts(st.StartedAt), ts(st.FinishedAt), st.Repo, cols[0], cols[1], st.GuardRefusals, cols[2],
		st.Subagents, st.HasReport, st.ResultSubtype, st.Source, st.RecordedAt.Unix(), st.CostUnknown,
		JoinNames(st.SkillsDelivered), st.Effort, st.ReportOffContract, st.DirBytes)
	if err != nil {
		return fmt.Errorf("put stats: %w", err)
	}
	return nil
}

func scanStats(r scanner) (*JobStats, error) {
	var st JobStats
	var created, recorded int64
	var started, finished sql.NullInt64
	var tools, skills, rules, delivered string
	if err := r.Scan(&st.ID, &st.Kind, &st.Model, &st.Status, &st.Turns, &st.CostUSD, &st.DurationSec, &created,
		&started, &finished, &st.Repo, &tools, &skills, &st.GuardRefusals, &rules, &st.Subagents, &st.HasReport,
		&st.ResultSubtype, &st.Source, &recorded, &st.CostUnknown, &delivered, &st.Effort, &st.ReportOffContract,
		&st.DirBytes); err != nil {
		return nil, err
	}
	st.SkillsDelivered = SplitNames(delivered)
	st.CreatedAt, st.RecordedAt = time.Unix(created, 0), time.Unix(recorded, 0)
	if started.Valid {
		t := time.Unix(started.Int64, 0)
		st.StartedAt = &t
	}
	if finished.Valid {
		t := time.Unix(finished.Int64, 0)
		st.FinishedAt = &t
	}
	for _, c := range []struct {
		raw string
		dst *map[string]int
	}{{tools, &st.ToolCalls}, {skills, &st.Skills}, {rules, &st.GuardRules}} {
		*c.dst = map[string]int{}
		if err := json.Unmarshal([]byte(c.raw), c.dst); err != nil {
			return nil, fmt.Errorf("stats %s: %w", st.ID, err)
		}
	}
	return &st, nil
}

// GetStats returns the stats row of a job (ErrNotFound when there is none).
func (s *Store) GetStats(id string) (*JobStats, error) {
	st, err := scanStats(s.db.QueryRow(`SELECT `+statsCols+` FROM job_stats WHERE id = ?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("get stats: %w", err)
	}
	return st, nil
}

// ListStats returns the stats rows of jobs finished at or after since (all rows for a zero since),
// oldest first.
func (s *Store) ListStats(since time.Time) ([]*JobStats, error) {
	return s.ListStatsContext(context.Background(), since)
}

// ListStatsContext is ListStats, given up when ctx ends.
func (s *Store) ListStatsContext(ctx context.Context, since time.Time) ([]*JobStats, error) {
	q, args := `SELECT `+statsCols+` FROM job_stats ORDER BY finished_at, id`, []any{}
	if !since.IsZero() {
		q, args = `SELECT `+statsCols+` FROM job_stats WHERE finished_at >= ? ORDER BY finished_at, id`, []any{since.Unix()}
	}
	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("list stats: %w", err)
	}
	defer rows.Close()
	var out []*JobStats
	for rows.Next() {
		st, err := scanStats(rows)
		if err != nil {
			return nil, fmt.Errorf("scan stats: %w", err)
		}
		out = append(out, st)
	}
	return out, rows.Err()
}

// ListTerminal returns every job in a terminal status, oldest first.
func (s *Store) ListTerminal() ([]*Job, error) {
	return s.query(`WHERE status NOT IN ('queued','running') ORDER BY created_at, rowid`)
}
