package jobs

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/qwwiwi/agent-jobs/internal/checklist"
	"github.com/qwwiwi/agent-jobs/internal/store"
)

// Milestone limits.
const (
	MaxMilestoneTitleRunes = 200
	MaxMilestoneGoalRunes  = 300
	MaxMilestoneNoteRunes  = 300
	MaxMilestoneTasks      = 100
	MaxTaskDeps            = 50
	MaxTaskCovers          = 50
)

// Derived task statuses (TaskStatus), in the order they are decided.
const (
	TaskDropped = "dropped"
	TaskBlocked = "blocked"
	TaskRunning = "running"
	TaskReview  = "review"
	TaskDone    = "done"
	TaskFailed  = "failed"
	TaskReady   = "ready"
)

// MilestoneStatuses are the values milestone_set and milestone_list take.
var MilestoneStatuses = []string{store.MilestoneOpen, store.MilestoneDone, store.MilestoneDropped}

// reTaskKey is a task key: T1, T2, … or any short name without spaces or commas.
var reTaskKey = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.-]{0,31}$`)

// MilestoneTaskInput is one task of milestone_create, or one upsert of milestone_set: a nil DependsOn
// or Covers keeps the stored value of an existing task (none for a new one), an empty list clears it.
type MilestoneTaskInput struct {
	Key       string    `json:"key" jsonschema:"task key, unique in the milestone: T1, T2, ..."`
	Title     string    `json:"title,omitempty" jsonschema:"one line; required for a new task"`
	DependsOn *[]string `json:"depends_on,omitempty" jsonschema:"keys of the tasks this one waits for (their accepted job); no cycles"`
	Covers    *[]string `json:"covers,omitempty" jsonschema:"requirement IDs the task works on: live rows of the milestone's checklist when it has one, else free-form"`
	// Sources (G02) are planned like covers: nil keeps the stored list, empty clears it.
	Sources *[]string `json:"sources,omitempty" jsonschema:"https git URLs a job on the task gets cloned read-only into its folder (sources/<name>) unless job_start gives its own"`
}

// MilestoneCreate is milestone_create's input.
type MilestoneCreate struct {
	Title       string               `json:"title" jsonschema:"one line, at most 200 characters"`
	Goal        string               `json:"goal,omitempty" jsonschema:"optional one-line goal"`
	Repo        string               `json:"repo,omitempty" jsonschema:"optional absolute path of the git repository (stored as its main worktree)"`
	Branch      string               `json:"branch,omitempty" jsonschema:"optional integration branch: job_start on a task takes it as the default base"`
	ChecklistID string               `json:"checklist_id,omitempty" jsonschema:"optional: the id of the repo's brief/CHECKLIST.md; task covers must then be its live rows"`
	Order       int                  `json:"order,omitempty" jsonschema:"position among the repo's milestones, lowest first; default 0"`
	MaxParallel int                  `json:"max_parallel,omitempty" jsonschema:"at most this many of the milestone's jobs run at once (1 = one after another); default no cap of its own"`
	Tasks       []MilestoneTaskInput `json:"tasks" jsonschema:"the tasks, at least one"`
}

// MilestoneSet is milestone_set's input: every field but id is optional; an empty field keeps its value.
type MilestoneSet struct {
	ID          int64                `json:"id" jsonschema:"milestone id"`
	Title       string               `json:"title,omitempty"`
	Goal        *string              `json:"goal,omitempty" jsonschema:"new goal; empty string clears it"`
	Branch      *string              `json:"branch,omitempty" jsonschema:"new integration branch; empty string clears it"`
	Order       *int                 `json:"order,omitempty"`
	MaxParallel *int                 `json:"max_parallel,omitempty" jsonschema:"new cap of running jobs; 0 removes it"`
	Status      string               `json:"status,omitempty" jsonschema:"open | done | dropped; done is refused while a task is not done or dropped"`
	CloseNote   string               `json:"close_note,omitempty" jsonschema:"one line, at most 300 characters"`
	Tasks       []MilestoneTaskInput `json:"tasks,omitempty" jsonschema:"tasks to add or edit, by key (an edit of a dropped task brings it back)"`
	Drop        []string             `json:"drop,omitempty" jsonschema:"keys of tasks to drop (never deleted)"`
}

// MilestoneJobView is one job of a task in the milestone tree.
type MilestoneJobView struct {
	ID          string  `json:"id"`
	Status      string  `json:"status"`
	Outcome     string  `json:"outcome,omitempty"`
	Model       string  `json:"model"`
	CostUSD     float64 `json:"cost_usd"`
	CostUnknown bool    `json:"cost_unknown,omitempty"`
	Turns       int     `json:"turns"`
	CreatedAt   string  `json:"created_at"`
	FinishedAt  string  `json:"finished_at,omitempty"`
}

// MilestoneTaskView is one task in the milestone tree, with its derived status and its jobs (oldest first).
type MilestoneTaskView struct {
	Key       string             `json:"key"`
	Title     string             `json:"title"`
	Status    string             `json:"status"`
	DependsOn []string           `json:"depends_on"`
	Covers    []string           `json:"covers"`
	Jobs      []MilestoneJobView `json:"jobs"`
	CreatedAt string             `json:"created_at"`
	UpdatedAt string             `json:"updated_at"`
	// Waiting is why the task's queued job does not start yet ("waiting for T1 (failed)"), "" when none waits.
	Waiting string `json:"waiting,omitempty"`
	// Sources are the git URLs planned for the task's jobs (G02).
	Sources []string `json:"sources"`
}

// MilestoneProgress counts the done tasks (dropped ones left out) and, for a milestone tied to a
// checklist, the covered requirements the latest judge verdict found met (null without a verdict).
type MilestoneProgress struct {
	TasksDone       int  `json:"tasks_done"`
	TasksTotal      int  `json:"tasks_total"`
	AcceptanceMet   *int `json:"acceptance_met"`
	AcceptanceTotal int  `json:"acceptance_total"`
}

// MilestoneView is a milestone as milestone_list returns it; the tree fields (tasks, ready, progress,
// uncovered) are set by milestone_get and by milestone_list with tree.
type MilestoneView struct {
	ID    int64  `json:"id"`
	Title string `json:"title"`
	Goal  string `json:"goal,omitempty"`
	Repo  string `json:"repo,omitempty"`
	// RepoID is a stable opaque id of the repository (MilestoneRepoID), the same in MCP and the HTTP API,
	// where repo is only its folder name: a frontend filters by it.
	RepoID      string              `json:"repo_id,omitempty"`
	Branch      string              `json:"branch,omitempty"`
	ChecklistID string              `json:"checklist_id,omitempty"`
	Order       int                 `json:"order"`
	Status      string              `json:"status"`
	MaxParallel *int                `json:"max_parallel,omitempty"`
	CreatedAt   string              `json:"created_at"`
	UpdatedAt   string              `json:"updated_at"`
	ClosedAt    string              `json:"closed_at,omitempty"`
	CloseNote   string              `json:"close_note,omitempty"`
	Tasks       []MilestoneTaskView `json:"tasks,omitempty"`
	// Ready are the keys of the tasks a job can start on now.
	Ready    []string           `json:"ready,omitempty"`
	Progress *MilestoneProgress `json:"progress,omitempty"`
	// Uncovered are the live checklist rows no task covers (a milestone tied to a checklist only).
	Uncovered []string `json:"uncovered,omitempty"`
	// Note says why the checklist part of the tree could not be read ("" when it was, or there is none).
	Note string `json:"note,omitempty"`
}

// MilestonesView is milestone_list's output. Cursor is the next updated_since: the highest updated_at
// of all milestones when the list was read, in Unix seconds ("0" without one).
type MilestonesView struct {
	Milestones []MilestoneView `json:"milestones"`
	Cursor     string          `json:"cursor"`
}

// MilestoneListRequest is milestone_list's filter.
type MilestoneListRequest struct {
	Status string `json:"status,omitempty" jsonschema:"open | done | dropped; default all"`
	Repo   string `json:"repo,omitempty" jsonschema:"absolute path of a repository: only its milestones (the project view)"`
	RepoID string `json:"repo_id,omitempty" jsonschema:"repo_id of a repository, as milestone_get returns it: only its milestones"`
	// RepoName is the folder name of a repository (the HTTP API's repo filter): it must name one
	// repository of the milestones (ErrAmbiguousRepo otherwise).
	RepoName    string `json:"-"`
	ChecklistID string `json:"checklist_id,omitempty" jsonschema:"only the milestones tied to this checklist id"`
	// UpdatedSince is RFC 3339 or Unix seconds.
	UpdatedSince string `json:"updated_since,omitempty" jsonschema:"change cursor: only milestones whose updated_at is at or after it (RFC 3339 or Unix seconds); pass the cursor of the previous answer"`
	Tree         bool   `json:"tree,omitempty" jsonschema:"true: each milestone with its full tree (tasks, jobs, ready, progress), in order"`
}

func fmtMilestoneTS(t *time.Time) string {
	if t == nil {
		return ""
	}
	return t.UTC().Format(time.RFC3339)
}

// oneLine validates a one-line text field: valid UTF-8, no control or format character, at most max
// characters (refused, not cut); required when req.
func oneLine(what, s string, max int, req bool) (string, error) {
	s = strings.TrimSpace(s)
	if !utf8.ValidString(s) {
		return "", fmt.Errorf("%s is not valid UTF-8", what)
	}
	if containsControl(s) {
		return "", fmt.Errorf("%s contains a control character (newline, tab, escape, ...); it is one line", what)
	}
	if n := utf8.RuneCountInString(s); n > max {
		return "", fmt.Errorf("%s is %d characters, at most %d", what, n, max)
	}
	if req && s == "" {
		return "", fmt.Errorf("%s is required", what)
	}
	return s, nil
}

// milestoneRepo resolves a repository path to its main worktree ("" for "").
func milestoneRepo(p string) (top, main string, err error) {
	if p == "" {
		return "", "", nil
	}
	if !strings.HasPrefix(p, "/") {
		return "", "", fmt.Errorf("repo %q must be an absolute path", Printable(p, 200))
	}
	if top, err = git(p, "rev-parse", "--show-toplevel"); err != nil {
		return "", "", fmt.Errorf("repo %s is not a git repository: %w", Printable(p, 200), err)
	}
	return top, mainWorktree(top), nil
}

func checkBranch(b string) (string, error) {
	b, err := oneLine("branch", b, 200, false)
	if err != nil || b == "" {
		return b, err
	}
	if strings.ContainsAny(b, " ~^:?*[\\") || strings.HasPrefix(b, "-") || strings.Contains(b, "..") {
		return "", fmt.Errorf("branch %q is not a valid branch name", b)
	}
	return b, nil
}

// liveRows reads the live requirement IDs of checklist id in the work tree of repo; it fails when the
// repository's brief/CHECKLIST.md is missing or has another id.
func (m *Manager) liveRows(repo, id string) ([]string, error) {
	f, err := m.readWorkTreeBrief(repo)
	if err != nil {
		return nil, err
	}
	if f.Checklist == nil {
		return nil, fmt.Errorf("%s has no %s (checklist_init first)", repo, checklist.ChecklistRel)
	}
	text, _ := Redact(string(f.Checklist))
	c, err := checklist.Parse(text)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", checklist.ChecklistRel, err)
	}
	if c.ID != id {
		return nil, fmt.Errorf("checklist_id %q is not the id of %s in %s (%q)", id, checklist.ChecklistRel, repo, c.ID)
	}
	return c.LiveIDs(), nil
}

// applyTasks merges the task inputs into tasks (upsert by key) and drops the keys in drop; it returns the
// whole new task list and the changed tasks, or an error for an invalid key, title, dependency (unknown
// key, itself, a cycle) or cover (not a live row when live is not nil).
func applyTasks(tasks []store.MilestoneTask, in []MilestoneTaskInput, drop []string, live []string) (all, changed []store.MilestoneTask, err error) {
	all = slices.Clone(tasks)
	idx := map[string]int{}
	for i, t := range all {
		idx[t.Key] = i
	}
	touched, newCovers := map[string]bool{}, map[string]bool{}
	seen := map[string]bool{}
	for _, ti := range in {
		key := strings.TrimSpace(ti.Key)
		if !reTaskKey.MatchString(key) {
			return nil, nil, fmt.Errorf("task key %q must be 1-32 letters, digits, '.', '_' or '-' (T1, T2, ...)", Printable(key, 40))
		}
		if seen[key] {
			return nil, nil, fmt.Errorf("task %s is given twice", key)
		}
		seen[key] = true
		title, err := oneLine("task "+key+" title", ti.Title, MaxMilestoneTitleRunes, false)
		if err != nil {
			return nil, nil, err
		}
		i, ok := idx[key]
		if !ok {
			if title == "" {
				return nil, nil, fmt.Errorf("task %s: title is required for a new task", key)
			}
			all = append(all, store.MilestoneTask{Key: key})
			i = len(all) - 1
			idx[key] = i
		}
		t := &all[i]
		if title != "" {
			t.Title = title
		}
		t.Dropped = false
		if ti.Sources != nil {
			if t.Sources, err = ValidateSources(*ti.Sources); err != nil {
				return nil, nil, fmt.Errorf("task %s: %w", key, err)
			}
		}
		if ti.DependsOn != nil {
			t.DependsOn = trimList(*ti.DependsOn)
		}
		if ti.Covers != nil {
			t.Covers = trimList(*ti.Covers)
			newCovers[key] = true
		}
		touched[key] = true
	}
	for _, key := range drop {
		key = strings.TrimSpace(key)
		i, ok := idx[key]
		if !ok {
			return nil, nil, fmt.Errorf("drop: task %q is not in the milestone", Printable(key, 40))
		}
		if !all[i].Dropped {
			all[i].Dropped = true
			touched[key] = true
		}
	}
	if len(all) == 0 {
		return nil, nil, errors.New("a milestone needs at least one task")
	}
	if len(all) > MaxMilestoneTasks {
		return nil, nil, fmt.Errorf("a milestone has at most %d tasks", MaxMilestoneTasks)
	}
	for _, t := range all {
		if !touched[t.Key] {
			continue
		}
		// Covers are checked against the live rows when they are given: stored ones stay valid after a
		// row is retired, so a title edit or a revived task is not refused for them.
		rows := live
		if !newCovers[t.Key] {
			rows = nil
		}
		if err := checkTask(t, idx, rows); err != nil {
			return nil, nil, err
		}
	}
	if cyc := findCycle(all); cyc != "" {
		return nil, nil, fmt.Errorf("depends_on has a cycle: %s", cyc)
	}
	for _, t := range all {
		if touched[t.Key] {
			changed = append(changed, t)
		}
	}
	return all, changed, nil
}

func trimList(in []string) []string {
	out := []string{}
	for _, s := range in {
		if s = strings.TrimSpace(s); s != "" && !slices.Contains(out, s) {
			out = append(out, s)
		}
	}
	return out
}

func checkTask(t store.MilestoneTask, idx map[string]int, live []string) error {
	if len(t.DependsOn) > MaxTaskDeps || len(t.Covers) > MaxTaskCovers {
		return fmt.Errorf("task %s: at most %d depends_on and %d covers", t.Key, MaxTaskDeps, MaxTaskCovers)
	}
	for _, d := range t.DependsOn {
		if d == t.Key {
			return fmt.Errorf("task %s depends on itself", t.Key)
		}
		if _, ok := idx[d]; !ok {
			return fmt.Errorf("task %s depends on %q, which is not a task of the milestone", t.Key, Printable(d, 40))
		}
	}
	for _, c := range t.Covers {
		if live != nil {
			if !slices.Contains(live, c) {
				return fmt.Errorf("task %s covers %q, which is not a live row of the milestone's checklist", t.Key, Printable(c, 40))
			}
			continue
		}
		if strings.Contains(c, ",") || containsControl(c) || utf8.RuneCountInString(c) > 40 {
			return fmt.Errorf("task %s: cover %q must be a short ID without commas", t.Key, Printable(c, 40))
		}
	}
	return nil
}

// findCycle returns a dependency cycle of tasks as "T1 -> T2 -> T1", or "".
func findCycle(tasks []store.MilestoneTask) string {
	deps := map[string][]string{}
	for _, t := range tasks {
		deps[t.Key] = t.DependsOn
	}
	const (
		visiting = 1
		finished = 2
	)
	state := map[string]int{}
	var stack []string
	var visit func(k string) string
	visit = func(k string) string {
		switch state[k] {
		case visiting:
			i := slices.Index(stack, k)
			return strings.Join(append(slices.Clone(stack[i:]), k), " -> ")
		case finished:
			return ""
		}
		state[k] = visiting
		stack = append(stack, k)
		for _, d := range deps[k] {
			if c := visit(d); c != "" {
				return c
			}
		}
		stack = stack[:len(stack)-1]
		state[k] = finished
		return ""
	}
	for _, t := range tasks {
		if c := visit(t.Key); c != "" {
			return c
		}
	}
	return ""
}

// workUsed reports whether a job's outcome means its work was used: accepted, extended, or reworked
// (taken and changed by hand). Such a job makes its task done and is its prior result.
func workUsed(outcome string) bool {
	return outcome == OutcomeAccepted || outcome == OutcomeExtended || outcome == OutcomeReworked
}

// ownStatus is a task's status from its own jobs (oldest first), dependencies not looked at.
func ownStatus(jobs []*store.Job) string {
	for _, j := range jobs {
		switch j.Status {
		case store.StatusQueued, store.StatusRunning, store.StatusStopped:
			return TaskRunning
		}
	}
	if len(jobs) == 0 {
		return TaskReady
	}
	last := jobs[len(jobs)-1]
	switch {
	case last.Status == store.StatusSucceeded && last.Outcome == "":
		return TaskReview
	case workUsed(last.Outcome):
		return TaskDone
	case last.Status == store.StatusFailed || last.Status == store.StatusTimeout || last.Status == store.StatusCancelled,
		last.Outcome == OutcomeDiscarded:
		return TaskFailed
	}
	return TaskReady
}

// TaskStatuses derives the status of every task: dropped; running (a queued, running or stopped job);
// review (the latest job succeeded, no outcome yet); done (the latest job's outcome is accepted,
// extended or reworked); failed (the latest job failed, timed out or was cancelled, or its outcome is
// discarded); else blocked when a dependency (not dropped) is not done, else ready. Blocked replaces
// only ready: a task whose own jobs say more keeps that status.
func TaskStatuses(tasks []store.MilestoneTask, jobs []*store.Job) map[string]string {
	byTask := map[string][]*store.Job{}
	for _, j := range jobs {
		byTask[j.TaskKey] = append(byTask[j.TaskKey], j)
	}
	task := map[string]store.MilestoneTask{}
	for _, t := range tasks {
		task[t.Key] = t
	}
	out := map[string]string{}
	var status func(k string, depth int) string
	status = func(k string, depth int) string {
		if s, ok := out[k]; ok {
			return s
		}
		t := task[k]
		if t.Dropped {
			out[k] = TaskDropped
			return TaskDropped
		}
		s := ownStatus(byTask[k])
		if s == TaskReady && depth <= len(tasks) { // a cycle (refused on write) cannot recurse forever
			for _, d := range t.DependsOn {
				if dt, ok := task[d]; ok && !dt.Dropped && status(d, depth+1) != TaskDone {
					s = TaskBlocked
					break
				}
			}
		}
		out[k] = s
		return s
	}
	for _, t := range tasks {
		status(t.Key, 0)
	}
	return out
}

// waitingDeps lists the dependencies of task key that are not done (dropped ones left out), as "T1
// running": a job on the task starts once the list is empty.
func waitingDeps(tasks []store.MilestoneTask, jobs []*store.Job, key string) []string {
	i := slices.IndexFunc(tasks, func(t store.MilestoneTask) bool { return t.Key == key })
	if i < 0 {
		return nil
	}
	st := TaskStatuses(tasks, jobs)
	var out []string
	for _, d := range tasks[i].DependsOn {
		if s, ok := st[d]; ok && s != TaskDone && s != TaskDropped {
			out = append(out, d+" "+s)
		}
	}
	return out
}

// openTasks lists the tasks that are neither done nor dropped, as "T2 ready".
func openTasks(tasks []store.MilestoneTask, jobs []*store.Job) []string {
	st := TaskStatuses(tasks, jobs)
	var out []string
	for _, t := range tasks {
		if s := st[t.Key]; s != TaskDone && s != TaskDropped {
			out = append(out, t.Key+" "+s)
		}
	}
	return out
}

// CreateMilestone validates and stores a milestone with its tasks (milestone_create) and returns its tree.
func (m *Manager) CreateMilestone(ctx context.Context, in MilestoneCreate) (*MilestoneView, error) {
	title, err := oneLine("title", in.Title, MaxMilestoneTitleRunes, true)
	if err != nil {
		return nil, err
	}
	goal, err := oneLine("goal", in.Goal, MaxMilestoneGoalRunes, false)
	if err != nil {
		return nil, err
	}
	branch, err := checkBranch(in.Branch)
	if err != nil {
		return nil, err
	}
	top, repo, err := milestoneRepo(strings.TrimSpace(in.Repo))
	if err != nil {
		return nil, err
	}
	if in.MaxParallel < 0 {
		return nil, errors.New("max_parallel must be 0 (no cap) or more")
	}
	id := strings.TrimSpace(in.ChecklistID)
	var live []string
	if id != "" {
		if repo == "" {
			return nil, errors.New("checklist_id needs a repo: the checklist is the repo's " + checklist.ChecklistRel)
		}
		if live, err = m.liveRows(top, id); err != nil {
			return nil, err
		}
		if live == nil {
			live = []string{}
		}
	}
	_, tasks, err := applyTasks(nil, in.Tasks, nil, live)
	if err != nil {
		return nil, err
	}
	ms := &store.Milestone{Title: title, Goal: goal, Repo: repo, Branch: branch, ChecklistID: id, Order: in.Order}
	if in.MaxParallel > 0 {
		ms.MaxParallel = &in.MaxParallel
	}
	mid, err := m.Store.CreateMilestone(ms, tasks, m.Now())
	if err != nil {
		return nil, err
	}
	return m.MilestoneTree(ctx, mid)
}

// SetMilestone edits a milestone in place (milestone_set): its fields, its tasks (upsert by key, drop),
// its status. Closing it as done is refused while a task is not done or dropped; an edit that leaves a
// done milestone with such a task reopens it.
func (m *Manager) SetMilestone(ctx context.Context, in MilestoneSet) (*MilestoneView, error) {
	if in.ID <= 0 {
		return nil, errors.New("id is required")
	}
	cur, err := m.Store.GetMilestone(ctx, in.ID)
	if errors.Is(err, store.ErrMilestoneNotFound) {
		return nil, fmt.Errorf("milestone %d: %w", in.ID, store.ErrMilestoneNotFound)
	}
	if err != nil {
		return nil, err
	}
	if in.Status != "" && !slices.Contains(MilestoneStatuses, in.Status) {
		return nil, fmt.Errorf("status %q must be one of %s", Printable(in.Status, 40), strings.Join(MilestoneStatuses, ", "))
	}
	title, err := oneLine("title", in.Title, MaxMilestoneTitleRunes, false)
	if err != nil {
		return nil, err
	}
	note, err := oneLine("close_note", in.CloseNote, MaxMilestoneNoteRunes, false)
	if err != nil {
		return nil, err
	}
	var goal, branch *string
	if in.Goal != nil {
		g, err := oneLine("goal", *in.Goal, MaxMilestoneGoalRunes, false)
		if err != nil {
			return nil, err
		}
		goal = &g
	}
	if in.Branch != nil {
		b, err := checkBranch(*in.Branch)
		if err != nil {
			return nil, err
		}
		branch = &b
	}
	if in.MaxParallel != nil && *in.MaxParallel < 0 {
		return nil, errors.New("max_parallel must be 0 (no cap) or more")
	}
	var live []string
	if cur.ChecklistID != "" && len(in.Tasks) > 0 {
		top, _, err := milestoneRepo(cur.Repo)
		if err != nil {
			return nil, err
		}
		if live, err = m.liveRows(top, cur.ChecklistID); err != nil {
			return nil, err
		}
		if live == nil {
			live = []string{}
		}
	}
	now := m.Now()
	err = m.Store.UpdateMilestone(ctx, in.ID, now, func(ms *store.Milestone, tasks []store.MilestoneTask,
		jobs []*store.Job) (*store.Milestone, []store.MilestoneTask, error) {
		all, changed := tasks, []store.MilestoneTask(nil)
		if len(in.Tasks) > 0 || len(in.Drop) > 0 {
			var err error
			if all, changed, err = applyTasks(tasks, in.Tasks, in.Drop, live); err != nil {
				return nil, nil, err
			}
		}
		next := *ms
		if title != "" {
			next.Title = title
		}
		if goal != nil {
			next.Goal = *goal
		}
		if branch != nil {
			next.Branch = *branch
		}
		if in.Order != nil {
			next.Order = *in.Order
		}
		if in.MaxParallel != nil {
			next.MaxParallel = nil
			if n := *in.MaxParallel; n > 0 {
				next.MaxParallel = &n
			}
		}
		if note != "" {
			next.CloseNote = note
		}
		// The resulting state is checked on every edit: done only while every task is done or dropped. A
		// close as done is refused otherwise; any other edit of a done milestone with such a task (a new
		// task, a revived one, or a task whose latest job failed after the close) reopens it.
		status := ms.Status
		if in.Status != "" {
			status = in.Status
		}
		if status == store.MilestoneDone {
			if open := openTasks(all, jobs); len(open) > 0 {
				if in.Status == store.MilestoneDone {
					return nil, nil, fmt.Errorf("milestone %d cannot close as done: tasks not done or dropped: %s "+
						"(drop them, or close the milestone as dropped)", ms.ID, strings.Join(open, ", "))
				}
				status = store.MilestoneOpen
			}
		}
		if status != ms.Status {
			next.Status = status
			next.ClosedAt = nil
			if status != store.MilestoneOpen {
				next.ClosedAt = &now
			}
		}
		return &next, changed, nil
	})
	if err != nil {
		return nil, err
	}
	return m.MilestoneTree(ctx, in.ID)
}

// MilestoneRepoID is the opaque id of a milestone's repository (its stored main worktree): the first 12
// hex digits of its SHA-256, "" for none. It does not change while the path does not.
func MilestoneRepoID(repo string) string {
	if repo == "" {
		return ""
	}
	sum := sha256.Sum256([]byte(repo))
	return hex.EncodeToString(sum[:6])
}

// ErrAmbiguousRepo is returned for a repository folder name that names more than one repository.
var ErrAmbiguousRepo = errors.New("the repo name names more than one repository")

// milestoneRepoFilter resolves repo_id (or, without one, a folder name) to the stored repository path:
// ok is false when none matches (the list is then empty).
func (m *Manager) milestoneRepoFilter(ctx context.Context, id, name string) (repo string, ok bool, err error) {
	repos, err := m.Store.MilestoneRepos(ctx)
	if err != nil {
		return "", false, err
	}
	var hits []string
	for _, r := range repos {
		if (id != "" && MilestoneRepoID(r) == id) || (id == "" && filepath.Base(r) == name) {
			hits = append(hits, r)
		}
	}
	switch len(hits) {
	case 0:
		return "", false, nil
	case 1:
		return hits[0], true, nil
	}
	ids := make([]string, len(hits))
	for i, r := range hits {
		ids[i] = MilestoneRepoID(r)
	}
	return "", false, fmt.Errorf("%w: %q is the folder of repo_id %s; filter by repo_id", ErrAmbiguousRepo,
		Printable(name, 80), strings.Join(ids, ", "))
}

// milestoneView is the row part of a milestone's view.
func milestoneView(ms *store.Milestone) MilestoneView {
	return MilestoneView{
		ID: ms.ID, Title: ms.Title, Goal: ms.Goal, Repo: ms.Repo, RepoID: MilestoneRepoID(ms.Repo), Branch: ms.Branch,
		ChecklistID: ms.ChecklistID,
		Order:       ms.Order, Status: ms.Status, MaxParallel: ms.MaxParallel, CreatedAt: fmtMilestoneTS(&ms.CreatedAt),
		UpdatedAt: fmtMilestoneTS(&ms.UpdatedAt), ClosedAt: fmtMilestoneTS(ms.ClosedAt), CloseNote: ms.CloseNote,
	}
}

// MilestoneTree is milestone_get: the milestone with its tasks (derived status, dependencies, covers,
// jobs), the ready keys, the progress and, tied to a checklist, the live rows no task covers.
func (m *Manager) MilestoneTree(ctx context.Context, id int64) (*MilestoneView, error) {
	ms, err := m.Store.GetMilestone(ctx, id)
	if errors.Is(err, store.ErrMilestoneNotFound) {
		return nil, fmt.Errorf("milestone %d: %w", id, store.ErrMilestoneNotFound)
	}
	if err != nil {
		return nil, err
	}
	v, err := m.tree(ctx, ms)
	if err != nil {
		return nil, err
	}
	return &v, nil
}

func (m *Manager) tree(ctx context.Context, ms *store.Milestone) (MilestoneView, error) {
	v := milestoneView(ms)
	tasks, err := m.Store.MilestoneTasks(ctx, ms.ID)
	if err != nil {
		return v, err
	}
	jobs, err := m.Store.MilestoneJobs(ctx, ms.ID)
	if err != nil {
		return v, err
	}
	st := TaskStatuses(tasks, jobs)
	byTask := map[string][]MilestoneJobView{}
	for _, j := range jobs {
		byTask[j.TaskKey] = append(byTask[j.TaskKey], MilestoneJobView{
			ID: j.ID, Status: j.Status, Outcome: j.Outcome, Model: j.Model, CostUSD: j.CostUSD, CostUnknown: j.CostIsUnknown(),
			Turns: j.Turns, CreatedAt: fmtMilestoneTS(&j.CreatedAt), FinishedAt: fmtMilestoneTS(j.FinishedAt),
		})
	}
	p := &MilestoneProgress{}
	var covered []string
	v.Tasks, v.Ready = []MilestoneTaskView{}, []string{}
	for _, t := range tasks {
		tv := MilestoneTaskView{Key: t.Key, Title: t.Title, Status: st[t.Key], DependsOn: nonNilList(t.DependsOn),
			Covers: nonNilList(t.Covers), Jobs: byTask[t.Key], CreatedAt: fmtMilestoneTS(&t.CreatedAt),
			UpdatedAt: fmtMilestoneTS(&t.UpdatedAt), Sources: nonNilList(t.Sources)}
		if tv.Jobs == nil {
			tv.Jobs = []MilestoneJobView{}
		}
		if i := slices.IndexFunc(jobs, func(j *store.Job) bool { return j.TaskKey == t.Key && j.Status == store.StatusQueued }); i >= 0 {
			tv.Waiting, _ = launchVerdict(jobs[i], ms, tasks, jobs)
		}
		v.Tasks = append(v.Tasks, tv)
		if t.Dropped {
			continue
		}
		p.TasksTotal++
		switch tv.Status {
		case TaskDone:
			p.TasksDone++
		case TaskReady:
			v.Ready = append(v.Ready, t.Key)
		}
		for _, c := range t.Covers {
			if !slices.Contains(covered, c) {
				covered = append(covered, c)
			}
		}
	}
	v.Progress = p
	if ms.ChecklistID == "" {
		return v, nil
	}
	p.AcceptanceTotal = len(covered)
	if met, err := m.acceptanceMet(ms, covered); err != nil {
		v.Note = "judge verdict not read: " + RedactPrintable(err.Error(), 200)
	} else {
		p.AcceptanceMet = met
	}
	top, _, err := milestoneRepo(ms.Repo)
	if err == nil {
		var live []string
		if live, err = m.liveRows(top, ms.ChecklistID); err == nil {
			for _, id := range live {
				if !slices.Contains(covered, id) {
					v.Uncovered = append(v.Uncovered, id)
				}
			}
		}
	}
	if err != nil {
		note := "checklist not read: " + RedactPrintable(err.Error(), 200)
		if v.Note != "" {
			note = v.Note + "; " + note
		}
		v.Note = note
	}
	return v, nil
}

func nonNilList(s []string) []string {
	if s == nil {
		return []string{}
	}
	return s
}

// acceptanceMet counts the covered IDs the latest judge verdict of the milestone's checklist found met;
// nil when there is no verdict.
func (m *Manager) acceptanceMet(ms *store.Milestone, covered []string) (*int, error) {
	v, err := m.Store.LatestVerdict(ms.Repo, ms.ChecklistID, "")
	if err != nil || v == nil {
		return nil, err
	}
	b, err := json.Marshal(v.Verdict)
	if err != nil {
		return nil, err
	}
	var out JudgeOutput
	if err := json.Unmarshal(b, &out); err != nil {
		return nil, err
	}
	n := 0
	for _, r := range out.Requirements {
		if r.Verdict == "met" && slices.Contains(covered, r.ID) {
			n++
		}
	}
	return &n, nil
}

// ParseMilestoneCursor reads updated_since: RFC 3339 or Unix seconds ("" = none).
func ParseMilestoneCursor(s string) (*time.Time, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return nil, nil
	}
	if t, err := time.Parse(time.RFC3339, s); err == nil {
		return &t, nil
	}
	var n int64
	if _, err := fmt.Sscanf(s, "%d", &n); err == nil && fmt.Sprint(n) == s {
		t := time.Unix(n, 0)
		return &t, nil
	}
	return nil, fmt.Errorf("updated_since %q must be RFC 3339 or Unix seconds", Printable(s, 40))
}

// ListMilestones is milestone_list: the milestones matching r, ordered by repo, then order, then
// creation; with Tree each carries its full tree (the project view in one call).
func (m *Manager) ListMilestones(ctx context.Context, r MilestoneListRequest) (*MilestonesView, error) {
	if r.Status != "" && !slices.Contains(MilestoneStatuses, r.Status) {
		return nil, fmt.Errorf("status %q must be one of %s", Printable(r.Status, 40), strings.Join(MilestoneStatuses, ", "))
	}
	since, err := ParseMilestoneCursor(r.UpdatedSince)
	if err != nil {
		return nil, err
	}
	// The cursor is read before the list (see Store.MilestoneCursor).
	cursor, err := m.Store.MilestoneCursor(ctx)
	if err != nil {
		return nil, err
	}
	out := &MilestonesView{Milestones: []MilestoneView{}, Cursor: fmt.Sprint(cursor)}
	repo := strings.TrimSpace(r.Repo)
	if repo != "" {
		// The stored repo is the main worktree: a path of a linked worktree finds it too; a path that is no
		// (longer a) repository is matched as given.
		if _, main, err := milestoneRepo(repo); err == nil {
			repo = main
		}
	}
	// repo_id, or a folder name, names a stored repository; with repo too, all must name the same one. A
	// folder name next to a repository already named is only compared with it (never ambiguous).
	for _, by := range []struct{ id, name string }{{strings.TrimSpace(r.RepoID), ""}, {"", strings.TrimSpace(r.RepoName)}} {
		if by.id == "" && by.name == "" {
			continue
		}
		if by.name != "" && repo != "" {
			if filepath.Base(repo) != by.name {
				return out, nil
			}
			continue
		}
		found, ok, err := m.milestoneRepoFilter(ctx, by.id, by.name)
		if err != nil {
			return nil, err
		}
		if !ok || (repo != "" && found != repo) {
			return out, nil
		}
		repo = found
	}
	list, err := m.Store.ListMilestones(ctx, store.MilestoneFilter{Status: r.Status, Repo: repo,
		ChecklistID: strings.TrimSpace(r.ChecklistID), UpdatedSince: since})
	if err != nil {
		return nil, err
	}
	for _, ms := range list {
		v := milestoneView(ms)
		if r.Tree {
			if v, err = m.tree(ctx, ms); err != nil {
				return nil, err
			}
		}
		out.Milestones = append(out.Milestones, v)
	}
	return out, nil
}

// milestoneStart checks job_start on a task of a milestone and fills in the request: the milestone is
// open, the task exists and is neither blocked nor dropped, a repo given is the milestone's, and the
// milestone's branch is the default base when it resolves in the repo.
func (m *Manager) milestoneStart(r *StartRequest) error {
	if r.Milestone == 0 && r.Task == "" {
		return nil
	}
	if r.Milestone <= 0 || strings.TrimSpace(r.Task) == "" {
		return errors.New("milestone and task go together: give both (milestone id, task key) or neither")
	}
	if r.Gate != nil || r.ScheduleID != "" {
		return errors.New("gate and scheduled jobs are not attached to a milestone")
	}
	ctx := context.Background()
	ms, err := m.Store.GetMilestone(ctx, r.Milestone)
	if errors.Is(err, store.ErrMilestoneNotFound) {
		return fmt.Errorf("milestone %d: %w", r.Milestone, store.ErrMilestoneNotFound)
	}
	if err != nil {
		return err
	}
	if ms.Status != store.MilestoneOpen {
		return fmt.Errorf("milestone %d is %s (milestone_set status open reopens it)", ms.ID, ms.Status)
	}
	tasks, err := m.Store.MilestoneTasks(ctx, ms.ID)
	if err != nil {
		return err
	}
	jobs, err := m.Store.MilestoneJobs(ctx, ms.ID)
	if err != nil {
		return err
	}
	r.Task = strings.TrimSpace(r.Task)
	idx := slices.IndexFunc(tasks, func(t store.MilestoneTask) bool { return t.Key == r.Task })
	if idx < 0 {
		return fmt.Errorf("task %q is not a task of milestone %d", Printable(r.Task, 40), ms.ID)
	}
	if tasks[idx].Dropped {
		return fmt.Errorf("task %s of milestone %d is dropped", r.Task, ms.ID)
	}
	if r.Sources == nil {
		r.Sources = tasks[idx].Sources // planned at milestone_create; job_start's own list wins
	}
	// Its dependencies decide, whatever its own jobs say (a second job on a running task waits too).
	if waiting := waitingDeps(tasks, jobs, r.Task); len(waiting) > 0 {
		return fmt.Errorf("task %s of milestone %d is blocked: it waits for %s (a dependency is done once its job's outcome is accepted)",
			r.Task, ms.ID, strings.Join(waiting, ", "))
	}
	if ms.Repo != "" && r.RepoPath != "" {
		abs, err := filepath.Abs(r.RepoPath)
		if err != nil {
			return fmt.Errorf("repo path: %w", err)
		}
		_, main, err := milestoneRepo(abs)
		if err != nil {
			return err
		}
		if !samePath(main, ms.Repo) {
			return fmt.Errorf("repo %s is not the repository of milestone %d (%s)", main, ms.ID, ms.Repo)
		}
	}
	if ms.Branch != "" && r.Base == "" && r.RepoPath != "" {
		if _, err := git(r.RepoPath, "rev-parse", "--verify", "-q", "refs/heads/"+ms.Branch+"^{commit}"); err == nil {
			r.Base = ms.Branch
		}
	}
	return nil
}

// milestoneLaunch is the launch check of a queued job on a task (launchQueued), taken again because the
// milestone may have changed since job_start: launchVerdict on the milestone as stored now. A job of no
// milestone is not looked at.
func (m *Manager) milestoneLaunch(ctx context.Context, j *store.Job) (wait, cancel string, err error) {
	if j.MilestoneID == 0 {
		return "", "", nil
	}
	ms, err := m.Store.GetMilestone(ctx, j.MilestoneID)
	if errors.Is(err, store.ErrMilestoneNotFound) {
		ms, err = nil, nil
	}
	if err != nil {
		return "", "", err
	}
	var tasks []store.MilestoneTask
	var jobs []*store.Job
	if ms != nil {
		if tasks, err = m.Store.MilestoneTasks(ctx, ms.ID); err != nil {
			return "", "", err
		}
		if jobs, err = m.Store.MilestoneJobs(ctx, ms.ID); err != nil {
			return "", "", err
		}
	}
	wait, cancel = launchVerdict(j, ms, tasks, jobs)
	return wait, cancel, nil
}

// launchVerdict judges queued job j of milestone ms (nil = gone) from its tasks and jobs: wait, a reason,
// when it stays queued this tick (a dependency is not done, or the milestone runs max_parallel jobs);
// cancel, a reason, when it can no longer run (the milestone is closed, the task dropped or gone).
// launchQueued takes it before the claim and again inside the claim's transaction (Manager.claim).
func launchVerdict(j *store.Job, ms *store.Milestone, tasks []store.MilestoneTask, jobs []*store.Job) (wait, cancel string) {
	if ms == nil {
		return "", fmt.Sprintf("milestone %d no longer exists", j.MilestoneID)
	}
	if ms.Status != store.MilestoneOpen {
		return "", fmt.Sprintf("milestone %d is %s", ms.ID, ms.Status)
	}
	i := slices.IndexFunc(tasks, func(t store.MilestoneTask) bool { return t.Key == j.TaskKey })
	if i < 0 {
		return "", fmt.Sprintf("task %s is not a task of milestone %d", j.TaskKey, ms.ID)
	}
	if tasks[i].Dropped {
		return "", fmt.Sprintf("task %s of milestone %d was dropped", j.TaskKey, ms.ID)
	}
	if deps := waitingDeps(tasks, jobs, j.TaskKey); len(deps) > 0 {
		for k, d := range deps {
			key, status, _ := strings.Cut(d, " ")
			deps[k] = key + " (" + status + ")"
		}
		return "waiting for " + strings.Join(deps, ", "), ""
	}
	if ms.MaxParallel != nil {
		running := 0
		for _, o := range jobs {
			if o.Status == store.StatusRunning {
				running++
			}
		}
		if running >= *ms.MaxParallel {
			return fmt.Sprintf("waiting for a slot: milestone %d runs %d jobs (max_parallel %d)", ms.ID, running, *ms.MaxParallel), ""
		}
	}
	return "", ""
}

// MilestoneWaiting is why a queued job of a task does not start yet ("waiting for T1 (failed)": the
// launch check as the daemon takes it), "" for any other job or when it can start. A waiting job is not
// cancelled for it: once the dependency is done again, or dropped, it starts.
func (m *Manager) MilestoneWaiting(ctx context.Context, j *store.Job) string {
	if j.MilestoneID == 0 || j.Status != store.StatusQueued {
		return ""
	}
	wait, _, err := m.milestoneLaunch(ctx, j)
	if err != nil {
		return "launch check not read: " + RedactPrintable(err.Error(), 200)
	}
	return wait
}

// milestoneResumable refuses job_resume of a job whose milestone is closed or whose task was dropped: its
// attempt could never start. A blocked task is no reason: the resumed job waits queued for it.
func (m *Manager) milestoneResumable(j *store.Job) error {
	_, cancel, err := m.milestoneLaunch(context.Background(), j)
	if err != nil || cancel == "" {
		return err
	}
	return fmt.Errorf("job %s cannot be resumed: %s (milestone_set reopens it or brings the task back)", j.ID, cancel)
}
