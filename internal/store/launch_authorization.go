package store

// LaunchAuthorization lives in the same protected database as the launch token. It is never
// read from job-writable files or command-line options, and is valid for exactly one attempt.
type LaunchAuthorization struct {
	CodexWriteAllowed bool
	AcceptanceAllowed bool
}

// SetLaunchAuthorization records the launcher's effective permissions only for its running claim.
func (s *Store) SetLaunchAuthorization(id, token string, attempt int, a LaunchAuthorization) error {
	return affected(s.db.Exec(`INSERT INTO launch_authorizations
		(job_id, attempt, launch_token, codex_write_allowed, acceptance_allowed)
		SELECT id, attempt, launch_token, ?, ? FROM jobs
		WHERE id=? AND status='running' AND launch_token=? AND attempt=? AND launch_token!=''
		ON CONFLICT(job_id) DO UPDATE SET attempt=excluded.attempt, launch_token=excluded.launch_token,
		codex_write_allowed=excluded.codex_write_allowed, acceptance_allowed=excluded.acceptance_allowed`,
		a.CodexWriteAllowed, a.AcceptanceAllowed, id, token, attempt))
}

// ReadLaunchAuthorization fails closed for missing, stale, other-attempt or unreadable state.
func (s *Store) ReadLaunchAuthorization(id, token string) (LaunchAuthorization, error) {
	var a LaunchAuthorization
	err := s.db.QueryRow(`SELECT a.codex_write_allowed, a.acceptance_allowed
		FROM launch_authorizations a JOIN jobs j ON j.id=a.job_id
		WHERE j.id=? AND j.status='running' AND j.launch_token=? AND j.launch_token!=''
		AND a.launch_token=j.launch_token AND a.attempt=j.attempt`, id, token).
		Scan(&a.CodexWriteAllowed, &a.AcceptanceAllowed)
	if err != nil {
		return LaunchAuthorization{}, err
	}
	return a, nil
}
