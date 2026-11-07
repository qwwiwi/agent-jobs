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

// overlapAdmission runs inside the insert's BEGIN IMMEDIATE transaction, like milestone dependency
// checks at claim time. Simultaneous starts cannot both miss each other, including under budget caps.
func (m *Manager) overlapAdmission(j *store.Job) store.JobAdmission {
	return func(others []*store.Job, parent *store.Job, milestones map[int64]store.AdmissionMilestone) error {
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
			if other.ID == j.ID || len(j.Paths) == 0 || len(other.Paths) == 0 {
				continue
			}
			overlap := store.JobOverlap{ID: other.ID}
			for _, a := range j.Paths {
				for _, b := range other.Paths {
					if pathscope.Intersect(a, b) {
						if !slices.Contains(overlap.Paths, a) {
							overlap.Paths = append(overlap.Paths, a)
						}
						if !slices.Contains(overlap.OtherPaths, b) {
							overlap.OtherPaths = append(overlap.OtherPaths, b)
						}
					}
				}
			}
			if len(overlap.Paths) == 0 {
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
		if cycle := dependencyCycle(j, others, parent, milestones); cycle != "" {
			return fmt.Errorf("dependency cycle: %s; neither job could ever start (use on_overlap warn, drop the parent, or start after the job it waits for has finished)", cycle)
		}
		return nil
	}
}

// dependencyEdge is one "waits for" arc between jobs: a serialized wait, a stack parent, or a milestone
// task dependency (a job waits for the active jobs of the tasks it depends on).
type dependencyEdge struct {
	to, why string
}

// dependencyCycle looks for a cycle through the new job j across all three edge kinds, as the queue
// will judge them once j is stored: j's own serialized waits and parent, and the milestone edges that
// j's task adds to queued jobs of tasks depending on it (j makes its task running again). The graph
// before j is acyclic (every earlier admission was checked the same way), so any cycle contains j.
// It returns "j waits for X (why), X waits for j (why)" or "".
func dependencyCycle(j *store.Job, others []*store.Job, parent *store.Job, milestones map[int64]store.AdmissionMilestone) string {
	byID := map[string]*store.Job{}
	for _, o := range others {
		byID[o.ID] = o
	}
	if parent != nil {
		byID[parent.ID] = parent
	}
	taskStatus := map[int64]map[string]string{}
	msJobs := map[int64][]*store.Job{}
	for id, ms := range milestones {
		jobs := slices.Clone(ms.Jobs)
		if j.MilestoneID == id && !slices.ContainsFunc(jobs, func(o *store.Job) bool { return o.ID == j.ID }) {
			jobs = append(jobs, j)
		}
		msJobs[id] = jobs
		taskStatus[id] = TaskStatuses(ms.Tasks, jobs)
		for _, o := range jobs {
			if byID[o.ID] == nil {
				byID[o.ID] = o
			}
		}
	}
	byID[j.ID] = j
	active := func(o *store.Job) bool {
		return o.Status == store.StatusQueued || o.Status == store.StatusRunning || o.Status == store.StatusStopped
	}
	edges := func(x *store.Job) []dependencyEdge {
		if x.Status != store.StatusQueued {
			return nil // only a queued job waits
		}
		var out []dependencyEdge
		if x.Parent != "" {
			if p := byID[x.Parent]; p != nil && active(p) {
				out = append(out, dependencyEdge{p.ID, "parent"})
			}
		}
		for _, id := range x.WaitFor {
			if w := byID[id]; w != nil && active(w) {
				out = append(out, dependencyEdge{w.ID, "serialized"})
			}
		}
		if ms, ok := milestones[x.MilestoneID]; ok && x.MilestoneID != 0 {
			if i := slices.IndexFunc(ms.Tasks, func(t store.MilestoneTask) bool { return t.Key == x.TaskKey }); i >= 0 {
				for _, d := range ms.Tasks[i].DependsOn {
					if s := taskStatus[x.MilestoneID][d]; s == TaskDone || s == TaskDropped || s == "" {
						continue
					}
					for _, o := range msJobs[x.MilestoneID] {
						if o.TaskKey == d && active(o) {
							out = append(out, dependencyEdge{o.ID, "milestone task " + d})
						}
					}
				}
			}
		}
		return out
	}
	// Depth-first from j along "waits for" edges; an edge back to j closes the cycle.
	visited := map[string]bool{}
	var path []string
	var walk func(x *store.Job) bool
	walk = func(x *store.Job) bool {
		for _, e := range edges(x) {
			step := x.ID + " waits for " + e.to + " (" + e.why + ")"
			if e.to == j.ID {
				path = append(path, step)
				return true
			}
			if visited[e.to] || byID[e.to] == nil {
				continue
			}
			visited[e.to] = true
			path = append(path, step)
			if walk(byID[e.to]) {
				return true
			}
			path = path[:len(path)-1]
		}
		return false
	}
	if walk(j) {
		return strings.Join(path, ", ")
	}
	return ""
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
