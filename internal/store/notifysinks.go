package store

import (
	"database/sql"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"
)

// States of a notify_sink_outbox row. A row goes pending -> running (claimed) -> done or failed and
// never back: a claimed row is never handed out again, so the sinks of one batch run at most once.
const (
	// SinkPending: queued with the acknowledgement of its line, not claimed yet.
	SinkPending = "pending"
	// SinkRunning: claimed by a sink run (ClaimSinkDelivery) that has not finished it yet.
	SinkRunning = "running"
	// SinkDone: every configured sink took the line.
	SinkDone = "done"
	// SinkFailed: a sink failed, or the run was interrupted (InterruptStaleSinkDeliveries).
	SinkFailed = "failed"
)

// SinkJob is one job of a sink delivery, at the attempt the line is about.
type SinkJob struct {
	ID      string
	Attempt int
}

// SinkDelivery is one job-done line queued for the sinks of config notify (notify.command,
// notify.inbox_dir): a row of notify_sink_outbox.
type SinkDelivery struct {
	// Ref is the batch identity (the jobs, their attempts and finish times), the row's key.
	Ref  string
	Line string
	Jobs []SinkJob
	// Owner is set when the sinks alone carry the line (no tmux target): their success marks the
	// owner-waits attempts of Jobs delivered, a failure leaves them delivering.
	Owner bool
	// Noted are the SLO breaches whose note rides on Line: marked noted in the transaction that
	// inserts the row, so a rolled-back insert keeps them pending for the next line. Not stored.
	Noted     []int64
	State     string
	Error     string
	CreatedAt time.Time
	UpdatedAt time.Time
}

// sinkOutboxSchema is applied on every Open (CREATE … IF NOT EXISTS).
const sinkOutboxSchema = `
CREATE TABLE IF NOT EXISTS notify_sink_outbox (
	ref        TEXT PRIMARY KEY,
	line       TEXT NOT NULL,
	jobs       TEXT NOT NULL DEFAULT '',
	owner      INTEGER NOT NULL DEFAULT 0,
	state      TEXT NOT NULL,
	error      TEXT NOT NULL DEFAULT '',
	created_at INTEGER NOT NULL,
	updated_at INTEGER NOT NULL
);
CREATE INDEX IF NOT EXISTS notify_sink_outbox_state ON notify_sink_outbox(state, created_at);
`

const sinkCols = `ref, line, jobs, owner, state, error, created_at, updated_at`

// queueSinkDeliveryTx inserts d as a pending row unless its Ref is queued already, and marks the
// breaches its line carries (d.Noted) noted only when it inserted the row: an existing row keeps its
// own line, so the breaches of this one were not queued and stay pending.
func queueSinkDeliveryTx(tx *sql.Tx, d *SinkDelivery, now time.Time) error {
	if d.Ref == "" {
		return errors.New("queue sink delivery: empty ref")
	}
	jobs := make([]string, 0, len(d.Jobs))
	for _, j := range d.Jobs {
		jobs = append(jobs, j.ID+"@"+strconv.Itoa(j.Attempt))
	}
	res, err := tx.Exec(`INSERT INTO notify_sink_outbox (`+sinkCols+`) VALUES (?, ?, ?, ?, ?, '', ?, ?)
		ON CONFLICT(ref) DO NOTHING`, d.Ref, d.Line, strings.Join(jobs, ","), boolInt(d.Owner), SinkPending,
		now.Unix(), now.Unix())
	if err != nil {
		return fmt.Errorf("queue sink delivery: %w", err)
	}
	inserted, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("queue sink delivery: %w", err)
	}
	if inserted == 0 {
		return nil
	}
	return markSLOBreachesNoted(tx, d.Noted, now)
}

func boolInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

func scanSinkDelivery(row interface{ Scan(...any) error }) (*SinkDelivery, error) {
	var d SinkDelivery
	var jobs string
	var owner int
	var created, updated int64
	if err := row.Scan(&d.Ref, &d.Line, &jobs, &owner, &d.State, &d.Error, &created, &updated); err != nil {
		return nil, err
	}
	for _, s := range strings.Split(jobs, ",") {
		if s == "" {
			continue
		}
		id, att, ok := strings.Cut(s, "@")
		n, err := strconv.Atoi(att)
		if !ok || err != nil {
			return nil, fmt.Errorf("sink delivery %s: bad job %q", d.Ref, s)
		}
		d.Jobs = append(d.Jobs, SinkJob{ID: id, Attempt: n})
	}
	d.Owner = owner == 1
	d.CreatedAt, d.UpdatedAt = time.Unix(created, 0), time.Unix(updated, 0)
	return &d, nil
}

// ClaimSinkDelivery marks the oldest pending row running and returns it; nil when none is pending.
// One statement, so two processes never claim the same row.
func (s *Store) ClaimSinkDelivery(now time.Time) (*SinkDelivery, error) {
	d, err := scanSinkDelivery(s.db.QueryRow(`UPDATE notify_sink_outbox SET state = ?, updated_at = ?
		WHERE ref = (SELECT ref FROM notify_sink_outbox WHERE state = ? ORDER BY created_at, ref LIMIT 1)
			AND state = ?
		RETURNING `+sinkCols, SinkRunning, now.Unix(), SinkPending, SinkPending))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("claim sink delivery: %w", err)
	}
	return d, nil
}

// FinishSinkDelivery records the outcome of a claimed row: done with errText "", failed otherwise.
// A done row that carried the line alone (Owner) marks its jobs' owner-waits attempts delivered, in
// the same transaction; an attempt acknowledged or expired meanwhile keeps its state.
func (s *Store) FinishSinkDelivery(ref, errText string, now time.Time) error {
	tx, err := s.db.Begin()
	if err != nil {
		return fmt.Errorf("finish sink delivery: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	state := SinkDone
	if errText != "" {
		state = SinkFailed
	}
	d, err := scanSinkDelivery(tx.QueryRow(`UPDATE notify_sink_outbox SET state = ?, error = ?, updated_at = ?
		WHERE ref = ? AND state = ? RETURNING `+sinkCols, state, errText, now.Unix(), ref, SinkRunning))
	if errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("finish sink delivery %s: not running", ref)
	}
	if err != nil {
		return fmt.Errorf("finish sink delivery: %w", err)
	}
	if state == SinkDone && d.Owner {
		for _, j := range d.Jobs {
			if _, err := tx.Exec(`UPDATE owner_pending SET state = ?, created_at = ?
				WHERE job_id = ? AND attempt = ? AND state = ?`, OwnerDelivered, now.Unix(), j.ID, j.Attempt, OwnerDelivering); err != nil {
				return fmt.Errorf("finish sink delivery: owner pending: %w", err)
			}
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("finish sink delivery: %w", err)
	}
	return nil
}

// InterruptStaleSinkDeliveries fails the rows claimed before cutoff and never finished: the process
// that claimed them ended (a crash, a kill) part way. They are not run again, since a sink may have
// taken the line already; their owner-waits attempts stay delivering. Returns the refs it failed.
func (s *Store) InterruptStaleSinkDeliveries(cutoff, now time.Time) ([]string, error) {
	rows, err := s.db.Query(`UPDATE notify_sink_outbox SET state = ?, error = ?, updated_at = ?
		WHERE state = ? AND updated_at < ? RETURNING ref`, SinkFailed,
		"interrupted: the run that claimed it ended before it finished; not run again", now.Unix(), SinkRunning, cutoff.Unix())
	if err != nil {
		return nil, fmt.Errorf("interrupt sink deliveries: %w", err)
	}
	defer rows.Close()
	var refs []string
	for rows.Next() {
		var ref string
		if err := rows.Scan(&ref); err != nil {
			return nil, fmt.Errorf("interrupt sink deliveries: %w", err)
		}
		refs = append(refs, ref)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("interrupt sink deliveries: %w", err)
	}
	return refs, nil
}

// PruneSinkDeliveries deletes the finished rows (done, failed) last changed before cutoff. A batch
// is retried only while its jobs are not notified, and they are marked notified together with the
// row's insert, so a pruned row's batch is never queued again.
func (s *Store) PruneSinkDeliveries(cutoff time.Time) error {
	if _, err := s.db.Exec(`DELETE FROM notify_sink_outbox WHERE state IN (?, ?) AND updated_at < ?`,
		SinkDone, SinkFailed, cutoff.Unix()); err != nil {
		return fmt.Errorf("prune sink deliveries: %w", err)
	}
	return nil
}

// GetSinkDelivery returns the row of ref.
func (s *Store) GetSinkDelivery(ref string) (*SinkDelivery, error) {
	d, err := scanSinkDelivery(s.db.QueryRow(`SELECT `+sinkCols+` FROM notify_sink_outbox WHERE ref = ?`, ref))
	if err != nil {
		return nil, fmt.Errorf("sink delivery %s: %w", ref, err)
	}
	return d, nil
}
