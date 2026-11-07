package jobs

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/qwwiwi/agent-jobs/internal/pathscope"
	"github.com/qwwiwi/agent-jobs/internal/store"
)

func coderJob(kind string) bool { return kind == "fix" || kind == "general" }

func validateOverlapRequest(r *StartRequest) error {
	if len(r.Paths) > 64 {
		return errors.New("paths: at most 64 declarations")
	}
	if len(r.Paths) > 0 || r.OnOverlap != "" || r.Parent != "" {
		if !coderJob(r.Kind) || r.RepoPath == "" {
			return errors.New("paths, on_overlap and parent require a fix/general job with a repo")
		}
	}
	if r.Parent != "" && r.Base != "" {
		return errors.New("parent and base are mutually exclusive: a stack uses its parent's result")
	}
	if r.OnOverlap == "" {
		r.OnOverlap = "warn"
	}
	if !slices.Contains([]string{"warn", "serialize", "refuse"}, r.OnOverlap) {
		return errors.New("on_overlap must be warn, serialize or refuse")
	}
	var paths []string
	for _, p := range r.Paths {
		if err := pathscope.Validate(p); err != nil {
			return fmt.Errorf("paths: %w", err)
		}
		if !slices.Contains(paths, p) {
			paths = append(paths, p)
		}
	}
	r.Paths = paths
	return nil
}

// overlapOf lists the declared paths of a and b that intersect.
func overlapOf(a, b *store.Job) (store.JobOverlap, bool) {
	o := store.JobOverlap{ID: b.ID}
	if len(a.Paths) == 0 || len(b.Paths) == 0 {
		return o, false
	}
	for _, p := range a.Paths {
		for _, q := range b.Paths {
			if pathscope.Intersect(p, q) {
				if !slices.Contains(o.Paths, p) {
					o.Paths = append(o.Paths, p)
				}
				if !slices.Contains(o.OtherPaths, q) {
					o.OtherPaths = append(o.OtherPaths, q)
				}
			}
		}
	}
	return o, len(o.Paths) > 0
}

// overlapAdmission runs inside the insert's BEGIN IMMEDIATE transaction, like milestone dependency
// checks at claim time. Simultaneous starts cannot both miss each other, including under budget caps.
func (m *Manager) overlapAdmission(j *store.Job) store.JobAdmission {
	return func(others []*store.Job, parent *store.Job, graph store.AdmissionGraph) error {
		j.Overlap, j.WaitFor = nil, nil // an id collision may retry the admission
		if j.Parent != "" {
			if parent == nil || parent.Agent != j.Agent || parent.RepoPath != j.RepoPath || !coderJob(parent.Kind) {
				return errors.New("parent must be a fix/general job of this agent on the same repository")
			}
			switch parent.Status {
			case store.StatusQueued, store.StatusRunning, store.StatusStopped:
				j.BaseCommit, j.BaseBranch = parent.BaseCommit, parent.BaseBranch
			case store.StatusSucceeded:
				base, err := m.parentResult(parent)
				if err != nil {
					return err
				}
				j.BaseCommit, j.ParentBase, j.BaseBranch = base, base, parent.BaseBranch
			default:
				return fmt.Errorf("parent %s is %s: stack refused", parent.ID, parent.Status)
			}
		}
		for _, other := range others {
			if other.ID == j.ID {
				continue
			}
			overlap, ok := overlapOf(j, other)
			if !ok {
				continue
			}
			j.Overlap = append(j.Overlap, overlap)
			// An explicit stack already waits for this parent. Other overlaps still follow on_overlap.
			if other.ID == j.Parent {
				continue
			}
			switch j.OnOverlap {
			case "refuse":
				return fmt.Errorf("overlap with job %s: paths %s intersect %s (use serialize or an explicit parent stack)",
					other.ID, strings.Join(overlap.Paths, ", "), strings.Join(overlap.OtherPaths, ", "))
			case "serialize":
				j.WaitFor = append(j.WaitFor, other.ID)
			}
		}
		if cycle := newDepGraph(graph, j, parent).cycleThrough(j); cycle != "" {
			return fmt.Errorf("dependency cycle: %s; neither job could ever start (use on_overlap warn, drop the parent, or start after the job it waits for has finished)", cycle)
		}
		return nil
	}
}

func (m *Manager) parentResult(parent *store.Job) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), MergeCheckTimeout)
	defer cancel()
	head, note := m.jobHead(ctx, parent) // verified live worktree or refs/agent-jobs/jobs/<id> after cleanup
	if note != "" || head == "" {
		if note == "" {
			note = "no verified worktree or saved result ref"
		}
		return "", fmt.Errorf("parent %s result unavailable: %s", parent.ID, note)
	}
	return head, nil
}

// jobDependencies is the queue launch verdict, parallel to milestoneLaunch. Serialized jobs wait for
// completion, regardless of outcome; a stack requires success. A stopped dependency can still resume.
// resolve freezes the successful parent base before claim, never during a read-only status request.
func (m *Manager) jobDependencies(j *store.Job, resolve bool) (wait, cancel string, err error) {
	if j.Status != store.StatusQueued {
		return "", "", nil
	}
	var waiting []string
	if j.Parent != "" {
		parent, err := m.Store.Get(j.Parent)
		if err != nil {
			return "", "", err
		}
		switch parent.Status {
		case store.StatusQueued, store.StatusRunning, store.StatusStopped:
			waiting = append(waiting, "parent "+parent.ID+" ("+parent.Status+")")
		case store.StatusSucceeded:
			if resolve && j.ParentBase == "" {
				base, err := m.parentResult(parent)
				if err != nil {
					return "", "parent result unavailable: " + err.Error(), nil
				}
				if err := m.Store.SetParentBase(j.ID, parent.ID, base, parent.BaseBranch); err != nil {
					return "", "", err
				}
				j.BaseCommit, j.ParentBase, j.BaseBranch = base, base, parent.BaseBranch
			}
		default:
			return "", fmt.Sprintf("parent %s is %s: stack refused", parent.ID, parent.Status), nil
		}
	}
	for _, id := range j.WaitFor {
		dep, err := m.Store.Get(id)
		if err != nil {
			return "", "", err
		}
		if dep.Status == store.StatusQueued || dep.Status == store.StatusRunning || dep.Status == store.StatusStopped {
			waiting = append(waiting, id+" ("+dep.Status+")")
		}
	}
	// A blocker resumed after its serialized successor started waits for that successor.
	successors, err := m.Store.ActiveSuccessors(j.ID)
	if err != nil {
		return "", "", err
	}
	for _, s := range successors {
		waiting = append(waiting, "successor "+s.ID+" ("+s.Status+")")
	}
	if len(waiting) > 0 {
		return "serialized wait for " + strings.Join(waiting, ", "), "", nil
	}
	return "", "", nil
}

// JobWaiting combines job and milestone dependencies for status callers, without changing the queue.
func (m *Manager) JobWaiting(ctx context.Context, j *store.Job) string {
	wait, cancel, err := m.jobDependencies(j, false)
	if err != nil {
		return "dependency check unavailable: " + err.Error()
	}
	if cancel != "" {
		return cancel
	}
	return strings.Trim(strings.Join([]string{wait, m.MilestoneWaiting(ctx, j)}, "; "), "; ")
}
