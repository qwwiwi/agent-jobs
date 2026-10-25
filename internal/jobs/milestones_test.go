package jobs

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/qwwiwi/agent-jobs/internal/checklist"
	"github.com/qwwiwi/agent-jobs/internal/store"
)

func deps(keys ...string) *[]string { return &keys }

func taskOf(t *testing.T, v *MilestoneView, key string) MilestoneTaskView {
	t.Helper()
	for _, tv := range v.Tasks {
		if tv.Key == key {
			return tv
		}
	}
	t.Fatalf("no task %s in %+v", key, v.Tasks)
	return MilestoneTaskView{}
}

func tree(t *testing.T, m *Manager, id int64) *MilestoneView {
	t.Helper()
	v, err := m.MilestoneTree(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	return v
}

func wantStatuses(t *testing.T, v *MilestoneView, want map[string]string) {
	t.Helper()
	for k, s := range want {
		if got := taskOf(t, v, k).Status; got != s {
			t.Errorf("task %s: status %s, want %s", k, got, s)
		}
	}
}

// milestone_create makes the milestone and its tasks in one call; milestone_set edits fields and tasks
// in place (upsert by key, drop); a dropped dependency no longer blocks; closing as done is refused
// while a task is open; milestone_list orders and filters, and tree carries the tasks.
func TestMilestoneCreateSetGetList(t *testing.T) {
	m := newManager(t)
	ctx := context.Background()
	v, err := m.CreateMilestone(ctx, MilestoneCreate{Title: "Stage one", Goal: "ship it", Order: 2, Tasks: []MilestoneTaskInput{
		{Key: "T1", Title: "store"}, {Key: "T2", Title: "tools", DependsOn: deps("T1")}, {Key: "T3", Title: "docs"},
	}})
	if err != nil {
		t.Fatal(err)
	}
	wantStatuses(t, v, map[string]string{"T1": TaskReady, "T2": TaskBlocked, "T3": TaskReady})
	if !slices.Equal(v.Ready, []string{"T1", "T3"}) || v.Progress.TasksTotal != 3 || v.Progress.TasksDone != 0 ||
		v.Progress.AcceptanceMet != nil || v.Status != store.MilestoneOpen || v.Goal != "ship it" {
		t.Fatalf("created: %+v", v)
	}
	v, err = m.SetMilestone(ctx, MilestoneSet{ID: v.ID, Title: "Stage 1", Tasks: []MilestoneTaskInput{
		{Key: "T4", Title: "examples", DependsOn: deps("T3")}, {Key: "T1", Covers: deps("R01")},
	}, Drop: []string{"T3"}})
	if err != nil {
		t.Fatal(err)
	}
	wantStatuses(t, v, map[string]string{"T3": TaskDropped, "T4": TaskReady})
	if v.Title != "Stage 1" || !slices.Equal(v.Ready, []string{"T1", "T4"}) || v.Progress.TasksTotal != 3 ||
		taskOf(t, v, "T1").Title != "store" || !slices.Equal(taskOf(t, v, "T1").Covers, []string{"R01"}) {
		t.Fatalf("after set: %+v", v)
	}
	if _, err := m.SetMilestone(ctx, MilestoneSet{ID: v.ID, Status: store.MilestoneDone}); err == nil ||
		!strings.Contains(err.Error(), "cannot close as done") || !strings.Contains(err.Error(), "T2 blocked") {
		t.Fatalf("close as done with open tasks: %v", err)
	}
	other, err := m.CreateMilestone(ctx, MilestoneCreate{Title: "Stage zero", Order: 1, Tasks: []MilestoneTaskInput{{Key: "T1", Title: "x"}}})
	if err != nil {
		t.Fatal(err)
	}
	closed, err := m.SetMilestone(ctx, MilestoneSet{ID: other.ID, Status: store.MilestoneDropped, CloseNote: "not needed"})
	if err != nil || closed.Status != store.MilestoneDropped || closed.ClosedAt == "" || closed.CloseNote != "not needed" {
		t.Fatalf("drop: %+v %v", closed, err)
	}
	list, err := m.ListMilestones(ctx, MilestoneListRequest{})
	if err != nil || len(list.Milestones) != 2 || list.Milestones[0].ID != other.ID || list.Milestones[0].Tasks != nil {
		t.Fatalf("list by order: %+v %v", list, err)
	}
	list, err = m.ListMilestones(ctx, MilestoneListRequest{Status: store.MilestoneOpen, Tree: true})
	if err != nil || len(list.Milestones) != 1 || len(list.Milestones[0].Tasks) != 4 || list.Milestones[0].Progress == nil {
		t.Fatalf("open with tree: %+v %v", list, err)
	}
	future := m.Now().Add(time.Hour).UTC().Format(time.RFC3339)
	if list, err := m.ListMilestones(ctx, MilestoneListRequest{UpdatedSince: future}); err != nil || len(list.Milestones) != 0 {
		t.Fatalf("cursor in the future: %+v %v", list, err)
	}
	if _, err := m.ListMilestones(ctx, MilestoneListRequest{UpdatedSince: "yesterday"}); err == nil {
		t.Fatal("bad cursor accepted")
	}
}

// Writes refuse a dependency cycle, an unknown or own key, duplicate or invalid keys, a new task without
// a title, and a milestone without tasks; a refused milestone_set changes nothing.
func TestMilestoneRefusesCyclesAndUnknownKeys(t *testing.T) {
	m := newManager(t)
	ctx := context.Background()
	for name, c := range map[string]struct {
		tasks []MilestoneTaskInput
		want  string
	}{
		"cycle":     {[]MilestoneTaskInput{{Key: "T1", Title: "a", DependsOn: deps("T2")}, {Key: "T2", Title: "b", DependsOn: deps("T1")}}, "cycle"},
		"unknown":   {[]MilestoneTaskInput{{Key: "T1", Title: "a", DependsOn: deps("T9")}}, "not a task"},
		"self":      {[]MilestoneTaskInput{{Key: "T1", Title: "a", DependsOn: deps("T1")}}, "itself"},
		"duplicate": {[]MilestoneTaskInput{{Key: "T1", Title: "a"}, {Key: "T1", Title: "b"}}, "twice"},
		"bad key":   {[]MilestoneTaskInput{{Key: "T 1", Title: "a"}}, "task key"},
		"no title":  {[]MilestoneTaskInput{{Key: "T1"}}, "title is required"},
		"no tasks":  {nil, "at least one task"},
	} {
		if _, err := m.CreateMilestone(ctx, MilestoneCreate{Title: "m", Tasks: c.tasks}); err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: %v, want %q", name, err, c.want)
		}
	}
	if _, err := m.CreateMilestone(ctx, MilestoneCreate{Title: "", Tasks: []MilestoneTaskInput{{Key: "T1", Title: "a"}}}); err == nil {
		t.Error("milestone without a title accepted")
	}
	v, err := m.CreateMilestone(ctx, MilestoneCreate{Title: "m", Tasks: []MilestoneTaskInput{
		{Key: "T1", Title: "a"}, {Key: "T2", Title: "b", DependsOn: deps("T1")}, {Key: "T3", Title: "c", DependsOn: deps("T2")}}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := m.SetMilestone(ctx, MilestoneSet{ID: v.ID, Title: "renamed", Tasks: []MilestoneTaskInput{{Key: "T1", DependsOn: deps("T3")}}}); err == nil ||
		!strings.Contains(err.Error(), "T1 -> T3 -> T2 -> T1") {
		t.Fatalf("cycle through set: %v", err)
	}
	if _, err := m.SetMilestone(ctx, MilestoneSet{ID: v.ID, Drop: []string{"T7"}}); err == nil {
		t.Fatal("drop of an unknown key accepted")
	}
	after := tree(t, m, v.ID)
	if after.Title != "m" || len(taskOf(t, after, "T1").DependsOn) != 0 {
		t.Fatalf("a refused set changed the milestone: %+v", after)
	}
	if _, err := m.SetMilestone(ctx, MilestoneSet{ID: 999, Title: "x"}); err == nil || !strings.Contains(err.Error(), "not found") {
		t.Fatalf("unknown id: %v", err)
	}
}

// Task status follows its jobs through a real lifecycle: running while queued, review when it
// succeeded, done once accepted (which unblocks its dependents), failed when cancelled; job_start
// refuses a blocked or dropped task; two independent tasks are ready at once; every job change moves
// the milestone's updated_at.
func TestMilestoneTaskLifecycle(t *testing.T) {
	m := newManager(t)
	ctx := context.Background()
	clock := m.Now
	past := time.Now().Add(-time.Hour)
	m.Now = func() time.Time { return past }
	v, err := m.CreateMilestone(ctx, MilestoneCreate{Title: "m", Tasks: []MilestoneTaskInput{
		{Key: "T1", Title: "base"}, {Key: "T2", Title: "next", DependsOn: deps("T1")}, {Key: "T3", Title: "side", DependsOn: deps("T1")},
	}})
	if err != nil {
		t.Fatal(err)
	}
	m.Now = clock
	// updated_at is stamped by the database's clock, in seconds: the job below is attached in a later one.
	time.Sleep(time.Until(time.Now().Truncate(time.Second).Add(time.Second)))
	onTask := func(key string) func(*StartRequest) {
		return func(r *StartRequest) { r.Milestone, r.Task = v.ID, key }
	}
	if _, err := m.Enqueue(StartRequest{Title: "x", Kind: "general", Brief: "b", Model: "sonnet", Milestone: v.ID, Task: "T2"}); err == nil ||
		!strings.Contains(err.Error(), "blocked") || !strings.Contains(err.Error(), "T1 ready") {
		t.Fatalf("job_start on a blocked task: %v", err)
	}
	if _, err := m.Enqueue(StartRequest{Title: "x", Kind: "general", Brief: "b", Model: "sonnet", Milestone: v.ID}); err == nil {
		t.Fatal("milestone without task accepted")
	}
	if _, err := m.Enqueue(StartRequest{Title: "x", Kind: "general", Brief: "b", Model: "sonnet", Milestone: v.ID, Task: "T9"}); err == nil {
		t.Fatal("unknown task accepted")
	}
	j1 := enqueue(t, m, "do T1", onTask("T1"))
	if j1.MilestoneID != v.ID || j1.TaskKey != "T1" {
		t.Fatalf("job row: %+v", j1)
	}
	got := tree(t, m, v.ID)
	if got.UpdatedAt == v.UpdatedAt {
		t.Fatal("attaching a job did not move updated_at")
	}
	wantStatuses(t, got, map[string]string{"T1": TaskRunning, "T2": TaskBlocked})
	if js := taskOf(t, got, "T1").Jobs; len(js) != 1 || js[0].ID != j1.ID || js[0].Status != store.StatusQueued || js[0].Model == "" {
		t.Fatalf("task jobs: %+v", js)
	}
	runToEnd(t, m, j1.ID)
	wantStatuses(t, tree(t, m, v.ID), map[string]string{"T1": TaskReview, "T2": TaskBlocked})
	if err := m.SetOutcome(j1.ID, OutcomeAccepted, ""); err != nil {
		t.Fatal(err)
	}
	got = tree(t, m, v.ID)
	wantStatuses(t, got, map[string]string{"T1": TaskDone, "T2": TaskReady, "T3": TaskReady})
	if !slices.Equal(got.Ready, []string{"T2", "T3"}) || got.Progress.TasksDone != 1 {
		t.Fatalf("two independent tasks ready: %+v", got)
	}
	if js := taskOf(t, got, "T1").Jobs; js[0].Outcome != OutcomeAccepted || js[0].FinishedAt == "" || js[0].Turns != 3 || js[0].CostUSD != 0.25 {
		t.Fatalf("finished job view: %+v", js[0])
	}
	j2 := enqueue(t, m, "do T2", onTask("T2"))
	j3 := enqueue(t, m, "do T3", onTask("T3"))
	wantStatuses(t, tree(t, m, v.ID), map[string]string{"T2": TaskRunning, "T3": TaskRunning})
	if err := m.Cancel(j3.ID); err != nil {
		t.Fatal(err)
	}
	wantStatuses(t, tree(t, m, v.ID), map[string]string{"T3": TaskFailed})
	if _, err := m.SetMilestone(ctx, MilestoneSet{ID: v.ID, Drop: []string{"T3"}}); err != nil {
		t.Fatal(err)
	}
	if _, err := m.Enqueue(StartRequest{Title: "x", Kind: "general", Brief: "b", Model: "sonnet", Milestone: v.ID, Task: "T3"}); err == nil ||
		!strings.Contains(err.Error(), "dropped") {
		t.Fatalf("job_start on a dropped task: %v", err)
	}
	runToEnd(t, m, j2.ID)
	if err := m.SetOutcome(j2.ID, OutcomeAccepted, ""); err != nil {
		t.Fatal(err)
	}
	done, err := m.SetMilestone(ctx, MilestoneSet{ID: v.ID, Status: store.MilestoneDone, CloseNote: "shipped"})
	if err != nil || done.Status != store.MilestoneDone || done.Progress.TasksDone != 2 || done.Progress.TasksTotal != 2 {
		t.Fatalf("close as done: %+v %v", done, err)
	}
	if _, err := m.Enqueue(StartRequest{Title: "x", Kind: "general", Brief: "b", Model: "sonnet", Milestone: v.ID, Task: "T1"}); err == nil {
		t.Fatal("job_start on a closed milestone accepted")
	}
}

// ownStatus: the latest job decides; a discarded or a failed latest job (after an accepted one too) make
// the task failed; reworked (the work was used, changed by hand) is done like extended; rerun leaves it ready.
func TestMilestoneOwnStatus(t *testing.T) {
	job := func(status, outcome string) *store.Job { return &store.Job{Status: status, Outcome: outcome} }
	for _, c := range []struct {
		jobs []*store.Job
		want string
	}{
		{nil, TaskReady},
		{[]*store.Job{job(store.StatusStopped, "")}, TaskRunning},
		{[]*store.Job{job(store.StatusSucceeded, "")}, TaskReview},
		{[]*store.Job{job(store.StatusSucceeded, OutcomeExtended)}, TaskDone},
		{[]*store.Job{job(store.StatusSucceeded, OutcomeAccepted), job(store.StatusTimeout, "")}, TaskFailed},
		{[]*store.Job{job(store.StatusSucceeded, OutcomeReworked)}, TaskDone},
		{[]*store.Job{job(store.StatusSucceeded, OutcomeDiscarded)}, TaskFailed},
		{[]*store.Job{job(store.StatusSucceeded, OutcomeRerun)}, TaskReady},
	} {
		if got := ownStatus(c.jobs); got != c.want {
			t.Errorf("%v: %s, want %s", c.jobs, got, c.want)
		}
	}
}

// max_parallel 1 runs the milestone's jobs one after another even with free daemon slots.
func TestMilestoneMaxParallel(t *testing.T) {
	m := newManager(t)
	v, err := m.CreateMilestone(context.Background(), MilestoneCreate{Title: "m", MaxParallel: 1,
		Tasks: []MilestoneTaskInput{{Key: "T1", Title: "a"}, {Key: "T2", Title: "b"}}})
	if err != nil {
		t.Fatal(err)
	}
	a := enqueue(t, m, "a", func(r *StartRequest) { r.Milestone, r.Task = v.ID, "T1" })
	b := enqueue(t, m, "b", func(r *StartRequest) { r.Milestone, r.Task = v.ID, "T2" })
	if err := m.Tick(); err != nil {
		t.Fatal(err)
	}
	if ja, jb := get(t, m, a.ID), get(t, m, b.ID); ja.Status != store.StatusRunning || jb.Status != store.StatusQueued {
		t.Fatalf("after one tick: %s %s", ja.Status, jb.Status)
	}
	waitFor(t, "first job", 15*time.Second, func() bool { _, ok := readExitCode(m.JobDir(a.ID)); return ok })
	if j := runToEnd(t, m, b.ID); j.Status != store.StatusSucceeded {
		t.Fatalf("second job: %s %s", j.Status, j.Error)
	}
}

// A job on a task with depends_on gets a "Prior results" section (the dependency's accepted job, its
// ref and diffstat, the Summary and Findings of its report, redacted, capped) and the full report in
// prior/<key>/report.md, which the guard protects; the milestone's branch is the default base.
func TestMilestonePriorResults(t *testing.T) {
	m := newManager(t)
	repo := newRepo(t)
	gitT(t, repo, "checkout", "-qb", "integ")
	if err := os.WriteFile(filepath.Join(repo, "integ.txt"), []byte("i\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitT(t, repo, "add", ".")
	gitT(t, repo, "commit", "-qm", "integ")
	integ := gitT(t, repo, "rev-parse", "HEAD")
	gitT(t, repo, "checkout", "-q", "main")
	v, err := m.CreateMilestone(context.Background(), MilestoneCreate{Title: "chain", Repo: repo, Branch: "integ",
		Tasks: []MilestoneTaskInput{{Key: "T1", Title: "first step"}, {Key: "T2", Title: "second step", DependsOn: deps("T1")}}})
	if err != nil {
		t.Fatal(err)
	}
	onTask := func(key string) func(*StartRequest) {
		return func(r *StartRequest) { r.Milestone, r.Task, r.RepoPath = v.ID, key, repo }
	}
	j1 := enqueue(t, m, "do T1", onTask("T1"))
	if j1.BaseCommit != integ || j1.BaseBranch != "refs/heads/integ" {
		t.Fatalf("base %s (%s), want the milestone branch %s", j1.BaseCommit, j1.BaseBranch, integ)
	}
	runToEnd(t, m, j1.ID)
	work := filepath.Join(m.JobDir(j1.ID), "work")
	if err := os.WriteFile(filepath.Join(work, "added.txt"), []byte("new\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitT(t, work, "add", "added.txt")
	gitT(t, work, "commit", "-qm", "T1 work")
	secret := "ghp_" + strings.Repeat("a1B2", 9)
	report := "## Summary\nPRIOR_SUMMARY_MARKER token " + secret + "\n\n## Findings / Changes\nPRIOR_FINDINGS_MARKER\n" +
		strings.Repeat("long finding line\n", 400) + "\n## Evidence\nEVIDENCE_NOT_IN_PROMPT\n\n## Not done / uncertain\nnothing\n"
	rp := filepath.Join(m.JobDir(j1.ID), "report.md")
	_ = os.Chmod(rp, 0o600)
	if err := os.WriteFile(rp, []byte(report), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := m.SetOutcome(j1.ID, OutcomeAccepted, ""); err != nil {
		t.Fatal(err)
	}
	j2 := enqueue(t, m, "do T2", onTask("T2"))
	runToEnd(t, m, j2.ID)
	dir := m.JobDir(j2.ID)
	prompt, err := os.ReadFile(filepath.Join(dir, "prompt.md"))
	if err != nil {
		t.Fatal(err)
	}
	p := string(prompt)
	_, section, _ := strings.Cut(p, "## Prior results")
	section, _, _ = strings.Cut(section, "## Checklist")
	for _, s := range []string{"### T1 · first step", "Used job " + j1.ID + " (outcome accepted) · ref: ", "refs/agent-jobs/jobs/" + j1.ID,
		"added.txt", "PRIOR_SUMMARY_MARKER", "PRIOR_FINDINGS_MARKER", "[... cut by agent-jobs]",
		"Full report: " + filepath.Join(dir, "prior", "T1", "report.md")} {
		if !strings.Contains(section, s) {
			t.Errorf("prior results lack %q:\n%s", s, section)
		}
	}
	if strings.Contains(p, secret) || strings.Contains(section, "EVIDENCE_NOT_IN_PROMPT") {
		t.Error("prior results carry the secret or the Evidence section")
	}
	if n := len(section); n > MaxPriorDepBytes+1024 {
		t.Errorf("prior results are %d bytes, the cap is %d per dependency", n, MaxPriorDepBytes)
	}
	copied, err := os.ReadFile(filepath.Join(dir, "prior", "T1", "report.md"))
	if err != nil || !strings.Contains(string(copied), "EVIDENCE_NOT_IN_PROMPT") || strings.Contains(string(copied), secret) {
		t.Fatalf("prior/T1/report.md: %v %.200q", err, copied)
	}
	if fi, _ := os.Stat(filepath.Join(dir, "prior", "T1", "report.md")); fi.Mode().Perm() != 0o400 {
		t.Errorf("prior report mode %v", fi.Mode().Perm())
	}
	var settings map[string]any
	b, _ := os.ReadFile(filepath.Join(dir, "settings.json"))
	if err := json.Unmarshal(b, &settings); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(b), "--protect '"+filepath.Join(dir, "prior")+"'") {
		t.Error("the guard does not protect the job's prior folder")
	}
	// A job on a task without dependencies gets no section.
	if strings.Contains(m.PriorResults(j1), "Prior results") {
		t.Error("a task without depends_on got prior results")
	}
}

// A milestone tied to the repo's checklist takes only its live rows as covers; its tree counts the
// covered rows, lists the live rows no task covers, and takes acceptance_met from the latest judge
// verdict (null before one).
func TestMilestoneChecklistCovers(t *testing.T) {
	m := newManager(t)
	ctx := context.Background()
	repo := newRepo(t)
	c := &checklist.Checklist{ID: "ms", Version: 1, Rows: []checklist.Row{
		{ID: "R01", Verbatim: "one", Contract: checklist.ContractLive, Rationale: "r"},
		{ID: "R02", Verbatim: "two", Contract: checklist.ContractLive, Rationale: "r"},
		{ID: "R03", Verbatim: "three", Contract: "deferred", Rationale: "r"},
	}}
	if err := os.MkdirAll(filepath.Join(repo, "brief"), 0o755); err != nil {
		t.Fatal(err)
	}
	for rel, text := range map[string]string{checklist.ChecklistRel: checklist.Render(c), checklist.BriefRel: checklist.InitBrief("words")} {
		if err := os.WriteFile(filepath.Join(repo, filepath.FromSlash(rel)), []byte(text), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	in := func(covers ...string) MilestoneCreate {
		return MilestoneCreate{Title: "m", Repo: repo, ChecklistID: "ms", Tasks: []MilestoneTaskInput{{Key: "T1", Title: "a", Covers: &covers}}}
	}
	if _, err := m.CreateMilestone(ctx, in("R03")); err == nil || !strings.Contains(err.Error(), "not a live row") {
		t.Fatalf("a deferred row as cover: %v", err)
	}
	wrong := in("R01")
	wrong.ChecklistID = "other"
	if _, err := m.CreateMilestone(ctx, wrong); err == nil || !strings.Contains(err.Error(), "not the id") {
		t.Fatalf("wrong checklist id: %v", err)
	}
	norepo := in("R01")
	norepo.Repo = ""
	if _, err := m.CreateMilestone(ctx, norepo); err == nil || !strings.Contains(err.Error(), "needs a repo") {
		t.Fatalf("checklist without repo: %v", err)
	}
	v, err := m.CreateMilestone(ctx, in("R01"))
	if err != nil {
		t.Fatal(err)
	}
	if v.Progress.AcceptanceTotal != 1 || v.Progress.AcceptanceMet != nil || !slices.Equal(v.Uncovered, []string{"R02"}) || v.Note != "" {
		t.Fatalf("tree before a verdict: %+v %+v", v, v.Progress)
	}
	if _, err := m.SetMilestone(ctx, MilestoneSet{ID: v.ID, Tasks: []MilestoneTaskInput{{Key: "T2", Title: "b", Covers: deps("R09")}}}); err == nil {
		t.Fatal("milestone_set took a cover that is not a row")
	}
	judge := &store.Job{ID: "judge-1", Agent: "test", Title: "judge", Kind: KindJudge, Brief: "-", Status: store.StatusRunning,
		Model: "opus", MaxTurns: 1, MaxBudgetUSD: 1, TimeoutMinutes: 1, CreatedAt: time.Now(), LaunchToken: "tok", Attempt: 1}
	if err := m.Store.Insert(judge); err != nil {
		t.Fatal(err)
	}
	verdict := map[string]any{"requirements": []any{map[string]any{"id": "R01", "verdict": "met"}, map[string]any{"id": "R02", "verdict": "met"}}}
	if _, err := m.Store.PutVerdict("tok", store.JudgeVerdict{JobID: judge.ID, Repo: v.Repo, SHA: "abc", ChecklistID: "ms", Version: 1,
		ChecklistSHA256: "h", Verdict: verdict}); err != nil {
		t.Fatal(err)
	}
	if got := tree(t, m, v.ID); got.Progress.AcceptanceMet == nil || *got.Progress.AcceptanceMet != 1 || got.Progress.AcceptanceTotal != 1 {
		t.Fatalf("acceptance after a verdict: %+v", got.Progress)
	}
	// R01 is retired: a title edit of the task that covers it still goes through, new covers on it do not.
	c.Rows[0].Contract = "deferred"
	if err := os.WriteFile(filepath.Join(repo, filepath.FromSlash(checklist.ChecklistRel)), []byte(checklist.Render(c)), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := m.SetMilestone(ctx, MilestoneSet{ID: v.ID, Tasks: []MilestoneTaskInput{{Key: "T1", Title: "renamed"}}}); err != nil {
		t.Fatalf("title edit with a retired cover: %v", err)
	}
	if _, err := m.SetMilestone(ctx, MilestoneSet{ID: v.ID, Tasks: []MilestoneTaskInput{{Key: "T1", Covers: deps("R01")}}}); err == nil {
		t.Fatal("a retired row given as a new cover was accepted")
	}
	// Without its checklist the tree still answers, with a note.
	if err := os.Remove(filepath.Join(repo, filepath.FromSlash(checklist.ChecklistRel))); err != nil {
		t.Fatal(err)
	}
	if got := tree(t, m, v.ID); got.Note == "" || got.Uncovered != nil {
		t.Fatalf("tree without the checklist: %+v", got)
	}
}

// capBytes keeps a cut part within its cap, ends it with the marker and closes a fence it cut through.
func TestPriorCapBytesClosesFence(t *testing.T) {
	s := "head\n```\n" + strings.Repeat("diffstat line\n", 100) + "```\ntail\n"
	out := capBytes(s, 200)
	if len(out) > 200 || !strings.HasSuffix(out, priorCut) || fenceOpen(out) {
		t.Fatalf("cut: %d bytes, open fence %v: %q", len(out), fenceOpen(out), out)
	}
	if capBytes("short", 200) != "short" {
		t.Fatal("a short text was cut")
	}
	if cut := capBytes(strings.Repeat("я", 300), 101); !utf8.ValidString(cut) || len(cut) > 101 {
		t.Fatalf("cut through a rune: %q", cut)
	}
}
