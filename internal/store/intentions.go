package store

import (
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// Intention statuses. An intention is never deleted: it ends done or dropped and can be reopened.
const (
	IntentionOpen    = "open"
	IntentionDone    = "done"
	IntentionDropped = "dropped"
)

// ErrIntentionNotFound is returned when an intention id does not exist.
var ErrIntentionNotFound = errors.New("intention not found")

// Intention is one row of the intentions table: one of the agent's own goals (the database is
// per agent, so every row is this agent's).
type Intention struct {
	ID     int64  `json:"id"`
	Text   string `json:"text"`
	Status string `json:"status"`
	// Due is a local date YYYY-MM-DD, "" when none was given.
	Due string `json:"due,omitempty"`
	// Priority is low | normal | high.
	Priority  string     `json:"priority"`
	CreatedAt time.Time  `json:"created_at"`
	UpdatedAt time.Time  `json:"updated_at"`
	ClosedAt  *time.Time `json:"closed_at,omitempty"`
}

// intentionsSchema is applied on every Open (CREATE … IF NOT EXISTS): an older database gains the
// table and keeps everything else.
const intentionsSchema = `
CREATE TABLE IF NOT EXISTS intentions (
	id         INTEGER PRIMARY KEY AUTOINCREMENT,
	text       TEXT NOT NULL,
	status     TEXT NOT NULL CHECK (status IN ('open','done','dropped')),
	due        TEXT NOT NULL DEFAULT '',
	priority   TEXT NOT NULL DEFAULT 'normal',
	created_at INTEGER NOT NULL,
	updated_at INTEGER NOT NULL,
	closed_at  INTEGER
);
CREATE INDEX IF NOT EXISTS intentions_status ON intentions(status);
`

const intentionCols = `id, text, status, due, priority, created_at, updated_at, closed_at`

// AddIntention inserts an open intention and returns it with its id. Validation is the caller's.
func (s *Store) AddIntention(text, due, priority string, now time.Time) (*Intention, error) {
	res, err := s.db.Exec(`INSERT INTO intentions (text, status, due, priority, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?)`, text, IntentionOpen, due, priority, now.Unix(), now.Unix())
	if err != nil {
		return nil, fmt.Errorf("add intention: %w", err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		return nil, fmt.Errorf("add intention: %w", err)
	}
	return s.GetIntention(id)
}

func scanIntention(r scanner) (*Intention, error) {
	var it Intention
	var created, updated int64
	var closed sql.NullInt64
	if err := r.Scan(&it.ID, &it.Text, &it.Status, &it.Due, &it.Priority, &created, &updated, &closed); err != nil {
		return nil, err
	}
	it.CreatedAt, it.UpdatedAt = time.Unix(created, 0), time.Unix(updated, 0)
	if closed.Valid {
		t := time.Unix(closed.Int64, 0)
		it.ClosedAt = &t
	}
	return &it, nil
}

// GetIntention returns one intention (ErrIntentionNotFound when there is none).
func (s *Store) GetIntention(id int64) (*Intention, error) {
	it, err := scanIntention(s.db.QueryRow(`SELECT `+intentionCols+` FROM intentions WHERE id = ?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrIntentionNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("get intention: %w", err)
	}
	return it, nil
}

// ListIntentions returns the intentions with status (all of them for ""), oldest first.
func (s *Store) ListIntentions(status string) ([]*Intention, error) {
	q, args := `SELECT `+intentionCols+` FROM intentions ORDER BY id`, []any{}
	if status != "" {
		q, args = `SELECT `+intentionCols+` FROM intentions WHERE status = ? ORDER BY id`, []any{status}
	}
	rows, err := s.db.Query(q, args...)
	if err != nil {
		return nil, fmt.Errorf("list intentions: %w", err)
	}
	defer rows.Close()
	out := []*Intention{}
	for rows.Next() {
		it, err := scanIntention(rows)
		if err != nil {
			return nil, fmt.Errorf("scan intention: %w", err)
		}
		out = append(out, it)
	}
	return out, rows.Err()
}

// SetIntention edits an intention in place: a non-empty text or status replaces the old one.
// Leaving open sets closed_at, reopening clears it; the same status keeps it.
func (s *Store) SetIntention(id int64, text, status string, now time.Time) (*Intention, error) {
	res, err := s.db.Exec(`UPDATE intentions SET
		text       = CASE WHEN ?1 = '' THEN text ELSE ?1 END,
		closed_at  = CASE WHEN ?2 = '' OR ?2 = status THEN closed_at WHEN ?2 = 'open' THEN NULL ELSE ?3 END,
		status     = CASE WHEN ?2 = '' THEN status ELSE ?2 END,
		updated_at = ?3
		WHERE id = ?4`, text, status, now.Unix(), id)
	if err != nil {
		return nil, fmt.Errorf("set intention: %w", err)
	}
	if n, err := res.RowsAffected(); err != nil {
		return nil, fmt.Errorf("set intention: %w", err)
	} else if n == 0 {
		return nil, ErrIntentionNotFound
	}
	return s.GetIntention(id)
}
