package store

import (
	"database/sql"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"
)

// GateRepo is what the merge hook (Wave F, F9 (b)) keeps of a repository with a stored checklist
// approval: the GitHub OWNER/REPO names of its remotes, stored by checklist_set at the approval, by
// release_check on a pass and by the daemon's backfill at its start (the hook opens the store read-only
// and only reads them), so a work tree that is gone or moved is still matched by name (and never makes
// merges elsewhere unreadable); and the owner's retirement (`agent-jobs gate retire`): a repository is
// out of the gate while its latest approval is not newer than RetiredSeq.
type GateRepo struct {
	Repo string
	// Names are lower-cased OWNER/REPO names, sorted.
	Names []string
	// RetiredSeq is the seq of the latest approval when the owner retired the repository (0 = never).
	RetiredSeq int64
}

// mergeGateSchema is applied on every Open (a table added after the others: CREATE TABLE IF NOT EXISTS).
const mergeGateSchema = `
CREATE TABLE IF NOT EXISTS merge_gate_repos (
	repo        TEXT PRIMARY KEY,
	names       TEXT NOT NULL DEFAULT '',
	retired_seq INTEGER NOT NULL DEFAULT 0,
	updated_at  INTEGER NOT NULL
);
`

// ErrNotGated is RetireGateRepo's error for a repository without a stored checklist approval.
var ErrNotGated = errors.New("the repository has no stored checklist approval, so it is not gated")

// GateRepoOf is the merge-gate record of repo (Repo "" and no names when there is none).
func (s *Store) GateRepoOf(repo string) (GateRepo, error) {
	var g GateRepo
	var names string
	err := s.db.QueryRow(`SELECT repo, names, retired_seq FROM merge_gate_repos WHERE repo=?`, repo).Scan(&g.Repo, &names, &g.RetiredSeq)
	// A read-only store (the merge hook's) opened before any read-write Open created the table has no
	// record: nothing was stored, nothing retired.
	if errors.Is(err, sql.ErrNoRows) || err != nil && strings.Contains(err.Error(), "no such table: merge_gate_repos") {
		return GateRepo{}, nil
	}
	if err != nil {
		return GateRepo{}, fmt.Errorf("merge gate repo: %w", err)
	}
	g.Names = SplitNames(names)
	return g, nil
}

// AddGateRepoNames adds OWNER/REPO names to repo's record (created when missing); names already stored
// stay. A name holding a comma is refused (the column is comma-joined).
func (s *Store) AddGateRepoNames(repo string, names []string) error {
	if repo == "" {
		return errors.New("merge gate repo: no repository")
	}
	for _, n := range names {
		if n == "" || strings.Contains(n, ",") {
			return fmt.Errorf("merge gate repo: bad name %q", n)
		}
	}
	tx, err := s.db.Begin()
	if err != nil {
		return fmt.Errorf("merge gate repo: %w", err)
	}
	defer tx.Rollback() //nolint:errcheck // a no-op after Commit
	var stored string
	switch err := tx.QueryRow(`SELECT names FROM merge_gate_repos WHERE repo=?`, repo).Scan(&stored); {
	case errors.Is(err, sql.ErrNoRows):
	case err != nil:
		return fmt.Errorf("merge gate repo: %w", err)
	}
	all := SplitNames(stored)
	grown := false
	for _, n := range names {
		if !slices.Contains(all, n) {
			all, grown = append(all, n), true
		}
	}
	if !grown && stored != "" {
		return nil
	}
	slices.Sort(all)
	if _, err := tx.Exec(`INSERT INTO merge_gate_repos (repo, names, updated_at) VALUES (?,?,?)
		ON CONFLICT(repo) DO UPDATE SET names=excluded.names, updated_at=excluded.updated_at`, repo, JoinNames(all),
		time.Now().Unix()); err != nil {
		return fmt.Errorf("merge gate repo: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("merge gate repo: %w", err)
	}
	return nil
}

// RetireGateRepo takes repo out of the merge gate (the owner's `agent-jobs gate retire`): it records the
// seq of its latest approval, so a later approval gates it again, and forgets its names. It returns that
// seq; ErrNotGated when repo has no stored approval.
func (s *Store) RetireGateRepo(repo string) (int64, error) {
	var seq sql.NullInt64
	if err := s.db.QueryRow(`SELECT MAX(seq) FROM checklist_approvals WHERE repo=?`, repo).Scan(&seq); err != nil {
		return 0, fmt.Errorf("retire merge gate repo: %w", err)
	}
	if !seq.Valid {
		return 0, ErrNotGated
	}
	if _, err := s.db.Exec(`INSERT INTO merge_gate_repos (repo, names, retired_seq, updated_at) VALUES (?, '', ?, ?)
		ON CONFLICT(repo) DO UPDATE SET names='', retired_seq=excluded.retired_seq, updated_at=excluded.updated_at`, repo,
		seq.Int64, time.Now().Unix()); err != nil {
		return 0, fmt.Errorf("retire merge gate repo: %w", err)
	}
	return seq.Int64, nil
}
