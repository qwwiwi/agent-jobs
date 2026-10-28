package store

import (
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"
)

// ReleasePass records that release_check (Wave F, F7) passed for a repository at a sha under one
// approved checklist (id, version and hash). The merge hook (F9 (b)) reads it: `gh pr merge
// --match-head-commit <sha>` in a repository with an approved checklist is allowed only with a pass for
// that sha at the checklist as it is now. Written by release_check on a pass; a later run for the same
// repository and sha that does not pass removes the repository's passes for the sha (the latest run wins).
// A pass ships only the approval it was checked at (ApprovalSeq, #99).
type ReleasePass struct {
	Repo            string    `json:"repo"`
	SHA             string    `json:"sha"`
	ChecklistID     string    `json:"checklist_id"`
	Version         int       `json:"version"`
	ChecklistSHA256 string    `json:"checklist_sha256"`
	VerdictJob      string    `json:"verdict_job"`
	CreatedAt       time.Time `json:"created_at"`
	// ApprovalSeq is the seq of the approval release_check read and checked (0 = a pass stored before it
	// was recorded): only a pass at the repository's latest approval seq ships it (#99).
	ApprovalSeq int64 `json:"approval_seq"`
}

// releaseSchema is applied on every Open (a table added after the others: CREATE TABLE IF NOT EXISTS).
const releaseSchema = `
CREATE TABLE IF NOT EXISTS release_passes (
	repo             TEXT NOT NULL,
	sha              TEXT NOT NULL,
	checklist_id     TEXT NOT NULL,
	version          INTEGER NOT NULL,
	checklist_sha256 TEXT NOT NULL,
	verdict_job      TEXT NOT NULL,
	created_at       INTEGER NOT NULL,
	approval_seq     INTEGER NOT NULL DEFAULT 0,
	PRIMARY KEY (repo, sha, checklist_id, version, checklist_sha256)
);
`

// releaseColumns add the evaluated approval's seq (#99) to a release_passes table created before it; its
// old rows keep 0, which ships nothing.
var releaseColumns = []string{
	`ALTER TABLE release_passes ADD COLUMN approval_seq INTEGER NOT NULL DEFAULT 0`,
}

// releasePassCols are the columns a ReleasePass is read from (scanReleasePass); releasePassColsOld reads
// a table without approval_seq as 0: a read-only store (the merge hook's) opened before a read-write Open
// added the column.
const (
	releasePassCols    = `repo, sha, checklist_id, version, checklist_sha256, verdict_job, created_at, approval_seq`
	releasePassColsOld = `repo, sha, checklist_id, version, checklist_sha256, verdict_job, created_at, 0`
)

// noApprovalSeq reports whether err is a read of release_passes without the approval_seq column.
func noApprovalSeq(err error) bool {
	return err != nil && strings.Contains(err.Error(), "no such column: approval_seq")
}

// scanReleasePass reads one ReleasePass row of releasePassCols.
func scanReleasePass(row interface{ Scan(...any) error }) (ReleasePass, error) {
	var p ReleasePass
	var at int64
	if err := row.Scan(&p.Repo, &p.SHA, &p.ChecklistID, &p.Version, &p.ChecklistSHA256, &p.VerdictJob, &at, &p.ApprovalSeq); err != nil {
		return ReleasePass{}, err
	}
	p.CreatedAt = time.Unix(at, 0).UTC()
	return p, nil
}

// PutReleasePass stores a pass. A second pass at the same key replaces the first unless the first was
// checked at a higher approval seq (#103): of two release_check runs that race, the one at the newer
// approval is kept whichever stores last; an equal seq replaces (the later run wins).
func (s *Store) PutReleasePass(p ReleasePass) error {
	if p.Repo == "" || p.SHA == "" || p.ChecklistID == "" || p.Version < 1 || p.ChecklistSHA256 == "" {
		return errors.New("release pass needs repo, sha, checklist id, version and hash")
	}
	_, err := s.db.Exec(`INSERT INTO release_passes (repo, sha, checklist_id, version, checklist_sha256, verdict_job,
		created_at, approval_seq) VALUES (?,?,?,?,?,?,?,?)
		ON CONFLICT (repo, sha, checklist_id, version, checklist_sha256) DO UPDATE SET verdict_job=excluded.verdict_job,
		created_at=excluded.created_at, approval_seq=excluded.approval_seq
		WHERE excluded.approval_seq >= release_passes.approval_seq`, p.Repo, p.SHA, p.ChecklistID, p.Version, p.ChecklistSHA256,
		p.VerdictJob, p.CreatedAt.Unix(), p.ApprovalSeq)
	if err != nil {
		return fmt.Errorf("put release pass: %w", err)
	}
	return nil
}

// DropReleasePasses removes every pass of repo for sha (release_check ran again and did not pass).
func (s *Store) DropReleasePasses(repo, sha string) error {
	if _, err := s.db.Exec(`DELETE FROM release_passes WHERE repo=? AND sha=?`, repo, sha); err != nil {
		return fmt.Errorf("drop release passes: %w", err)
	}
	return nil
}

// ReleasePassed is the pass of repo for sha at the checklist id, version and hash (nil = none).
func (s *Store) ReleasePassed(repo, sha, checklistID string, version int, hash string) (*ReleasePass, error) {
	read := func(cols string) (ReleasePass, error) {
		return scanReleasePass(s.db.QueryRow(`SELECT `+cols+` FROM release_passes
		WHERE repo=? AND sha=? AND checklist_id=? AND version=? AND checklist_sha256=?`, repo, sha, checklistID, version, hash))
	}
	p, err := read(releasePassCols)
	if noApprovalSeq(err) {
		p, err = read(releasePassColsOld)
	}
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("release pass: %w", err)
	}
	return &p, nil
}

// ReleasePassesAt lists the passes of repo at the checklist id, version and hash, newest first.
func (s *Store) ReleasePassesAt(repo, checklistID string, version int, hash string) ([]ReleasePass, error) {
	query := func(cols string) (*sql.Rows, error) {
		return s.db.Query(`SELECT `+cols+` FROM release_passes
		WHERE repo=? AND checklist_id=? AND version=? AND checklist_sha256=? ORDER BY created_at DESC, sha`, repo, checklistID,
			version, hash)
	}
	rows, err := query(releasePassCols)
	if noApprovalSeq(err) {
		rows, err = query(releasePassColsOld)
	}
	if err != nil {
		return nil, fmt.Errorf("release passes: %w", err)
	}
	defer rows.Close()
	var out []ReleasePass
	for rows.Next() {
		p, err := scanReleasePass(rows)
		if err != nil {
			return nil, fmt.Errorf("release passes: %w", err)
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// ApprovalRepos lists the repositories with a stored checklist approval (any checklist), sorted.
func (s *Store) ApprovalRepos() ([]string, error) {
	rows, err := s.db.Query(`SELECT DISTINCT repo FROM checklist_approvals ORDER BY repo`)
	if err != nil {
		return nil, fmt.Errorf("approval repos: %w", err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var r string
		if err := rows.Scan(&r); err != nil {
			return nil, fmt.Errorf("approval repos: %w", err)
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// FeatureJobs lists the feature jobs of checklist id in repo (Wave F, F7 / F9 (a)): the fix and general
// jobs whose covers were read from checklist id (any version, any status) and whose size is not small:
// large, or none recorded (a job queued before sizes; job_start now refuses small with covers), oldest
// first.
func (s *Store) FeatureJobs(repo, id string) ([]CoverJob, error) {
	all, err := s.coverJobs(repo, id, `AND size<>'small'`)
	if err != nil {
		return nil, fmt.Errorf("feature jobs: %w", err)
	}
	return all, nil
}
