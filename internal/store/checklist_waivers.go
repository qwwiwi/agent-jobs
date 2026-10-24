package store

import (
	"errors"
	"fmt"
	"time"
)

// ErrWaiverIncomplete is a waiver without a repo, a checklist hash, a sha, a requirement or a quote.
var ErrWaiverIncomplete = errors.New("waiver needs repo, checklist hash, sha, requirement and the owner's quote")

// Waiver is an owner waiver for one requirement of an approved checklist (Wave F, F7): written only by
// checklist_set with the owner's quote, bound to the checklist hash and the commit sha, so a new
// checklist version or another sha voids it.
type Waiver struct {
	Repo            string    `json:"repo"`
	ChecklistID     string    `json:"checklist_id"`
	ChecklistSHA256 string    `json:"checklist_sha256"`
	SHA             string    `json:"sha"`
	Requirement     string    `json:"requirement"`
	Quote           string    `json:"quote"`
	CreatedAt       time.Time `json:"created_at"`
}

// waiversSchema is applied on every Open (CREATE … IF NOT EXISTS). One waiver per requirement, checklist
// hash and sha; a second one replaces the first (a new quote).
const waiversSchema = `
CREATE TABLE IF NOT EXISTS checklist_waivers (
	repo             TEXT NOT NULL,
	checklist_id     TEXT NOT NULL,
	checklist_sha256 TEXT NOT NULL,
	sha              TEXT NOT NULL,
	requirement      TEXT NOT NULL,
	quote            TEXT NOT NULL,
	created_at       INTEGER NOT NULL,
	PRIMARY KEY (repo, checklist_sha256, sha, requirement)
);
`

// PutWaiver stores (or replaces) a waiver.
func (s *Store) PutWaiver(w Waiver) error {
	if w.Repo == "" || w.ChecklistSHA256 == "" || w.SHA == "" || w.Requirement == "" || w.Quote == "" {
		return ErrWaiverIncomplete
	}
	_, err := s.db.Exec(`INSERT OR REPLACE INTO checklist_waivers (repo, checklist_id, checklist_sha256, sha, requirement,
		quote, created_at) VALUES (?,?,?,?,?,?,?)`, w.Repo, w.ChecklistID, w.ChecklistSHA256, w.SHA, w.Requirement, w.Quote,
		w.CreatedAt.Unix())
	if err != nil {
		return fmt.Errorf("put waiver: %w", err)
	}
	return nil
}

// Waivers lists the waivers of repo bound to checklist hash, by sha then requirement.
func (s *Store) Waivers(repo, checklistSHA256 string) ([]Waiver, error) {
	rows, err := s.db.Query(`SELECT repo, checklist_id, checklist_sha256, sha, requirement, quote, created_at
		FROM checklist_waivers WHERE repo = ? AND checklist_sha256 = ? ORDER BY sha, requirement`, repo, checklistSHA256)
	if err != nil {
		return nil, fmt.Errorf("waivers: %w", err)
	}
	defer rows.Close()
	var out []Waiver
	for rows.Next() {
		var w Waiver
		var at int64
		if err := rows.Scan(&w.Repo, &w.ChecklistID, &w.ChecklistSHA256, &w.SHA, &w.Requirement, &w.Quote, &at); err != nil {
			return nil, fmt.Errorf("waivers: %w", err)
		}
		w.CreatedAt = time.Unix(at, 0).UTC()
		out = append(out, w)
	}
	return out, rows.Err()
}
