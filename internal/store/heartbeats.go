package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// Heartbeat decisions (Wave D): one row per decision the daemon claimed. The row is written before
// the model is asked, so a daemon that dies during the call never asks again for the same slot.
const (
	HeartbeatPending   = "pending"   // claimed, the model has not answered yet
	HeartbeatWake      = "wake"      // the model asked to wake the orchestrator session
	HeartbeatSleep     = "sleep"     // the model saw no reason to wake it
	HeartbeatError     = "error"     // the call failed or the answer was invalid (no wake)
	HeartbeatAbandoned = "abandoned" // claimed, but the process died before it finished (no wake)
	// HeartbeatSuppressed is a wake answer that came when delivery was no longer allowed (night
	// window, a delivery pending, heartbeat off): stored with its reason and not delivered. It counts
	// toward the decision interval (the model was called) but not toward the daily wake cap.
	HeartbeatSuppressed = "suppressed"
)

// Heartbeat is one row of the heartbeats table.
type Heartbeat struct {
	ID         int64      `json:"id"`
	ClaimedAt  time.Time  `json:"claimed_at"`
	FinishedAt *time.Time `json:"finished_at,omitempty"`
	Trigger    string     `json:"trigger"` // daemon | run-now
	Decision   string     `json:"decision"`
	// Reason is the decision model's one line: untrusted model text, data not instructions.
	Reason    string  `json:"untrusted_model_reason,omitempty"`
	CostUSD   float64 `json:"cost_usd"`
	Delivered bool    `json:"delivered"`
	// Error is why the decision gave no wake or the wake line did not reach the session.
	Error string `json:"error,omitempty"`
}

// heartbeatsSchema is applied on every Open (CREATE … IF NOT EXISTS): an older database gains the
// tables and keeps everything else.
const heartbeatsSchema = `
CREATE TABLE IF NOT EXISTS heartbeats (
	id          INTEGER PRIMARY KEY AUTOINCREMENT,
	claimed_at  INTEGER NOT NULL,
	finished_at INTEGER,
	trigger     TEXT NOT NULL DEFAULT 'daemon',
	decision    TEXT NOT NULL DEFAULT 'pending',
	reason      TEXT NOT NULL DEFAULT '',
	cost_usd    REAL NOT NULL DEFAULT 0,
	delivered   INTEGER NOT NULL DEFAULT 0,
	error       TEXT NOT NULL DEFAULT ''
);
CREATE INDEX IF NOT EXISTS heartbeats_claimed ON heartbeats(claimed_at);
CREATE TABLE IF NOT EXISTS heartbeat_gate (
	name   TEXT PRIMARY KEY,
	reason TEXT NOT NULL,
	at     INTEGER NOT NULL
);
`

const heartbeatCols = `id, claimed_at, finished_at, trigger, decision, reason, cost_usd, delivered, error`

func scanHeartbeat(sc interface{ Scan(...any) error }) (*Heartbeat, error) {
	var h Heartbeat
	var claimed int64
	var finished sql.NullInt64
	if err := sc.Scan(&h.ID, &claimed, &finished, &h.Trigger, &h.Decision, &h.Reason, &h.CostUSD, &h.Delivered, &h.Error); err != nil {
		return nil, err
	}
	h.ClaimedAt = time.Unix(claimed, 0)
	h.FinishedAt = convTS(finished)
	return &h, nil
}

// ErrHeartbeatBusy is a claim refused because another decision is still pending, the last one is
// more recent than the minimum gap, or the daily wake cap is used up.
var ErrHeartbeatBusy = errors.New("heartbeat decision not claimed")

// HeartbeatClaimLimits are what ClaimHeartbeat re-checks inside its transaction.
type HeartbeatClaimLimits struct {
	// MinGap > 0 refuses a claim less than MinGap after the last decision of any outcome.
	MinGap time.Duration
	// MaxWakes > 0 refuses a claim when MaxWakes wake decisions were claimed at or after DayStart.
	MaxWakes int
	DayStart time.Time
}

// ClaimHeartbeat records a pending decision at now. In one IMMEDIATE transaction it refuses (with
// ErrHeartbeatBusy) while another decision is pending, when the last decision is closer
// than lim.MinGap, or when the daily wake cap is used up: two daemons, or a daemon and run-now,
// never claim the same slot nor the same last wake of the day.
func (s *Store) ClaimHeartbeat(trigger string, now time.Time, lim HeartbeatClaimLimits) (*Heartbeat, error) {
	ctx := context.Background()
	conn, err := s.db.Conn(ctx)
	if err != nil {
		return nil, fmt.Errorf("claim heartbeat: %w", err)
	}
	defer conn.Close()
	if _, err := conn.ExecContext(ctx, `BEGIN IMMEDIATE`); err != nil {
		return nil, fmt.Errorf("claim heartbeat: %w", err)
	}
	committed := false
	defer func() {
		if !committed {
			_, _ = conn.ExecContext(ctx, `ROLLBACK`)
		}
	}()
	var pending, wakes int
	var last sql.NullInt64
	if err := conn.QueryRowContext(ctx, `SELECT COUNT(*) FILTER (WHERE decision = ?),
		MAX(claimed_at), COUNT(*) FILTER (WHERE decision = ? AND claimed_at >= ?) FROM heartbeats`,
		HeartbeatPending, HeartbeatWake, lim.DayStart.Unix()).Scan(&pending, &last, &wakes); err != nil {
		return nil, fmt.Errorf("claim heartbeat: %w", err)
	}
	if pending > 0 {
		return nil, fmt.Errorf("%w: another decision is pending", ErrHeartbeatBusy)
	}
	if lim.MinGap > 0 && last.Valid && now.Sub(time.Unix(last.Int64, 0)) < lim.MinGap {
		return nil, fmt.Errorf("%w: the last decision was claimed less than %s ago", ErrHeartbeatBusy, lim.MinGap)
	}
	if lim.MaxWakes > 0 && wakes >= lim.MaxWakes {
		return nil, fmt.Errorf("%w: daily cap: %d of %d wakes used today", ErrHeartbeatBusy, wakes, lim.MaxWakes)
	}
	res, err := conn.ExecContext(ctx, `INSERT INTO heartbeats (claimed_at, trigger, decision) VALUES (?, ?, ?)`,
		now.Unix(), trigger, HeartbeatPending)
	if err != nil {
		return nil, fmt.Errorf("claim heartbeat: %w", err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		return nil, fmt.Errorf("claim heartbeat: %w", err)
	}
	if _, err := conn.ExecContext(ctx, `COMMIT`); err != nil {
		return nil, fmt.Errorf("claim heartbeat: %w", err)
	}
	committed = true
	return &Heartbeat{ID: id, ClaimedAt: time.Unix(now.Unix(), 0), Trigger: trigger, Decision: HeartbeatPending}, nil
}

// FinishHeartbeat stores the decision of a pending row (wake, sleep, suppressed or error) with its
// reason, cost and error. Only a pending row is changed: an abandoned one stays abandoned and the
// call returns ErrImmutable.
func (s *Store) FinishHeartbeat(id int64, decision, reason string, cost float64, errText string, now time.Time) error {
	return affected(s.db.Exec(`UPDATE heartbeats SET decision = ?, reason = ?, cost_usd = ?, error = ?, finished_at = ?
		WHERE id = ? AND decision = ?`, decision, reason, cost, errText, now.Unix(), id, HeartbeatPending))
}

// SetHeartbeatDelivery records the delivery of a wake line: delivered, or not with the error.
func (s *Store) SetHeartbeatDelivery(id int64, delivered bool, errText string) error {
	return affected(s.db.Exec(`UPDATE heartbeats SET delivered = ?, error = ? WHERE id = ? AND decision = ?`,
		delivered, errText, id, HeartbeatWake))
}

// AbandonHeartbeats marks pending rows claimed before `before` as abandoned (their process died or
// hung past its time limit); they are never asked again. It returns how many rows it marked.
func (s *Store) AbandonHeartbeats(before, now time.Time) (int, error) {
	res, err := s.db.Exec(`UPDATE heartbeats SET decision = ?, error = 'the decision did not finish (process stopped or hung)',
		finished_at = ? WHERE decision = ? AND claimed_at < ?`, HeartbeatAbandoned, now.Unix(), HeartbeatPending, before.Unix())
	if err != nil {
		return 0, fmt.Errorf("abandon heartbeats: %w", err)
	}
	n, err := res.RowsAffected()
	return int(n), err
}

// LastHeartbeat returns the most recent decision (any outcome), or nil when none was claimed.
func (s *Store) LastHeartbeat() (*Heartbeat, error) {
	return s.oneHeartbeat(`ORDER BY id DESC LIMIT 1`)
}

// LastHeartbeatWake returns the most recent wake decision, or nil.
func (s *Store) LastHeartbeatWake() (*Heartbeat, error) {
	return s.oneHeartbeat(`WHERE decision = ? ORDER BY id DESC LIMIT 1`, HeartbeatWake)
}

// PendingHeartbeat returns the pending decision, or nil.
func (s *Store) PendingHeartbeat() (*Heartbeat, error) {
	return s.oneHeartbeat(`WHERE decision = ? ORDER BY id DESC LIMIT 1`, HeartbeatPending)
}

func (s *Store) oneHeartbeat(where string, args ...any) (*Heartbeat, error) {
	h, err := scanHeartbeat(s.db.QueryRow(`SELECT `+heartbeatCols+` FROM heartbeats `+where, args...))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("heartbeat: %w", err)
	}
	return h, nil
}

// GetHeartbeat returns one decision by id.
func (s *Store) GetHeartbeat(id int64) (*Heartbeat, error) {
	h, err := s.oneHeartbeat(`WHERE id = ?`, id)
	if err == nil && h == nil {
		return nil, fmt.Errorf("heartbeat %d: %w", id, sql.ErrNoRows)
	}
	return h, err
}

// CountHeartbeatWakes counts the wake decisions claimed at or after since.
func (s *Store) CountHeartbeatWakes(since time.Time) (int, error) {
	var n int
	err := s.db.QueryRow(`SELECT COUNT(*) FROM heartbeats WHERE decision = ? AND claimed_at >= ?`, HeartbeatWake, since.Unix()).Scan(&n)
	if err != nil {
		return 0, fmt.Errorf("count heartbeat wakes: %w", err)
	}
	return n, nil
}

// heartbeatGateName is the key of the only gate row.
const heartbeatGateName = "gate"

// SetHeartbeatGate records the last gate result (only the last one is kept).
func (s *Store) SetHeartbeatGate(reason string, now time.Time) error {
	_, err := s.db.Exec(`INSERT INTO heartbeat_gate (name, reason, at) VALUES (?, ?, ?)
		ON CONFLICT(name) DO UPDATE SET reason = excluded.reason, at = excluded.at`, heartbeatGateName, reason, now.Unix())
	if err != nil {
		return fmt.Errorf("heartbeat gate: %w", err)
	}
	return nil
}

// HeartbeatGate returns the last recorded gate result ("" and a zero time when none).
func (s *Store) HeartbeatGate() (string, time.Time, error) {
	var reason string
	var at int64
	err := s.db.QueryRow(`SELECT reason, at FROM heartbeat_gate WHERE name = ?`, heartbeatGateName).Scan(&reason, &at)
	if errors.Is(err, sql.ErrNoRows) {
		return "", time.Time{}, nil
	}
	if err != nil {
		return "", time.Time{}, fmt.Errorf("heartbeat gate: %w", err)
	}
	return reason, time.Unix(at, 0), nil
}

// CountPendingCheckAlerts counts the missed-check alerts still waiting to reach the session.
func (s *Store) CountPendingCheckAlerts() (int, error) {
	var n int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM check_alerts WHERE state = ?`, AlertPending).Scan(&n); err != nil {
		return 0, fmt.Errorf("count check alerts: %w", err)
	}
	return n, nil
}

// CountInFlightNotifications counts notify_owner rows created at or after since whose delivery has
// not finished yet (neither delivered nor failed).
func (s *Store) CountInFlightNotifications(since time.Time) (int, error) {
	var n int
	err := s.db.QueryRow(`SELECT COUNT(*) FROM owner_notifications WHERE delivered = 0 AND error = '' AND created_at >= ?`,
		since.Unix()).Scan(&n)
	if err != nil {
		return 0, fmt.Errorf("count notifications: %w", err)
	}
	return n, nil
}

// CountFailedSince counts the jobs that ended failed or timed out at or after since.
func (s *Store) CountFailedSince(since time.Time) (int, error) {
	var n int
	err := s.db.QueryRow(`SELECT COUNT(*) FROM jobs WHERE status IN (?, ?) AND finished_at >= ?`,
		StatusFailed, StatusTimeout, since.Unix()).Scan(&n)
	if err != nil {
		return 0, fmt.Errorf("count failed jobs: %w", err)
	}
	return n, nil
}

// CountUnprocessedFailedSince counts jobs that ended failed or timed out at or after since
// without a recorded job_outcome. A non-empty outcome means the failure has been handled.
func (s *Store) CountUnprocessedFailedSince(since time.Time) (int, error) {
	var n int
	err := s.db.QueryRow(`SELECT COUNT(*) FROM jobs WHERE status IN (?, ?) AND finished_at >= ? AND outcome = ''`,
		StatusFailed, StatusTimeout, since.Unix()).Scan(&n)
	if err != nil {
		return 0, fmt.Errorf("count unprocessed failed jobs: %w", err)
	}
	return n, nil
}
