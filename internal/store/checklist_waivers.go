package store

import (
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// ErrWaiverIncomplete is a waiver without a repo, a checklist hash and version, a sha, a requirement or
// a quote.
var ErrWaiverIncomplete = errors.New("waiver needs repo, checklist hash and version, sha, requirement and the owner's quote")

// ErrApprovalIncomplete is an approval without a repo, a checklist id, version and hash, the BRIEF hash
// or the owner's quote.
var ErrApprovalIncomplete = errors.New("approval needs repo, checklist id, version, checklist hash, BRIEF hash and the owner's quote")

// Waiver is an owner waiver for one requirement of an approved checklist (Wave F, F7): written only by
// checklist_set with the owner's quote, bound to the checklist hash and version and the commit sha, so
// a new checklist version or another sha voids it.
type Waiver struct {
	Repo             string    `json:"repo"`
	ChecklistID      string    `json:"checklist_id"`
	ChecklistSHA256  string    `json:"checklist_sha256"`
	ChecklistVersion int       `json:"checklist_version"`
	SHA              string    `json:"sha"`
	Requirement      string    `json:"requirement"`
	Quote            string    `json:"quote"`
	CreatedAt        time.Time `json:"created_at"`
}

// ChecklistApproval is an owner approval of a checklist (Wave F, F2), written only by checklist_set
// approve. It is the only source of «approved»: the checklist hash is unkeyed, so the approved line in
// CHECKLIST.md, which anyone editing the file can recompute, is informational. A checklist is approved
// only while its hash and version are those of the latest approval of its repository.
type ChecklistApproval struct {
	// Repo is the repository's main worktree (the key, as a job's repo_path).
	Repo        string `json:"repo"`
	ChecklistID string `json:"checklist_id"`
	Version     int    `json:"version"`
	// ChecklistSHA256 is the approved checklist hash; BriefSHA256 the sha256 of BRIEF.md's text above
	// «## Additions» at the approval (that text is never edited afterwards).
	ChecklistSHA256 string    `json:"checklist_sha256"`
	BriefSHA256     string    `json:"brief_sha256"`
	Quote           string    `json:"quote"`
	ApprovedAt      time.Time `json:"approved_at"`
}

// waiversSchema is applied on every Open (CREATE … IF NOT EXISTS). One waiver per requirement, checklist
// hash and version, and sha; a second one replaces the first (a new quote). Approvals are a log: the
// latest one of a repository (highest seq) is the one in force.
const waiversSchema = `
CREATE TABLE IF NOT EXISTS checklist_waivers (
	repo              TEXT NOT NULL,
	checklist_id      TEXT NOT NULL,
	checklist_sha256  TEXT NOT NULL,
	checklist_version INTEGER NOT NULL DEFAULT 0,
	sha               TEXT NOT NULL,
	requirement       TEXT NOT NULL,
	quote             TEXT NOT NULL,
	created_at        INTEGER NOT NULL,
	PRIMARY KEY (repo, checklist_sha256, checklist_version, sha, requirement)
);
CREATE TABLE IF NOT EXISTS checklist_approvals (
	seq              INTEGER PRIMARY KEY AUTOINCREMENT,
	repo             TEXT NOT NULL,
	checklist_id     TEXT NOT NULL,
	version          INTEGER NOT NULL,
	checklist_sha256 TEXT NOT NULL,
	brief_sha256     TEXT NOT NULL,
	quote            TEXT NOT NULL,
	approved_at      INTEGER NOT NULL
);
CREATE INDEX IF NOT EXISTS checklist_approvals_repo ON checklist_approvals(repo, seq);
`

// waiversMigrations add columns to a checklist_waivers table created before them.
var waiversMigrations = []string{
	`ALTER TABLE checklist_waivers ADD COLUMN checklist_version INTEGER NOT NULL DEFAULT 0`,
}

// PutWaiver stores (or replaces) a waiver.
func (s *Store) PutWaiver(w Waiver) error {
	if w.Repo == "" || w.ChecklistSHA256 == "" || w.ChecklistVersion < 1 || w.SHA == "" || w.Requirement == "" || w.Quote == "" {
		return ErrWaiverIncomplete
	}
	_, err := s.db.Exec(`INSERT OR REPLACE INTO checklist_waivers (repo, checklist_id, checklist_sha256, checklist_version, sha,
		requirement, quote, created_at) VALUES (?,?,?,?,?,?,?,?)`, w.Repo, w.ChecklistID, w.ChecklistSHA256, w.ChecklistVersion,
		w.SHA, w.Requirement, w.Quote, w.CreatedAt.Unix())
	if err != nil {
		return fmt.Errorf("put waiver: %w", err)
	}
	return nil
}

// Waivers lists the waivers of repo bound to checklist hash and version, by sha then requirement.
func (s *Store) Waivers(repo, checklistSHA256 string, version int) ([]Waiver, error) {
	rows, err := s.db.Query(`SELECT repo, checklist_id, checklist_sha256, checklist_version, sha, requirement, quote, created_at
		FROM checklist_waivers WHERE repo = ? AND checklist_sha256 = ? AND checklist_version = ? ORDER BY sha, requirement`,
		repo, checklistSHA256, version)
	if err != nil {
		return nil, fmt.Errorf("waivers: %w", err)
	}
	defer rows.Close()
	var out []Waiver
	for rows.Next() {
		var w Waiver
		var at int64
		if err := rows.Scan(&w.Repo, &w.ChecklistID, &w.ChecklistSHA256, &w.ChecklistVersion, &w.SHA, &w.Requirement, &w.Quote, &at); err != nil {
			return nil, fmt.Errorf("waivers: %w", err)
		}
		w.CreatedAt = time.Unix(at, 0).UTC()
		out = append(out, w)
	}
	return out, rows.Err()
}

// PutApproval appends an approval; it becomes the repository's latest.
func (s *Store) PutApproval(a ChecklistApproval) error {
	if a.Repo == "" || a.ChecklistID == "" || a.Version < 1 || a.ChecklistSHA256 == "" || a.BriefSHA256 == "" || a.Quote == "" {
		return ErrApprovalIncomplete
	}
	_, err := s.db.Exec(`INSERT INTO checklist_approvals (repo, checklist_id, version, checklist_sha256, brief_sha256, quote,
		approved_at) VALUES (?,?,?,?,?,?,?)`, a.Repo, a.ChecklistID, a.Version, a.ChecklistSHA256, a.BriefSHA256, a.Quote,
		a.ApprovedAt.Unix())
	if err != nil {
		return fmt.Errorf("put approval: %w", err)
	}
	return nil
}

// LatestApproval is the latest approval of repo, nil when it has none.
func (s *Store) LatestApproval(repo string) (*ChecklistApproval, error) {
	var a ChecklistApproval
	var at int64
	err := s.db.QueryRow(`SELECT repo, checklist_id, version, checklist_sha256, brief_sha256, quote, approved_at
		FROM checklist_approvals WHERE repo = ? ORDER BY seq DESC LIMIT 1`, repo).Scan(&a.Repo, &a.ChecklistID, &a.Version,
		&a.ChecklistSHA256, &a.BriefSHA256, &a.Quote, &at)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("latest approval: %w", err)
	}
	a.ApprovedAt = time.Unix(at, 0).UTC()
	return &a, nil
}
