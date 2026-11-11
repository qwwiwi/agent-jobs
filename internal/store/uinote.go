package store

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
)

// uiNoteAlter adds the daemon's job-done note of a job (jobs.ui_note): NULL (no note) or a JSON object
// {"attempt":N,"note":"..."} naming the attempt the note was worked out for. The column is nullable with
// no default and is named by no other statement (not in the job columns, not in Insert), so a binary
// that does not know it reads and writes the same rows unchanged.
const uiNoteAlter = `ALTER TABLE jobs ADD COLUMN ui_note TEXT`

// storedUINote is the value of jobs.ui_note.
type storedUINote struct {
	Attempt int    `json:"attempt"`
	Note    string `json:"note"`
}

// uiNoteLive is the WHERE clause (after id=?) of an attempt that has not ended yet, as FinishAttempt
// takes it: with a launch token only that running launch, without one a job that is not terminal.
func uiNoteLive(token string) (string, []any) {
	if token == "" {
		return `status IN ('queued','running','stopped')`, nil
	}
	return `status='running' AND launch_token=?`, []any{token}
}

// ClearUINote removes the job's stored note before its ending attempt works out a new one. It only
// touches an attempt that has not ended (token as in FinishAttempt): ErrImmutable otherwise, so a
// finish repeated for an ended attempt leaves the stored note alone. Its guard is the live status and
// the launch token only: it does not compare the attempt number (SetUINote does).
func (s *Store) ClearUINote(id, token string) error {
	live, args := uiNoteLive(token)
	return affected(s.db.Exec(`UPDATE jobs SET ui_note=NULL WHERE id=? AND `+live, append([]any{id}, args...)...))
}

// SetUINote stores note as the job-done note of attempt of job id. Like ClearUINote it only writes to an
// attempt that has not ended, and only when the row still is at that attempt: ErrImmutable otherwise.
func (s *Store) SetUINote(id, token string, attempt int, note string) error {
	if note == "" {
		return errors.New("ui note: empty")
	}
	raw, err := json.Marshal(storedUINote{Attempt: attempt, Note: note})
	if err != nil {
		return fmt.Errorf("ui note: %w", err)
	}
	live, args := uiNoteLive(token)
	return affected(s.db.Exec(`UPDATE jobs SET ui_note=? WHERE id=? AND attempt=? AND `+live,
		append([]any{string(raw), id, attempt}, args...)...))
}

// UINote is the job's stored note when it was worked out for attempt: "" when there is none, when it
// belongs to another attempt, or when the stored value is not readable.
func (s *Store) UINote(id string, attempt int) (string, error) {
	var raw sql.NullString
	if err := s.db.QueryRow(`SELECT ui_note FROM jobs WHERE id=?`, id).Scan(&raw); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return "", nil
		}
		return "", err
	}
	if !raw.Valid || raw.String == "" {
		return "", nil
	}
	var n storedUINote
	if err := json.Unmarshal([]byte(raw.String), &n); err != nil {
		return "", fmt.Errorf("ui note of job %s: %w", id, err)
	}
	if n.Attempt != attempt {
		return "", nil
	}
	return n.Note, nil
}
