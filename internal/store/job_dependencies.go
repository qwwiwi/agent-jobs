package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
)

// AdmissionMilestone is a milestone as the admission sees it: its tasks and all its jobs, read in
// the insert's transaction, so a milestone dependency edge can be judged next to the job edges.
type AdmissionMilestone struct {
	Tasks []MilestoneTask
	Jobs  []*Job
}

// AdmissionGraph is everything a "waits for" edge can come from, read under one write lock: every
// queued, running or stopped job of any repository, and every milestone one of them (or the job being
// admitted or edited) belongs to. A repository-unbound milestone chains jobs of several repositories,
// so a cycle can only be ruled out on the whole reachable graph, not on one repository's part of it.
type AdmissionGraph struct {
	Active     []*Job
	Milestones map[int64]AdmissionMilestone
}

// JobAdmission sees the same-repository active jobs, the requested parent and the complete dependency
// graph under the insert's write lock. It may fill the new job's dependencies. It must not call back
// into Store.
type JobAdmission func(others []*Job, parent *Job, graph AdmissionGraph) error

// ResumeAdmission renews the overlap admission of a job that is requeued: it sees the job as stored
// and the complete dependency graph, and returns the wait_for and overlap to store with the requeue.
type ResumeAdmission func(j *Job, graph AdmissionGraph) (waitFor []string, overlap []JobOverlap, err error)

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
			graph, err := loadAdmissionGraph(context.Background(), q, j.MilestoneID)
			if err != nil {
				return err
			}
			for _, check := range checks {
				if err := check(others, parent, graph); err != nil {
					return err
				}
			}
		}
		res, err := insert(q)
		if err != nil {
			if jobIDTaken(err) {
				return ErrJobIDExists
			}
			if idemKeyTaken(err) {
				return ErrIdemKeyExists
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

// bgContext is context.Background for functions whose "context" parameter shadows the package.
func bgContext() context.Context { return context.Background() }

// loadAdmissionGraph reads the active jobs of every repository and the milestones they belong to, plus
// the extra milestones (the one of the job being admitted or edited).
func loadAdmissionGraph(ctx context.Context, q querier, extra ...int64) (AdmissionGraph, error) {
	rows, err := q.QueryContext(ctx, `SELECT `+cols+` FROM jobs WHERE status IN ('queued','running','stopped') ORDER BY created_at,rowid`)
	if err != nil {
		return AdmissionGraph{}, fmt.Errorf("active jobs: %w", err)
	}
	var g AdmissionGraph
	ids := map[int64]bool{}
	for _, id := range extra {
		if id != 0 {
			ids[id] = true
		}
	}
	for rows.Next() {
		j, err := scanJob(rows)
		if err != nil {
			rows.Close()
			return AdmissionGraph{}, fmt.Errorf("scan job: %w", err)
		}
		g.Active = append(g.Active, j)
		if j.MilestoneID != 0 {
			ids[j.MilestoneID] = true
		}
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return AdmissionGraph{}, err
	}
	g.Milestones = make(map[int64]AdmissionMilestone, len(ids))
	for id := range ids {
		tasks, err := milestoneTasks(ctx, q, id)
		if err != nil {
			return AdmissionGraph{}, err
		}
		jobs, err := milestoneJobs(ctx, q, id)
		if err != nil {
			return AdmissionGraph{}, err
		}
		g.Milestones[id] = AdmissionMilestone{Tasks: tasks, Jobs: jobs}
	}
	return g, nil
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
// The reverse holds too: a blocker resumed after its serialized successor started (or stopped)
// waits for that successor, so the two never run side by side. Only a running or stopped successor
// holds it: a queued one waits for the blocker itself.
const jobDependenciesSQL = ` AND NOT EXISTS (
 SELECT 1 FROM json_each(jobs.wait_for) w LEFT JOIN jobs d ON d.id=w.value
 WHERE d.id IS NULL OR d.status IN ('queued','running','stopped'))
 AND NOT EXISTS (
 SELECT 1 FROM jobs s, json_each(s.wait_for) sw
 WHERE s.status IN ('running','stopped') AND s.wait_for<>'[]' AND sw.value=jobs.id)
 AND (jobs.parent='' OR (jobs.parent_base<>'' AND EXISTS (
 SELECT 1 FROM jobs p WHERE p.id=jobs.parent AND p.status='succeeded')))`

// ActiveSuccessors lists the running or stopped jobs serialized behind job id. A resumed blocker
// waits for them (jobDependenciesSQL), so a status caller can say why.
func (s *Store) ActiveSuccessors(id string) ([]*Job, error) {
	return s.queryContext(context.Background(), `WHERE status IN ('running','stopped') AND wait_for<>'[]'
  AND EXISTS (SELECT 1 FROM json_each(jobs.wait_for) sw WHERE sw.value=?) ORDER BY created_at,rowid`, id)
}

// SetParentBase freezes the parent's result for a queued job before its first claim. Resume keeps
// that base, even if the parent is later run again. The caller validates the commit in the repository.
func (s *Store) SetParentBase(id, parent, base, branch string) error {
	return affected(s.db.Exec(`UPDATE jobs SET base_commit=?,base_branch=?,parent_base=?
  WHERE id=? AND parent=? AND status='queued' AND parent_base='' AND EXISTS
  (SELECT 1 FROM jobs p WHERE p.id=? AND p.status='succeeded')`, base, branch, base, id, parent, parent))
}
