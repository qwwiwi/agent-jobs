package jobs

import (
	"fmt"
	"slices"
	"strings"

	"github.com/qwwiwi/agent-jobs/internal/store"
)

// dependencyEdge is one "waits for" arc between jobs: a serialized wait, a stack parent, or a milestone
// task dependency (a job waits for the active jobs of the tasks it depends on).
type dependencyEdge struct {
	to, why string
}

// depGraph is the "waits for" graph over all three edge kinds, built from the complete reachable state
// a store transaction read (store.AdmissionGraph): the active jobs of every repository and the
// milestones they belong to.
type depGraph struct {
	byID       map[string]*store.Job
	milestones map[int64]store.AdmissionMilestone
	msJobs     map[int64][]*store.Job
	taskStatus map[int64]map[string]string
}

// newDepGraph builds the graph. job, when not nil, is a job as it will be stored (a new job, or a
// requeued one with its status already queued): it replaces the stored row of that id and joins its
// milestone's jobs. parent, when not nil, is the requested parent (it may be finished).
func newDepGraph(g store.AdmissionGraph, job, parent *store.Job) *depGraph {
	d := &depGraph{byID: map[string]*store.Job{}, milestones: g.Milestones,
		msJobs: map[int64][]*store.Job{}, taskStatus: map[int64]map[string]string{}}
	for _, o := range g.Active {
		d.byID[o.ID] = o
	}
	if parent != nil {
		d.byID[parent.ID] = parent
	}
	for id, ms := range g.Milestones {
		jobs := make([]*store.Job, 0, len(ms.Jobs)+1)
		seen := false
		for _, o := range ms.Jobs {
			if job != nil && o.ID == job.ID {
				o, seen = job, true
			}
			jobs = append(jobs, o)
		}
		if job != nil && job.MilestoneID == id && !seen {
			jobs = append(jobs, job)
		}
		d.msJobs[id] = jobs
		d.taskStatus[id] = TaskStatuses(ms.Tasks, jobs)
		for _, o := range jobs {
			if d.byID[o.ID] == nil {
				d.byID[o.ID] = o
			}
		}
	}
	if job != nil {
		d.byID[job.ID] = job
	}
	return d
}

func activeJob(o *store.Job) bool {
	return o.Status == store.StatusQueued || o.Status == store.StatusRunning || o.Status == store.StatusStopped
}

// edges lists what x waits for, as the queue will judge it: only a queued job waits, for its active
// parent, its active serialized blockers, and the active jobs of the tasks it depends on that are not
// done.
func (d *depGraph) edges(x *store.Job) []dependencyEdge {
	if x.Status != store.StatusQueued {
		return nil
	}
	var out []dependencyEdge
	if x.Parent != "" {
		if p := d.byID[x.Parent]; p != nil && activeJob(p) {
			out = append(out, dependencyEdge{p.ID, "parent"})
		}
	}
	for _, id := range x.WaitFor {
		if w := d.byID[id]; w != nil && activeJob(w) {
			out = append(out, dependencyEdge{w.ID, "serialized"})
		}
	}
	if ms, ok := d.milestones[x.MilestoneID]; ok && x.MilestoneID != 0 {
		if i := slices.IndexFunc(ms.Tasks, func(t store.MilestoneTask) bool { return t.Key == x.TaskKey }); i >= 0 {
			for _, dep := range ms.Tasks[i].DependsOn {
				if s := d.taskStatus[x.MilestoneID][dep]; s == TaskDone || s == TaskDropped || s == "" {
					continue
				}
				for _, o := range d.msJobs[x.MilestoneID] {
					if o.TaskKey == dep && activeJob(o) {
						out = append(out, dependencyEdge{o.ID, "milestone task " + dep})
					}
				}
			}
		}
	}
	return out
}

// cycleThrough looks for a cycle through j along "waits for" edges and returns it as "j waits for X
// (why), X waits for j (why)", or "". Cycles elsewhere in the graph are not reported: they exist
// before the change being judged and are not its doing.
func (d *depGraph) cycleThrough(j *store.Job) string {
	visited := map[string]bool{}
	var path []string
	var walk func(x *store.Job) bool
	walk = func(x *store.Job) bool {
		for _, e := range d.edges(x) {
			step := x.ID + " waits for " + e.to + " (" + e.why + ")"
			if e.to == j.ID {
				path = append(path, step)
				return true
			}
			if visited[e.to] || d.byID[e.to] == nil {
				continue
			}
			visited[e.to] = true
			path = append(path, step)
			if walk(d.byID[e.to]) {
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

// reaches reports whether job from waits, directly or through other jobs, for job to.
func (d *depGraph) reaches(from, to string) bool {
	visited := map[string]bool{from: true}
	stack := []string{from}
	for len(stack) > 0 {
		x := d.byID[stack[len(stack)-1]]
		stack = stack[:len(stack)-1]
		if x == nil {
			continue
		}
		for _, e := range d.edges(x) {
			if e.to == to {
				return true
			}
			if !visited[e.to] {
				visited[e.to] = true
				stack = append(stack, e.to)
			}
		}
	}
	return false
}

// renewOverlapAdmission is the overlap admission of a job that is requeued (store.ResumeAdmission). The
// job was not active while it was failed or timed out, so jobs admitted meanwhile never recorded it.
// It now waits, under its own on_overlap serialize, for the active overlapping jobs of its repository,
// except those that wait for it (queued ones wait on their own; a running or stopped one holds the
// claim, see jobDependenciesSQL) and those that already wait for it through others. A wait kept from
// the earlier admission that now closes a cycle is dropped, since its target was admitted behind the
// job. A cycle that remains (through the parent or a milestone dependency) refuses the resume.
func renewOverlapAdmission(stored *store.Job, g store.AdmissionGraph) ([]string, []store.JobOverlap, error) {
	if !coderJob(stored.Kind) || stored.RepoPath == "" {
		return stored.WaitFor, stored.Overlap, nil
	}
	j := *stored
	j.Status = store.StatusQueued
	d := newDepGraph(g, &j, nil)
	var waits []string
	for _, id := range stored.WaitFor {
		if x := d.byID[id]; x != nil && activeJob(x) && d.reaches(x.ID, j.ID) {
			continue
		}
		waits = append(waits, id)
	}
	overlap := slices.Clone(stored.Overlap)
	if stored.OnOverlap == "serialize" {
		for _, x := range g.Active {
			if x.ID == j.ID || x.RepoPath != j.RepoPath || !coderJob(x.Kind) || slices.Contains(waits, x.ID) ||
				(x.Status != store.StatusQueued && x.Status != store.StatusRunning) {
				continue
			}
			o, ok := overlapOf(&j, x)
			if !ok || slices.Contains(x.WaitFor, j.ID) || d.reaches(x.ID, j.ID) {
				continue
			}
			waits = append(waits, x.ID)
			if !slices.ContainsFunc(overlap, func(e store.JobOverlap) bool { return e.ID == x.ID }) {
				overlap = append(overlap, o)
			}
		}
	}
	j.WaitFor = waits
	if cycle := d.cycleThrough(&j); cycle != "" {
		return nil, nil, fmt.Errorf("dependency cycle: %s; the resumed job could never start (resolve the dependency first)", cycle)
	}
	return waits, overlap, nil
}

// milestoneCycleCheck judges a milestone edit (store.MilestoneGraphCheck): the tasks it leaves, with
// the active jobs of every repository and every milestone they belong to, must not hold a cycle that
// the milestone did not already hold. Every new edge starts at a queued job of the edited milestone,
// so those are the starting points.
func milestoneCycleCheck(id int64, tasks func() []store.MilestoneTask) store.MilestoneGraphCheck {
	return func(g store.AdmissionGraph) error {
		after := make(map[int64]store.AdmissionMilestone, len(g.Milestones))
		for k, v := range g.Milestones {
			after[k] = v
		}
		ms := after[id]
		ms.Tasks = tasks()
		after[id] = ms
		before, next := newDepGraph(g, nil, nil), newDepGraph(store.AdmissionGraph{Active: g.Active, Milestones: after}, nil, nil)
		for _, x := range next.msJobs[id] {
			if x.Status != store.StatusQueued {
				continue
			}
			if cycle := next.cycleThrough(x); cycle != "" && before.cycleThrough(before.byID[x.ID]) == "" {
				return fmt.Errorf("milestone %d edit refused, dependency cycle: %s; neither job could ever start "+
					"(drop the new dependency, or start the job it waits for after the other has finished)", id, cycle)
			}
		}
		return nil
	}
}
