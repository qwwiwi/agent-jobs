package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"time"
)

// Crew is a crew job's setup (issue #169): the main loop runs on the job's (cheap) model and may ask a
// stronger advisor model a few times at a design fork (advisor_ask). PairedWith optionally names a plain
// job on the same brief to compare with in job_stats. nil on every other job.
type Crew struct {
	AdvisorModel        string  `json:"advisor_model,omitempty"`
	AdvisorMaxCalls     int     `json:"advisor_max_calls,omitempty"`
	AdvisorMaxBudgetUSD float64 `json:"advisor_max_budget_usd,omitempty"`
	PairedWith          string  `json:"paired_with,omitempty"`
}

// HasAdvisor reports whether the crew has an advisor model (a job with only paired_with has none).
func (c *Crew) HasAdvisor() bool { return c != nil && c.AdvisorModel != "" }

// crewMigrations add the crew column. Applied at the end of Open. Never edit or reorder a row.
var crewMigrations = []string{
	`ALTER TABLE jobs ADD COLUMN crew TEXT NOT NULL DEFAULT ''`,
}

// advisorSchema is applied on every Open (CREATE … IF NOT EXISTS).
const advisorSchema = `
CREATE TABLE IF NOT EXISTS advisor_calls (
	job_id        TEXT NOT NULL,
	n             INTEGER NOT NULL,
	turn          INTEGER NOT NULL DEFAULT 0,
	why           TEXT NOT NULL DEFAULT '',
	model         TEXT NOT NULL DEFAULT '',
	budget_usd    REAL NOT NULL DEFAULT 0,
	started_at    INTEGER NOT NULL,
	ended_at      INTEGER,
	outcome       TEXT NOT NULL DEFAULT '',
	turns         INTEGER NOT NULL DEFAULT 0,
	cost_usd      REAL NOT NULL DEFAULT 0,
	cost_unknown  INTEGER NOT NULL DEFAULT 0,
	duration_sec  INTEGER NOT NULL DEFAULT 0,
	error         TEXT NOT NULL DEFAULT '',
	PRIMARY KEY (job_id, n)
);
`

// migrateCrew applies crewMigrations and the advisor_calls table.
func migrateCrew(db *sql.DB) error {
	for _, alter := range crewMigrations {
		if err := migrate(db, alter); err != nil {
			return err
		}
	}
	_, err := db.Exec(advisorSchema)
	return err
}

// crewJSON is the crew column: "" for none.
func crewJSON(c *Crew) string {
	if c == nil {
		return ""
	}
	b, err := json.Marshal(c)
	if err != nil {
		return "" // strings, ints and floats always marshal
	}
	return string(b)
}

// parseCrew reads the crew column ("" = nil).
func parseCrew(s string) (*Crew, error) {
	if s == "" {
		return nil, nil
	}
	c := &Crew{}
	if err := json.Unmarshal([]byte(s), c); err != nil {
		return nil, err
	}
	return c, nil
}

// Advisor call outcomes.
const (
	AdvisorRunning  = "running"
	AdvisorAnswered = "answered"
	AdvisorFailed   = "failed"
	// AdvisorStale is a call whose row was never finished (the daemon stopped mid-call); it counts at
	// its whole budget.
	AdvisorStale = "stale"
)

// AdvisorCall is one advisor_ask of a crew job. UntrustedWhy is the job's question (model text,
// cut), never instructions.
type AdvisorCall struct {
	JobID        string     `json:"job_id"`
	N            int        `json:"n"`
	Turn         int        `json:"turn"`
	UntrustedWhy string     `json:"untrusted_why"`
	Model        string     `json:"model"`
	BudgetUSD    float64    `json:"budget_usd"`
	StartedAt    time.Time  `json:"started_at"`
	EndedAt      *time.Time `json:"ended_at,omitempty"`
	Outcome      string     `json:"outcome"`
	Turns        int        `json:"turns"`
	CostUSD      float64    `json:"cost_usd"`
	CostUnknown  bool       `json:"cost_unknown,omitempty"`
	DurationSec  int        `json:"duration_sec"`
	Error        string     `json:"error,omitempty"`
}

// AdvisorClaim bounds one claim: the crew's caps and how long an unfinished row counts as in flight.
type AdvisorClaim struct {
	Turn      int
	Why       string
	Model     string
	MaxCalls  int
	MaxBudget float64
	// StaleAfter: an unfinished row older than this is no longer in flight (a daemon stopped mid-call).
	StaleAfter time.Duration
}

// ErrAdvisorLimit wraps every refusal of an advisor call by a crew cap.
var ErrAdvisorLimit = errors.New("advisor limit")

// advisorSpendSQL is what a job's advisor calls spent or may still spend: a known cost, else the
// call's whole budget (in flight, failed without a cost, stale).
const advisorSpendSQL = `SELECT COUNT(*), COALESCE(SUM(CASE WHEN ended_at IS NULL OR cost_unknown = 1 THEN budget_usd ELSE cost_usd END), 0),
	COALESCE(SUM(CASE WHEN ended_at IS NULL AND started_at > ? THEN 1 ELSE 0 END), 0) FROM advisor_calls WHERE job_id = ?`

// ClaimAdvisorCall records the start of an advisor call of running job id when its caps allow one: no
// other call of the job in flight, fewer than MaxCalls calls, and spend below MaxBudget. The call
// gets an equal share of the budget left over the calls left, (MaxBudget - spend) / (MaxCalls - calls
// made): a call whose cost stays unknown counts at its whole budget, and a share keeps one such call
// (a timeout, a daemon stopped mid-call) from using up the calls after it. Counts and insert run in one IMMEDIATE
// transaction, so two concurrent calls cannot both pass a cap. A refusal wraps ErrAdvisorLimit.
func (s *Store) ClaimAdvisorCall(id string, now time.Time, c AdvisorClaim) (*AdvisorCall, error) {
	ctx := context.Background()
	conn, err := s.db.Conn(ctx)
	if err != nil {
		return nil, fmt.Errorf("claim advisor call: %w", err)
	}
	defer conn.Close()
	if _, err := conn.ExecContext(ctx, `BEGIN IMMEDIATE`); err != nil {
		return nil, fmt.Errorf("claim advisor call: %w", err)
	}
	committed := false
	defer func() {
		if !committed {
			_, _ = conn.ExecContext(ctx, `ROLLBACK`)
		}
	}()
	var status string
	if err := conn.QueryRowContext(ctx, `SELECT status FROM jobs WHERE id = ?`, id).Scan(&status); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, fmt.Errorf("claim advisor call: %w", err)
	}
	if status != StatusRunning {
		return nil, fmt.Errorf("claim advisor call: job %s is %s, not running", id, status)
	}
	var n, inFlight int
	var spent float64
	if err := conn.QueryRowContext(ctx, advisorSpendSQL, now.Add(-c.StaleAfter).Unix(), id).Scan(&n, &spent, &inFlight); err != nil {
		return nil, fmt.Errorf("claim advisor call: %w", err)
	}
	switch {
	case inFlight > 0:
		return nil, fmt.Errorf("%w: another advisor call of this job is still running (one at a time)", ErrAdvisorLimit)
	case n >= c.MaxCalls:
		return nil, fmt.Errorf("%w: this job made %d of its %d advisor calls (advisor_max_calls); decide without the advisor",
			ErrAdvisorLimit, n, c.MaxCalls)
	case spent >= c.MaxBudget:
		return nil, fmt.Errorf("%w: the advisor calls of this job spent or may still spend $%.2f of $%.2f (advisor_max_budget_usd); decide without the advisor",
			ErrAdvisorLimit, spent, c.MaxBudget)
	}
	// The share, rounded to a millionth of a dollar (a clean --max-budget-usd); below a cent there is
	// nothing left to ask with.
	share := math.Round((c.MaxBudget-spent)/float64(c.MaxCalls-n)*1e6) / 1e6
	if share < 0.01 {
		return nil, fmt.Errorf("%w: the advisor calls of this job spent or may still spend $%.2f of $%.2f (advisor_max_budget_usd); decide without the advisor",
			ErrAdvisorLimit, spent, c.MaxBudget)
	}
	call := &AdvisorCall{JobID: id, N: n + 1, Turn: c.Turn, UntrustedWhy: c.Why, Model: c.Model,
		BudgetUSD: share, StartedAt: time.Unix(now.Unix(), 0), Outcome: AdvisorRunning}
	if _, err := conn.ExecContext(ctx, `INSERT INTO advisor_calls (job_id, n, turn, why, model, budget_usd, started_at)
		VALUES (?, ?, ?, ?, ?, ?, ?)`, id, call.N, call.Turn, call.UntrustedWhy, call.Model, call.BudgetUSD, now.Unix()); err != nil {
		return nil, fmt.Errorf("claim advisor call: %w", err)
	}
	if _, err := conn.ExecContext(ctx, `COMMIT`); err != nil {
		return nil, fmt.Errorf("claim advisor call: %w", err)
	}
	committed = true
	return call, nil
}

// FinishAdvisorCall records how call n of job id ended.
func (s *Store) FinishAdvisorCall(c *AdvisorCall) error {
	if c.EndedAt == nil {
		return errors.New("finish advisor call: no end time")
	}
	_, err := s.db.Exec(`UPDATE advisor_calls SET ended_at = ?, outcome = ?, turns = ?, cost_usd = ?, cost_unknown = ?,
		duration_sec = ?, error = ? WHERE job_id = ? AND n = ?`, c.EndedAt.Unix(), c.Outcome, c.Turns, c.CostUSD,
		c.CostUnknown, c.DurationSec, c.Error, c.JobID, c.N)
	if err != nil {
		return fmt.Errorf("finish advisor call: %w", err)
	}
	return nil
}

// AdvisorCalls lists the advisor calls of job id, oldest first. An unfinished row older than
// staleAfter is shown as stale, at its whole budget (cost unknown).
func (s *Store) AdvisorCalls(id string, now time.Time, staleAfter time.Duration) ([]AdvisorCall, error) {
	rows, err := s.db.Query(`SELECT job_id, n, turn, why, model, budget_usd, started_at, ended_at, outcome, turns,
		cost_usd, cost_unknown, duration_sec, error FROM advisor_calls WHERE job_id = ? ORDER BY n`, id)
	if err != nil {
		return nil, fmt.Errorf("advisor calls: %w", err)
	}
	defer rows.Close()
	out := []AdvisorCall{}
	for rows.Next() {
		var c AdvisorCall
		var started int64
		var ended sql.NullInt64
		if err := rows.Scan(&c.JobID, &c.N, &c.Turn, &c.UntrustedWhy, &c.Model, &c.BudgetUSD, &started, &ended,
			&c.Outcome, &c.Turns, &c.CostUSD, &c.CostUnknown, &c.DurationSec, &c.Error); err != nil {
			return nil, fmt.Errorf("advisor calls: %w", err)
		}
		c.StartedAt, c.EndedAt = time.Unix(started, 0), convTS(ended)
		if c.EndedAt == nil {
			c.Outcome = AdvisorRunning
			if !c.StartedAt.After(now.Add(-staleAfter)) {
				c.Outcome, c.CostUnknown = AdvisorStale, true
			}
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// AdvisorSpend sums calls the way the caps count them: a known cost, else the call's whole budget.
func AdvisorSpend(calls []AdvisorCall) float64 {
	var v float64
	for _, c := range calls {
		if c.EndedAt == nil || c.CostUnknown {
			v += c.BudgetUSD
		} else {
			v += c.CostUSD
		}
	}
	return v
}

// CrewJobs lists the jobs that have a crew (an advisor or a paired_with), oldest first.
func (s *Store) CrewJobs(ctx context.Context) ([]*Job, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT `+cols+` FROM jobs WHERE crew != '' ORDER BY created_at, id`)
	if err != nil {
		return nil, fmt.Errorf("crew jobs: %w", err)
	}
	defer rows.Close()
	var out []*Job
	for rows.Next() {
		j, err := scanJob(rows)
		if err != nil {
			return nil, fmt.Errorf("crew jobs: %w", err)
		}
		out = append(out, j)
	}
	return out, rows.Err()
}
