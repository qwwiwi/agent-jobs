package store

// SetResultBranch records the actual result branch and the outcome of an optional requested target.
func (s *Store) SetResultBranch(id, branch, note string) error {
	return affected(s.db.Exec(`UPDATE jobs SET branch=?, branch_note=? WHERE id=? AND status NOT IN ('queued','running')`, branch, note, id))
}

// PendingResultBranch returns ended jobs with a repository and a base commit whose result branch is
// not recorded yet and whose work dir was not cleaned up: stopped jobs (PendingNotify skips them)
// and jobs not yet notified. A row already notified predates result branches (publication runs
// before the notification), so it is never published retrospectively. publishResultBranch decides
// per job whether there is anything to publish; a job that has an outcome recorded, or no commits,
// is not published again.
func (s *Store) PendingResultBranch() ([]*Job, error) {
	return s.query(`WHERE status NOT IN ('queued','running') AND repo_path != '' AND base_commit != ''
		AND (status = 'stopped' OR notified_at IS NULL)
		AND branch = '' AND branch_note = '' AND cleaned_at IS NULL ORDER BY finished_at`)
}
