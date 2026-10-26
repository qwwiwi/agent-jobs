package store

import (
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// ReleasePass records that release_check (Wave F, F7) passed for a repository at a sha under one
// approved checklist (id, version and hash). The merge hook (F9 (b)) reads it: `gh pr merge
// --match-head-commit <sha>` in a repository with an approved checklist is allowed only with a pass for
// that sha at the checklist as it is now. Written by release_check on a pass; a later run for the same
// repository and sha that does not pass removes the repository's passes for the sha (the latest run wins).
type ReleasePass struct {
	Repo            string    `json:"repo"`
	SHA             string    `json:"sha"`
	ChecklistID     string    `json:"checklist_id"`
	Version         int       `json:"version"`
	ChecklistSHA256 string    `json:"checklist_sha256"`
	VerdictJob      string    `json:"verdict_job"`
	CreatedAt       time.Time `json:"created_at"`
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
	PRIMARY KEY (repo, sha, checklist_id, version, checklist_sha256)
);
`

// PutReleasePass stores a pass (a second pass at the same key replaces the first).
func (s *Store) PutReleasePass(p ReleasePass) error {
	if p.Repo == "" || p.SHA == "" || p.ChecklistID == "" || p.Version < 1 || p.ChecklistSHA256 == "" {
		return errors.New("release pass needs repo, sha, checklist id, version and hash")
	}
	_, err := s.db.Exec(`INSERT OR REPLACE INTO release_passes (repo, sha, checklist_id, version, checklist_sha256, verdict_job,
		created_at) VALUES (?,?,?,?,?,?,?)`, p.Repo, p.SHA, p.ChecklistID, p.Version, p.ChecklistSHA256, p.VerdictJob, p.CreatedAt.Unix())
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
	var p ReleasePass
	var at int64
	err := s.db.QueryRow(`SELECT repo, sha, checklist_id, version, checklist_sha256, verdict_job, created_at FROM release_passes
		WHERE repo=? AND sha=? AND checklist_id=? AND version=? AND checklist_sha256=?`, repo, sha, checklistID, version, hash).
		Scan(&p.Repo, &p.SHA, &p.ChecklistID, &p.Version, &p.ChecklistSHA256, &p.VerdictJob, &at)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("release pass: %w", err)
	}
	p.CreatedAt = time.Unix(at, 0).UTC()
	return &p, nil
}

// ReleasePassesAt lists the passes of repo at the checklist id, version and hash, newest first.
func (s *Store) ReleasePassesAt(repo, checklistID string, version int, hash string) ([]ReleasePass, error) {
	rows, err := s.db.Query(`SELECT repo, sha, checklist_id, version, checklist_sha256, verdict_job, created_at FROM release_passes
		WHERE repo=? AND checklist_id=? AND version=? AND checklist_sha256=? ORDER BY created_at DESC, sha`, repo, checklistID,
		version, hash)
	if err != nil {
		return nil, fmt.Errorf("release passes: %w", err)
	}
	defer rows.Close()
	var out []ReleasePass
	for rows.Next() {
		var p ReleasePass
		var at int64
		if err := rows.Scan(&p.Repo, &p.SHA, &p.ChecklistID, &p.Version, &p.ChecklistSHA256, &p.VerdictJob, &at); err != nil {
			return nil, fmt.Errorf("release passes: %w", err)
		}
		p.CreatedAt = time.Unix(at, 0).UTC()
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
