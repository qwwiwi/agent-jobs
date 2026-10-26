package store

import (
	"database/sql"
	"errors"
)

// LaunchAuthorization lives in the same protected database as the launch token. It is never
// read from job-writable files or command-line options, and is valid for exactly one attempt.
type LaunchAuthorization struct {
	CodexWriteAllowed bool
	AcceptanceAllowed bool
	// CodexSandbox is the `codex exec --sandbox` value the launcher chose for a codex job ("" otherwise).
	CodexSandbox string
	// CodexNetwork is whether the launcher gave a codex write job network (config codex_write_network).
	CodexNetwork bool
}

// SetLaunchAuthorization records the launcher's effective permissions only for its running claim.
func (s *Store) SetLaunchAuthorization(id, token string, attempt int, a LaunchAuthorization) error {
	return affected(s.db.Exec(`INSERT INTO launch_authorizations
		(job_id, attempt, launch_token, codex_write_allowed, acceptance_allowed, codex_sandbox, codex_network)
		SELECT id, attempt, launch_token, ?, ?, ?, ? FROM jobs
		WHERE id=? AND status='running' AND launch_token=? AND attempt=? AND launch_token!=''
		ON CONFLICT(job_id) DO UPDATE SET attempt=excluded.attempt, launch_token=excluded.launch_token,
		codex_write_allowed=excluded.codex_write_allowed, acceptance_allowed=excluded.acceptance_allowed,
		codex_sandbox=excluded.codex_sandbox, codex_network=excluded.codex_network`,
		a.CodexWriteAllowed, a.AcceptanceAllowed, a.CodexSandbox, a.CodexNetwork, id, token, attempt))
}

// ReadLaunchAuthorization fails closed for missing, stale, other-attempt or unreadable state.
func (s *Store) ReadLaunchAuthorization(id, token string) (LaunchAuthorization, error) {
	var a LaunchAuthorization
	err := s.db.QueryRow(`SELECT a.codex_write_allowed, a.acceptance_allowed, a.codex_sandbox, a.codex_network
		FROM launch_authorizations a JOIN jobs j ON j.id=a.job_id
		WHERE j.id=? AND j.status='running' AND j.launch_token=? AND j.launch_token!=''
		AND a.launch_token=j.launch_token AND a.attempt=j.attempt`, id, token).
		Scan(&a.CodexWriteAllowed, &a.AcceptanceAllowed, &a.CodexSandbox, &a.CodexNetwork)
	if err != nil {
		return LaunchAuthorization{}, err
	}
	return a, nil
}

// LaunchCodexSandbox is the codex sandbox recorded by the job's latest launch, for display ("" = none
// recorded: not a codex job, not launched yet, or launched by an older build). It is no authorization.
func (s *Store) LaunchCodexSandbox(id string) (string, error) {
	var sandbox string
	err := s.db.QueryRow(`SELECT codex_sandbox FROM launch_authorizations WHERE job_id=?`, id).Scan(&sandbox)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	return sandbox, err
}
