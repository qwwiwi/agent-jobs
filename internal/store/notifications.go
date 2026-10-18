package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// Notification is one row of the owner_notifications table: a notify_owner call. JobID is "" for
// a notification the orchestrator sent itself (NULL in the table).
type Notification struct {
	ID        int64     `json:"id"`
	JobID     string    `json:"job_id,omitempty"`
	Text      string    `json:"text"`
	Urgency   string    `json:"urgency"`
	CreatedAt time.Time `json:"created_at"`
	Delivered bool      `json:"delivered"`
	// Error is why the line did not reach the session ("" when delivered or not tried yet).
	Error string `json:"error,omitempty"`
}

// NotifyLimits bound notify_owner: per job a total and a minimum interval between two calls, per
// agent a count within the last hour. A zero field is no limit.
type NotifyLimits struct {
	PerJob       int
	JobInterval  time.Duration
	PerAgentHour int
}

// ErrNotifyLimit wraps every refusal by a NotifyLimits bound.
var ErrNotifyLimit = errors.New("notify_owner rate limit")

// notificationsSchema is applied on every Open (CREATE … IF NOT EXISTS): an older database gains
// the table and keeps everything else.
const notificationsSchema = `
CREATE TABLE IF NOT EXISTS owner_notifications (
	id         INTEGER PRIMARY KEY AUTOINCREMENT,
	job_id     TEXT,
	text       TEXT NOT NULL,
	urgency    TEXT NOT NULL DEFAULT 'normal',
	created_at INTEGER NOT NULL,
	delivered  INTEGER NOT NULL DEFAULT 0,
	error      TEXT NOT NULL DEFAULT ''
);
CREATE INDEX IF NOT EXISTS owner_notifications_job ON owner_notifications(job_id);
CREATE INDEX IF NOT EXISTS owner_notifications_created ON owner_notifications(created_at);
`

const notificationCols = `id, COALESCE(job_id, ''), text, urgency, created_at, delivered, error`

// AddNotification records a notification (not delivered yet) when the limits allow it; a refusal
// wraps ErrNotifyLimit. The counts and the insert run in one IMMEDIATE transaction on the store's
// only connection, so concurrent callers (the daemon's proxy and the orchestrator, other processes
// too) cannot both pass a limit with one slot left. Validation of text and urgency is the caller's.
func (s *Store) AddNotification(jobID, text, urgency string, now time.Time, lim NotifyLimits) (*Notification, error) {
	ctx := context.Background()
	conn, err := s.db.Conn(ctx)
	if err != nil {
		return nil, fmt.Errorf("add notification: %w", err)
	}
	defer conn.Close()
	if _, err := conn.ExecContext(ctx, `BEGIN IMMEDIATE`); err != nil {
		return nil, fmt.Errorf("add notification: %w", err)
	}
	committed := false
	defer func() {
		if !committed {
			_, _ = conn.ExecContext(ctx, `ROLLBACK`)
		}
	}()
	if lim.PerAgentHour > 0 {
		var n int
		if err := conn.QueryRowContext(ctx, `SELECT COUNT(*) FROM owner_notifications WHERE created_at > ?`,
			now.Add(-time.Hour).Unix()).Scan(&n); err != nil {
			return nil, fmt.Errorf("add notification: %w", err)
		}
		if n >= lim.PerAgentHour {
			return nil, fmt.Errorf("%w: this agent sent %d notifications in the last hour (at most %d)", ErrNotifyLimit, n, lim.PerAgentHour)
		}
	}
	if jobID != "" {
		var n int
		var last sql.NullInt64
		if err := conn.QueryRowContext(ctx, `SELECT COUNT(*), MAX(created_at) FROM owner_notifications WHERE job_id = ?`,
			jobID).Scan(&n, &last); err != nil {
			return nil, fmt.Errorf("add notification: %w", err)
		}
		if lim.PerJob > 0 && n >= lim.PerJob {
			return nil, fmt.Errorf("%w: job %s already sent %d notifications (at most %d per job)", ErrNotifyLimit, jobID, n, lim.PerJob)
		}
		if lim.JobInterval > 0 && last.Valid && now.Sub(time.Unix(last.Int64, 0)) < lim.JobInterval {
			return nil, fmt.Errorf("%w: job %s sent a notification less than %s ago (at most one per %s)",
				ErrNotifyLimit, jobID, lim.JobInterval, lim.JobInterval)
		}
	}
	var job any
	if jobID != "" {
		job = jobID
	}
	res, err := conn.ExecContext(ctx, `INSERT INTO owner_notifications (job_id, text, urgency, created_at) VALUES (?, ?, ?, ?)`,
		job, text, urgency, now.Unix())
	if err != nil {
		return nil, fmt.Errorf("add notification: %w", err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		return nil, fmt.Errorf("add notification: %w", err)
	}
	if _, err := conn.ExecContext(ctx, `COMMIT`); err != nil {
		return nil, fmt.Errorf("add notification: %w", err)
	}
	committed = true
	return &Notification{ID: id, JobID: jobID, Text: text, Urgency: urgency, CreatedAt: time.Unix(now.Unix(), 0)}, nil
}

// SetNotificationResult records the delivery of a notification: delivered, or not with the error.
func (s *Store) SetNotificationResult(id int64, delivered bool, errText string) error {
	return affected(s.db.Exec(`UPDATE owner_notifications SET delivered = ?, error = ? WHERE id = ?`, delivered, errText, id))
}

func scanNotification(r scanner) (*Notification, error) {
	var n Notification
	var created int64
	if err := r.Scan(&n.ID, &n.JobID, &n.Text, &n.Urgency, &created, &n.Delivered, &n.Error); err != nil {
		return nil, err
	}
	n.CreatedAt = time.Unix(created, 0)
	return &n, nil
}

// GetNotification returns one notification.
func (s *Store) GetNotification(id int64) (*Notification, error) {
	n, err := scanNotification(s.db.QueryRow(`SELECT `+notificationCols+` FROM owner_notifications WHERE id = ?`, id))
	if err != nil {
		return nil, fmt.Errorf("get notification %d: %w", id, err)
	}
	return n, nil
}

// UndeliveredNotifications returns the notifications created since `since` that did not reach the
// session, oldest first.
func (s *Store) UndeliveredNotifications(since time.Time) ([]*Notification, error) {
	rows, err := s.db.Query(`SELECT `+notificationCols+` FROM owner_notifications
		WHERE delivered = 0 AND created_at >= ? ORDER BY id`, since.Unix())
	if err != nil {
		return nil, fmt.Errorf("undelivered notifications: %w", err)
	}
	defer rows.Close()
	out := []*Notification{}
	for rows.Next() {
		n, err := scanNotification(rows)
		if err != nil {
			return nil, fmt.Errorf("scan notification: %w", err)
		}
		out = append(out, n)
	}
	return out, rows.Err()
}
