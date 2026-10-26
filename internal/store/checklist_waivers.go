package store

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
)

// ErrWaiverIncomplete is a waiver without a repo, a checklist hash and version, a requirement or a quote.
var ErrWaiverIncomplete = errors.New("waiver needs repo, checklist hash and version, requirement and the owner's quote")

// ErrApprovalIncomplete is an approval without a repo, a checklist id, version and hash, the BRIEF hash
// or the owner's quote.
var ErrApprovalIncomplete = errors.New("approval needs repo, checklist id, version, checklist hash, BRIEF hash and the owner's quote")

// Waiver is an owner waiver for one requirement of an approved checklist (Wave F, F7): written only by
// checklist_set with the owner's quote, bound to the checklist hash and version and the requirement, so a
// new checklist version voids it and a new commit does not (owner decision 2026-10-25). SHA is the commit
// the caller named, kept for the record only ("" = none named); release_check does not read it.
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
	// Seq orders the approvals of all repositories (read only; PutApproval assigns it).
	Seq int64 `json:"-"`
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
	// Rows are the checklist rows as approved and BriefFileSHA256 the sha256 of the whole BRIEF.md (with
	// its Additions) at the approval: what a later version is compared with, so the owner re-reads only
	// the rows that changed (F8). Neither is hashed or checked; both are empty for an approval stored
	// before they were recorded.
	Rows            []GateRow `json:"rows,omitempty"`
	BriefFileSHA256 string    `json:"brief_file_sha256,omitempty"`
}

// ChecklistInit is the record checklist_init stores for a new checklist id of a repository: the sha256
// of the BRIEF text above «## Additions» it was created with. An id is used once per repository.
type ChecklistInit struct {
	Repo        string    `json:"repo"`
	ChecklistID string    `json:"checklist_id"`
	BriefSHA256 string    `json:"brief_sha256"`
	CreatedAt   time.Time `json:"created_at"`
}

// ChecklistBinding is what the store holds for one checklist id of a repository: the BRIEF prefix
// hash of its latest approval (Approved) or, before any, of its init. checklist_set works only on an
// id with a binding, and only while BRIEF.md's prefix matches it.
type ChecklistBinding struct {
	BriefSHA256 string
	Approved    bool
}

// ErrInitIncomplete is an init record without a repo, a checklist id or the BRIEF hash.
var ErrInitIncomplete = errors.New("checklist init needs repo, checklist id and BRIEF hash")

// waiversKey is the primary key of checklist_waivers since the checklist id joined it (two ids with
// identical content share a hash and version, and keep their own waivers).
const waiversKey = "PRIMARY KEY (repo, checklist_id, checklist_sha256, checklist_version, sha, requirement)"

// waiversColumns is the column list of checklist_waivers (CREATE TABLE <name> waiversColumns).
const waiversColumns = `(
	repo              TEXT NOT NULL,
	checklist_id      TEXT NOT NULL,
	checklist_sha256  TEXT NOT NULL,
	checklist_version INTEGER NOT NULL DEFAULT 0,
	sha               TEXT NOT NULL,
	requirement       TEXT NOT NULL,
	quote             TEXT NOT NULL,
	created_at        INTEGER NOT NULL,
	` + waiversKey + `
)`

// waiversSchema is applied on every Open (CREATE … IF NOT EXISTS). One waiver per requirement, checklist
// id, hash and version, and sha; a second one replaces the first (a new quote). Approvals are a log: the
// latest one of a repository (highest seq) is the one in force. Inits: one per checklist id of a
// repository, written by checklist_init.
const waiversSchema = `
CREATE TABLE IF NOT EXISTS checklist_waivers ` + waiversColumns + `;
CREATE TABLE IF NOT EXISTS checklist_approvals (
	seq              INTEGER PRIMARY KEY AUTOINCREMENT,
	repo             TEXT NOT NULL,
	checklist_id     TEXT NOT NULL,
	version          INTEGER NOT NULL,
	checklist_sha256 TEXT NOT NULL,
	brief_sha256     TEXT NOT NULL,
	quote            TEXT NOT NULL,
	approved_at      INTEGER NOT NULL,
	rows             TEXT NOT NULL DEFAULT '',
	brief_file_sha256 TEXT NOT NULL DEFAULT ''
);
CREATE INDEX IF NOT EXISTS checklist_approvals_repo ON checklist_approvals(repo, seq);
CREATE TABLE IF NOT EXISTS checklist_inits (
	repo         TEXT NOT NULL,
	checklist_id TEXT NOT NULL,
	brief_sha256 TEXT NOT NULL,
	created_at   INTEGER NOT NULL,
	PRIMARY KEY (repo, checklist_id)
);
`

// waiversMigrations add columns to a checklist_waivers or checklist_approvals table created before them.
var waiversMigrations = []string{
	`ALTER TABLE checklist_waivers ADD COLUMN checklist_version INTEGER NOT NULL DEFAULT 0`,
	`ALTER TABLE checklist_approvals ADD COLUMN rows TEXT NOT NULL DEFAULT ''`,
	`ALTER TABLE checklist_approvals ADD COLUMN brief_file_sha256 TEXT NOT NULL DEFAULT ''`,
}

// migrateWaiversKey rebuilds a checklist_waivers table created before checklist_version or checklist_id
// joined its primary key (the version column itself is added by waiversMigrations; every row always had
// its checklist_id): the table is recreated with the current key and its rows copied, keeping each row's
// id and version (0 for a row from before the column), all in one IMMEDIATE transaction that re-checks the
// definition (several processes open the database). The new key is a superset of every old one, so no
// row is lost.
func migrateWaiversKey(db *sql.DB) error {
	def := func() (string, error) {
		var q string
		err := db.QueryRow(`SELECT sql FROM sqlite_master WHERE type='table' AND name='checklist_waivers'`).Scan(&q)
		return q, err
	}
	q, err := def()
	if err != nil || strings.Contains(q, waiversKey) {
		return err
	}
	// One connection (SetMaxOpenConns(1)): the statements below run inside this transaction.
	if _, err := db.Exec(`BEGIN IMMEDIATE`); err != nil {
		return err
	}
	rollback := func(err error) error {
		_, _ = db.Exec(`ROLLBACK`)
		return err
	}
	if q, err = def(); err != nil {
		return rollback(err)
	}
	if strings.Contains(q, waiversKey) {
		return rollback(nil)
	}
	const cols = "repo, checklist_id, checklist_sha256, checklist_version, sha, requirement, quote, created_at"
	for _, stmt := range []string{`CREATE TABLE checklist_waivers_rebuild ` + waiversColumns,
		`INSERT OR REPLACE INTO checklist_waivers_rebuild (` + cols + `) SELECT ` + cols + ` FROM checklist_waivers`,
		`DROP TABLE checklist_waivers`, `ALTER TABLE checklist_waivers_rebuild RENAME TO checklist_waivers`} {
		if _, err := db.Exec(stmt); err != nil {
			return rollback(fmt.Errorf("rebuild checklist_waivers: %w", err))
		}
	}
	_, err = db.Exec(`COMMIT`)
	return err
}

// PutWaiver stores (or replaces) a waiver.
func (s *Store) PutWaiver(w Waiver) error {
	if w.Repo == "" || w.ChecklistSHA256 == "" || w.ChecklistVersion < 1 || w.Requirement == "" || w.Quote == "" {
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

// Waivers lists the waivers of repo bound to checklist id, hash and version, oldest first (created time,
// then insertion: a replaced waiver is a new row), so the last one of a requirement is its newest.
func (s *Store) Waivers(repo, checklistID, checklistSHA256 string, version int) ([]Waiver, error) {
	rows, err := s.db.Query(`SELECT repo, checklist_id, checklist_sha256, checklist_version, sha, requirement, quote, created_at
		FROM checklist_waivers WHERE repo = ? AND checklist_id = ? AND checklist_sha256 = ? AND checklist_version = ?
		ORDER BY created_at, rowid`, repo, checklistID, checklistSHA256, version)
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
	rows := ""
	if a.Rows != nil {
		b, err := json.Marshal(a.Rows)
		if err != nil {
			return fmt.Errorf("put approval: %w", err)
		}
		rows = string(b)
	}
	_, err := s.db.Exec(`INSERT INTO checklist_approvals (repo, checklist_id, version, checklist_sha256, brief_sha256, quote,
		approved_at, rows, brief_file_sha256) VALUES (?,?,?,?,?,?,?,?,?)`, a.Repo, a.ChecklistID, a.Version, a.ChecklistSHA256,
		a.BriefSHA256, a.Quote, a.ApprovedAt.Unix(), rows, a.BriefFileSHA256)
	if err != nil {
		return fmt.Errorf("put approval: %w", err)
	}
	return nil
}

// LatestApproval is the latest approval of repo, nil when it has none.
func (s *Store) LatestApproval(repo string) (*ChecklistApproval, error) {
	var a ChecklistApproval
	var at int64
	var rows string
	err := s.db.QueryRow(`SELECT seq, repo, checklist_id, version, checklist_sha256, brief_sha256, quote, approved_at, rows,
		brief_file_sha256 FROM checklist_approvals WHERE repo = ? ORDER BY seq DESC LIMIT 1`, repo).Scan(&a.Seq, &a.Repo,
		&a.ChecklistID, &a.Version, &a.ChecklistSHA256, &a.BriefSHA256, &a.Quote, &at, &rows, &a.BriefFileSHA256)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("latest approval: %w", err)
	}
	if rows != "" {
		if err := json.Unmarshal([]byte(rows), &a.Rows); err != nil {
			return nil, fmt.Errorf("latest approval %d: rows: %w", a.Seq, err)
		}
	}
	a.ApprovedAt = time.Unix(at, 0).UTC()
	return &a, nil
}

// PutInit stores the init record of a new checklist id; an id already recorded for the repository is
// refused (an error).
func (s *Store) PutInit(in ChecklistInit) error {
	if in.Repo == "" || in.ChecklistID == "" || in.BriefSHA256 == "" {
		return ErrInitIncomplete
	}
	if _, err := s.db.Exec(`INSERT INTO checklist_inits (repo, checklist_id, brief_sha256, created_at) VALUES (?,?,?,?)`,
		in.Repo, in.ChecklistID, in.BriefSHA256, in.CreatedAt.Unix()); err != nil {
		return fmt.Errorf("put checklist init: %w", err)
	}
	return nil
}

// Binding is the binding of checklist id in repo: its latest approval's BRIEF prefix hash, else its
// init's; nil when the store has neither (an id this store never created or approved).
func (s *Store) Binding(repo, id string) (*ChecklistBinding, error) {
	var b ChecklistBinding
	err := s.db.QueryRow(`SELECT brief_sha256 FROM checklist_approvals WHERE repo = ? AND checklist_id = ? ORDER BY seq DESC LIMIT 1`,
		repo, id).Scan(&b.BriefSHA256)
	if err == nil {
		b.Approved = true
		return &b, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return nil, fmt.Errorf("checklist binding: %w", err)
	}
	err = s.db.QueryRow(`SELECT brief_sha256 FROM checklist_inits WHERE repo = ? AND checklist_id = ?`, repo, id).Scan(&b.BriefSHA256)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("checklist binding: %w", err)
	}
	return &b, nil
}
