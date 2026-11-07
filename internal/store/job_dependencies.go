package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
)

// JobAdmission sees the same-repository active jobs and the requested parent under the insert's
// write lock. It may fill the new job's dependencies. It must not call back into Store.
type JobAdmission func(others []*Job, parent *Job) error

func overlapJSON(items []JobOverlap) string {
	if len(items) == 0 {
		return "[]"
	}
	b, _ := json.Marshal(items)
	return string(b)
}

func (s *Store) insertAdmitted(j *Job, checks []JobAdmission, insert func(querier) (sql.Result, error)) (bool, error) {
	inserted := false
	run := func(q querier) error {
		if len(checks) > 0 {
			others, err := activeRepoJobs(q, j.RepoPath)
			if err != nil {
				return err
			}
			var parent *Job
			if j.Parent != "" {
				parent, err = scanJob(q.QueryRowContext(context.Background(), `SELECT `+cols+` FROM jobs WHERE id=?`, j.Parent))
				if errors.Is(err, sql.ErrNoRows) {
					return fmt.Errorf("parent %s: %w", j.Parent, ErrNotFound)
				}
				if err != nil {
					return err
				}
			}
			for _, check := range checks {
				if err := check(others, parent); err != nil {
					return err
				}
			}
		}
		res, err := insert(q)
		if err != nil {
			if jobIDTaken(err) {
				return ErrJobIDExists
			}
			return fmt.Errorf("insert job: %w", err)
		}
		n, err := res.RowsAffected()
		inserted = n > 0
		return err
	}
	var err error
	if len(checks) > 0 {
		err = s.immediate(run)
	} else {
		err = run(s.db)
	}
	return inserted, err
}

func activeRepoJobs(q queryer, repo string) ([]*Job, error) {
	rows, err := q.QueryContext(context.Background(), `SELECT `+cols+` FROM jobs
  WHERE repo_path=? AND status IN ('queued','running') AND kind IN ('fix','general') ORDER BY created_at,rowid`, repo)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var jobs []*Job
	for rows.Next() {
		j, err := scanJob(rows)
		if err != nil {
			return nil, err
		}
		jobs = append(jobs, j)
	}
	return jobs, rows.Err()
}

// The claim statement is shared by normal and milestone jobs. A blocker resumed after the launch
// check still prevents the claim. Missing dependency rows fail closed. A stopped job may resume.
const jobDependenciesSQL = ` AND NOT EXISTS (
 SELECT 1 FROM json_each(jobs.wait_for) w LEFT JOIN jobs d ON d.id=w.value
 WHERE d.id IS NULL OR d.status IN ('queued','running','stopped'))
 AND (jobs.parent='' OR (jobs.parent_base<>'' AND EXISTS (
 SELECT 1 FROM jobs p WHERE p.id=jobs.parent AND p.status='succeeded')))`

// SetParentBase freezes the parent's result for a queued job before its first claim. Resume keeps
// that base, even if the parent is later run again. The caller validates the commit in the repository.
func (s *Store) SetParentBase(id, parent, base, branch string) error {
	return affected(s.db.Exec(`UPDATE jobs SET base_commit=?,base_branch=?,parent_base=?
  WHERE id=? AND parent=? AND status='queued' AND parent_base='' AND EXISTS
  (SELECT 1 FROM jobs p WHERE p.id=? AND p.status='succeeded')`, base, branch, base, id, parent, parent))
}
