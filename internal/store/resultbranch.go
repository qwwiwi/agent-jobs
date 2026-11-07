package store

// SetResultBranch records the actual result branch and the outcome of an optional requested target.
func (s *Store) SetResultBranch(id, branch, note string) error {
	return affected(s.db.Exec(`UPDATE jobs SET branch=?, branch_note=? WHERE id=? AND status NOT IN ('queued','running')`, branch, note, id))
}
