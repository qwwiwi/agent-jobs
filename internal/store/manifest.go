package store

import (
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// ManifestCheck is the record of one manifest-check run (Wave F, F5 / F6): the judge's wrapper runs the
// repository's scripts/manifest-check.sh in a separate checkout of the judged sha before the judge model
// starts and stores the exit code and the output tail here, bound to the job, its attempt and the sha.
// Written only by the wrapper under the job's launch token; release_check (F7) reads it.
type ManifestCheck struct {
	Seq   int64  `json:"seq"`
	JobID string `json:"job_id"`
	// Attempt is the job's attempt that stored the record (read from the job row under the launch token).
	Attempt         int    `json:"attempt"`
	Repo            string `json:"repo"`
	SHA             string `json:"sha"`
	ChecklistID     string `json:"checklist_id"`
	Version         int    `json:"version"`
	ChecklistSHA256 string `json:"checklist_sha256"`
	// Script is the path of the script in the checkout (scripts/manifest-check.sh).
	Script string `json:"script"`
	// ExitCode is the script's exit code: 128+signal when a signal ended it, AcceptanceTimedOut (-1) on
	// its timeout.
	ExitCode    int `json:"exit_code"`
	DurationSec int `json:"duration_sec"`
	// Output is the tail of its stdout and stderr (redacted); OutputCut the bytes cut before it.
	Output    string    `json:"output"`
	OutputCut int64     `json:"output_cut"`
	CreatedAt time.Time `json:"created_at"`
}

// manifestSchema is applied on every Open. A log: the latest record per job attempt (by seq) is the one read.
const manifestSchema = `
CREATE TABLE IF NOT EXISTS gate_manifest_checks (
	seq              INTEGER PRIMARY KEY AUTOINCREMENT,
	job_id           TEXT NOT NULL,
	attempt          INTEGER NOT NULL,
	repo             TEXT NOT NULL,
	sha              TEXT NOT NULL,
	checklist_id     TEXT NOT NULL,
	version          INTEGER NOT NULL,
	checklist_sha256 TEXT NOT NULL,
	script           TEXT NOT NULL,
	exit_code        INTEGER NOT NULL,
	duration_sec     INTEGER NOT NULL,
	output           TEXT NOT NULL,
	output_cut       INTEGER NOT NULL,
	created_at       INTEGER NOT NULL
);
CREATE INDEX IF NOT EXISTS gate_manifest_checks_job ON gate_manifest_checks(job_id, attempt, seq);
CREATE INDEX IF NOT EXISTS gate_manifest_checks_sha ON gate_manifest_checks(repo, sha, seq);
`

// PutManifestCheck stores a manifest-check record for the running launch token of its job (the
// wrapper's write); the attempt is the one the token runs, never the caller's.
func (s *Store) PutManifestCheck(token string, c ManifestCheck) (int64, error) {
	if c.JobID == "" || c.Repo == "" || c.SHA == "" || c.ChecklistID == "" || c.Version < 1 || c.ChecklistSHA256 == "" || c.Script == "" {
		return 0, errors.New("manifest-check record needs job, repo, sha, checklist id, version and hash, and the script")
	}
	tx, err := s.db.Begin()
	if err != nil {
		return 0, err
	}
	defer func() { _ = tx.Rollback() }()
	attempt, err := runningUnder(tx, c.JobID, token)
	if err != nil {
		return 0, fmt.Errorf("put manifest-check record: %w", err)
	}
	res, err := tx.Exec(`INSERT INTO gate_manifest_checks (job_id, attempt, repo, sha, checklist_id, version, checklist_sha256, script,
		exit_code, duration_sec, output, output_cut, created_at) VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?)`, c.JobID, attempt, c.Repo, c.SHA,
		c.ChecklistID, c.Version, c.ChecklistSHA256, c.Script, c.ExitCode, c.DurationSec, c.Output, c.OutputCut, c.CreatedAt.Unix())
	if err != nil {
		return 0, fmt.Errorf("put manifest-check record: %w", err)
	}
	seq, err := res.LastInsertId()
	if err != nil {
		return 0, err
	}
	return seq, tx.Commit()
}

const manifestCols = `seq, job_id, attempt, repo, sha, checklist_id, version, checklist_sha256, script, exit_code, duration_sec,
	output, output_cut, created_at`

func scanManifest(r scanner) (*ManifestCheck, error) {
	var c ManifestCheck
	var at int64
	if err := r.Scan(&c.Seq, &c.JobID, &c.Attempt, &c.Repo, &c.SHA, &c.ChecklistID, &c.Version, &c.ChecklistSHA256, &c.Script,
		&c.ExitCode, &c.DurationSec, &c.Output, &c.OutputCut, &at); err != nil {
		return nil, err
	}
	c.CreatedAt = time.Unix(at, 0).UTC()
	return &c, nil
}

func oneManifest(row *sql.Row) (*ManifestCheck, error) {
	c, err := scanManifest(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("manifest-check record: %w", err)
	}
	return c, nil
}

// ManifestCheckByJob is the manifest-check record attempt of a job stored (nil = none).
func (s *Store) ManifestCheckByJob(jobID string, attempt int) (*ManifestCheck, error) {
	return oneManifest(s.db.QueryRow(`SELECT `+manifestCols+` FROM gate_manifest_checks WHERE job_id=? AND attempt=?
		ORDER BY seq DESC LIMIT 1`, jobID, attempt))
}

// LatestManifestCheck is the latest manifest-check record of repo for sha (nil = none).
func (s *Store) LatestManifestCheck(repo, sha string) (*ManifestCheck, error) {
	return oneManifest(s.db.QueryRow(`SELECT `+manifestCols+` FROM gate_manifest_checks WHERE repo=? AND sha=?
		ORDER BY seq DESC LIMIT 1`, repo, sha))
}
