package jobs

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/qwwiwi/agent-jobs/internal/store"
)

// A queued job on a task is checked again at launch: one whose task got an unfinished dependency after
// job_start stays queued (and starts once the dependency is done), one whose task was dropped or whose
// milestone was closed is cancelled with the reason; job_resume refuses a job of a dropped task and
// queues one of a blocked task, which then waits too.
func TestMilestoneLaunchRechecks(t *testing.T) {
	m := newManager(t)
	ctx := context.Background()
	v, err := m.CreateMilestone(ctx, MilestoneCreate{Title: "m", Tasks: []MilestoneTaskInput{
		{Key: "T1", Title: "base"}, {Key: "T2", Title: "next"}, {Key: "T3", Title: "side"}, {Key: "T4", Title: "later"}}})
	if err != nil {
		t.Fatal(err)
	}
	other, err := m.CreateMilestone(ctx, MilestoneCreate{Title: "other", Tasks: []MilestoneTaskInput{{Key: "T1", Title: "x"}}})
	if err != nil {
		t.Fatal(err)
	}
	onTask := func(id int64, key string) func(*StartRequest) {
		return func(r *StartRequest) { r.Milestone, r.Task = id, key }
	}
	j2 := enqueue(t, m, "do T2", onTask(v.ID, "T2"))
	j3 := enqueue(t, m, "do T3", onTask(v.ID, "T3"))
	j4 := enqueue(t, m, "do other T1", onTask(other.ID, "T1"))
	j5 := enqueue(t, m, "do T4", onTask(v.ID, "T4"))
	if _, err := m.Stop(j5.ID, ""); err != nil {
		t.Fatal(err)
	}
	if _, err := m.SetMilestone(ctx, MilestoneSet{ID: v.ID, Tasks: []MilestoneTaskInput{{Key: "T2", DependsOn: deps("T1")}},
		Drop: []string{"T3", "T4"}}); err != nil {
		t.Fatal(err)
	}
	if _, err := m.SetMilestone(ctx, MilestoneSet{ID: other.ID, Status: store.MilestoneDropped}); err != nil {
		t.Fatal(err)
	}
	if err := m.Tick(); err != nil {
		t.Fatal(err)
	}
	if j := get(t, m, j2.ID); j.Status != store.StatusQueued {
		t.Errorf("job of a task blocked after job_start: %s %q, want queued", j.Status, j.Error)
	}
	if j := get(t, m, j3.ID); j.Status != store.StatusCancelled || !strings.Contains(j.Error, "T3") || !strings.Contains(j.Error, "dropped") {
		t.Errorf("job of a dropped task: %s %q, want cancelled with the reason", j.Status, j.Error)
	}
	if j := get(t, m, j4.ID); j.Status != store.StatusCancelled || !strings.Contains(j.Error, "is dropped") {
		t.Errorf("job of a closed milestone: %s %q, want cancelled with the reason", j.Status, j.Error)
	}
	if _, err := m.Resume(j5.ID, "go on", Limits{}); err == nil || !strings.Contains(err.Error(), "dropped") {
		t.Errorf("resume of a job of a dropped task: %v", err)
	}
	// A blocked task's job is resumed and waits queued too.
	if _, err := m.Stop(j2.ID, ""); err != nil {
		t.Fatal(err)
	}
	if _, err := m.Resume(j2.ID, "go on", Limits{}); err != nil {
		t.Fatalf("resume of a job of a blocked task: %v", err)
	}
	if err := m.Tick(); err != nil {
		t.Fatal(err)
	}
	if j := get(t, m, j2.ID); j.Status != store.StatusQueued {
		t.Fatalf("resumed job of a blocked task: %s, want queued", j.Status)
	}
	// Once its dependency is done, it starts.
	j1 := enqueue(t, m, "do T1", onTask(v.ID, "T1"))
	runToEnd(t, m, j1.ID)
	if err := m.SetOutcome(j1.ID, OutcomeAccepted, ""); err != nil {
		t.Fatal(err)
	}
	if j := runToEnd(t, m, j2.ID); j.Status != store.StatusSucceeded {
		t.Fatalf("job after its dependency is done: %s %q", j.Status, j.Error)
	}
}

// Blocked replaces only ready: a task whose dependency is not done keeps running, review or done when
// its own jobs say so; a reworked job (the work was used) makes its task done, like extended.
func TestMilestoneBlockedOnlyOverReady(t *testing.T) {
	tasks := []store.MilestoneTask{{Key: "T1", Title: "base"},
		{Key: "T2", DependsOn: []string{"T1"}}, {Key: "T3", DependsOn: []string{"T1"}},
		{Key: "T4", DependsOn: []string{"T1"}}, {Key: "T5", DependsOn: []string{"T1"}}, {Key: "T6", DependsOn: []string{"T1"}}}
	jobs := []*store.Job{
		{TaskKey: "T2", Status: store.StatusRunning},
		{TaskKey: "T3", Status: store.StatusSucceeded},
		{TaskKey: "T4", Status: store.StatusSucceeded, Outcome: OutcomeAccepted},
		{TaskKey: "T5", Status: store.StatusFailed},
	}
	want := map[string]string{"T1": TaskReady, "T2": TaskRunning, "T3": TaskReview, "T4": TaskDone, "T5": TaskFailed, "T6": TaskBlocked}
	got := TaskStatuses(tasks, jobs)
	for k, s := range want {
		if got[k] != s {
			t.Errorf("task %s: %s, want %s", k, got[k], s)
		}
	}
	jobs = append(jobs, &store.Job{TaskKey: "T1", Status: store.StatusSucceeded, Outcome: OutcomeReworked})
	if got := TaskStatuses(tasks, jobs); got["T1"] != TaskDone || got["T6"] != TaskReady {
		t.Errorf("a reworked dependency: T1 %s, T6 %s; want done, ready", got["T1"], got["T6"])
	}
}

// A job whose dependency's latest used job is reworked gets it as its prior result.
func TestMilestonePriorTakesReworked(t *testing.T) {
	m := newManager(t)
	v, err := m.CreateMilestone(context.Background(), MilestoneCreate{Title: "m", Tasks: []MilestoneTaskInput{
		{Key: "T1", Title: "base"}, {Key: "T2", Title: "next", DependsOn: deps("T1")}}})
	if err != nil {
		t.Fatal(err)
	}
	j1 := enqueue(t, m, "do T1", func(r *StartRequest) { r.Milestone, r.Task = v.ID, "T1" })
	runToEnd(t, m, j1.ID)
	if err := m.SetOutcome(j1.ID, OutcomeReworked, ""); err != nil {
		t.Fatal(err)
	}
	if s := m.PriorResults(&store.Job{ID: "x", MilestoneID: v.ID, TaskKey: "T2"}); !strings.Contains(s, "Accepted job: "+j1.ID) {
		t.Fatalf("prior results without the reworked job:\n%s", s)
	}
}

// Adding unfinished work to a done milestone reopens it; closing as done with an unfinished task is
// refused even when the milestone already is done; an edit without new work leaves it done.
func TestMilestoneDoneReopensOnNewWork(t *testing.T) {
	m := newManager(t)
	ctx := context.Background()
	v, err := m.CreateMilestone(ctx, MilestoneCreate{Title: "m", Tasks: []MilestoneTaskInput{{Key: "T1", Title: "a"}}})
	if err != nil {
		t.Fatal(err)
	}
	j1 := enqueue(t, m, "do T1", func(r *StartRequest) { r.Milestone, r.Task = v.ID, "T1" })
	runToEnd(t, m, j1.ID)
	if err := m.SetOutcome(j1.ID, OutcomeAccepted, ""); err != nil {
		t.Fatal(err)
	}
	if v, err = m.SetMilestone(ctx, MilestoneSet{ID: v.ID, Status: store.MilestoneDone}); err != nil || v.Status != store.MilestoneDone {
		t.Fatalf("close: %+v %v", v, err)
	}
	if v, err = m.SetMilestone(ctx, MilestoneSet{ID: v.ID, Title: "renamed"}); err != nil || v.Status != store.MilestoneDone {
		t.Fatalf("a title edit: %s %v, want done", v.Status, err)
	}
	if _, err := m.SetMilestone(ctx, MilestoneSet{ID: v.ID, Status: store.MilestoneDone,
		Tasks: []MilestoneTaskInput{{Key: "T9", Title: "more"}}}); err == nil || !strings.Contains(err.Error(), "cannot close as done") {
		t.Fatalf("done with a new unfinished task: %v", err)
	}
	v, err = m.SetMilestone(ctx, MilestoneSet{ID: v.ID, Tasks: []MilestoneTaskInput{{Key: "T2", Title: "more"}}})
	if err != nil || v.Status != store.MilestoneOpen || v.ClosedAt != "" {
		t.Fatalf("new work on a done milestone: status %s closed_at %q %v, want open", v.Status, v.ClosedAt, err)
	}
	if j := enqueue(t, m, "do T2", func(r *StartRequest) { r.Milestone, r.Task = v.ID, "T2" }); j.TaskKey != "T2" {
		t.Fatalf("job on the reopened milestone: %+v", j)
	}
}

// updated_at is stamped by the database's clock inside the write (not a time taken before the write
// lock), and milestone_list returns the cursor to pass as the next updated_since: the highest
// updated_at, so the latest change is in the next answer.
func TestMilestoneCursorStampedInSQL(t *testing.T) {
	m := newManager(t)
	ctx := context.Background()
	start := time.Now().Truncate(time.Second)
	past := start.Add(-time.Hour)
	clock := m.Now
	m.Now = func() time.Time { return past } // a stamp taken long before the write
	v, err := m.CreateMilestone(ctx, MilestoneCreate{Title: "m", Tasks: []MilestoneTaskInput{{Key: "T1", Title: "a"}}})
	if err != nil {
		t.Fatal(err)
	}
	if v, err = m.SetMilestone(ctx, MilestoneSet{ID: v.ID, Title: "b"}); err != nil {
		t.Fatal(err)
	}
	m.Now = clock
	at, err := time.Parse(time.RFC3339, v.UpdatedAt)
	if err != nil || at.Before(start) {
		t.Fatalf("updated_at %s, want the time of the write (at or after %s)", v.UpdatedAt, start.UTC().Format(time.RFC3339))
	}
	list, err := m.ListMilestones(ctx, MilestoneListRequest{})
	if err != nil {
		t.Fatal(err)
	}
	b, _ := json.Marshal(list)
	var out map[string]any
	if err := json.Unmarshal(b, &out); err != nil {
		t.Fatal(err)
	}
	cursor, _ := out["cursor"].(string)
	if cursor != "" {
		if c, err := ParseMilestoneCursor(cursor); err != nil || !c.Equal(at) {
			t.Fatalf("cursor %q, want updated_at %s", cursor, v.UpdatedAt)
		}
	}
	if cursor == "" {
		t.Fatalf("milestone_list has no cursor: %s", b)
	}
	if again, err := m.ListMilestones(ctx, MilestoneListRequest{UpdatedSince: cursor}); err != nil || len(again.Milestones) != 1 {
		t.Fatalf("list since the cursor: %+v %v", again, err)
	}
}

// The prompt's prior results and the prior/ folder come from one read of the dependencies: a resumed
// job whose dependency got a newer accepted job meanwhile gets that job in both, prior/ refreshed, and
// so does the update of a resumed session.
func TestMilestonePriorRefreshedOnResume(t *testing.T) {
	m := newManager(t)
	sessionsHome(t, false) // fresh session: the fallback prompt carries the prior results
	v, err := m.CreateMilestone(context.Background(), MilestoneCreate{Title: "m", Tasks: []MilestoneTaskInput{
		{Key: "T1", Title: "base"}, {Key: "T2", Title: "next", DependsOn: deps("T1")}}})
	if err != nil {
		t.Fatal(err)
	}
	onTask := func(key string) func(*StartRequest) {
		return func(r *StartRequest) { r.Milestone, r.Task = v.ID, key }
	}
	accepted := func(marker string) *store.Job {
		t.Helper()
		j := enqueue(t, m, "do T1", onTask("T1"))
		runToEnd(t, m, j.ID)
		rp := filepath.Join(m.JobDir(j.ID), "report.md")
		_ = os.Chmod(rp, 0o600)
		if err := os.WriteFile(rp, []byte("## Summary\n"+marker+"\n\n## Findings / Changes\nf\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := m.SetOutcome(j.ID, OutcomeAccepted, ""); err != nil {
			t.Fatal(err)
		}
		return j
	}
	first := accepted("FIRST_T1_REPORT")
	j2 := startAndStop(t, m, onTask("T2"))
	dir := m.JobDir(j2.ID)
	if s := readFile(t, filepath.Join(dir, PriorDir, "T1", "report.md")); !strings.Contains(s, "FIRST_T1_REPORT") {
		t.Fatalf("first attempt prior/T1: %s", s)
	}
	if p := readFile(t, filepath.Join(dir, "prompt.md")); !strings.Contains(p, "Accepted job: "+first.ID) {
		t.Fatal("first prompt lacks the first T1 job")
	}
	second := accepted("SECOND_T1_REPORT")
	if _, err := m.Resume(j2.ID, "go on", Limits{MaxBudgetUSD: 4}); err != nil {
		t.Fatal(err)
	}
	j := runToEnd(t, m, j2.ID)
	if j.Status != store.StatusSucceeded {
		t.Fatalf("resumed: %s %q", j.Status, j.Error)
	}
	stdin := readFile(t, filepath.Join(dir, "stdin.txt"))
	if !strings.Contains(stdin, "Accepted job: "+second.ID) {
		t.Fatalf("the resumed prompt lacks the newer T1 job:\n%s", stdin)
	}
	if s := readFile(t, filepath.Join(dir, PriorDir, "T1", "report.md")); !strings.Contains(s, "SECOND_T1_REPORT") {
		t.Fatalf("prior/T1/report.md is not the job the prompt names: %.200s", s)
	}
	if s := readFile(t, filepath.Join(dir, resumePromptFile)); !strings.Contains(s, "Accepted job: "+second.ID) {
		t.Fatalf("the resumed session's update lacks the prior results:\n%s", s)
	}
}

// writePriorReports replaces what an earlier attempt left in prior/, even a folder made read-only.
func TestWritePriorReportsReplacesReadOnly(t *testing.T) {
	m := newManager(t)
	dir := t.TempDir()
	old := filepath.Join(dir, PriorDir, "T1")
	if err := os.MkdirAll(old, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(old, "report.md"), []byte("OLD"), 0o400); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(old, 0o500); err != nil {
		t.Fatal(err)
	}
	snap := priorSnapshot{deps: []priorDep{{key: "T1", job: &store.Job{ID: "j"}, report: "NEW"}}}
	if err := m.writePriorReports(snap, dir); err != nil {
		t.Fatal(err)
	}
	if s := readFile(t, filepath.Join(old, "report.md")); s != "NEW" {
		t.Fatalf("prior/T1/report.md: %q", s)
	}
}

// The prior results section stays within its cap, marker and newlines included, for any limit.
func TestPriorCapTinyLimits(t *testing.T) {
	long := "head\n```\n" + strings.Repeat("line\n", 50)
	for max := 0; max <= 60; max++ {
		if out := capBytes(long, max); len(out) > max || fenceOpen(out) {
			t.Fatalf("capBytes(%d): %d bytes %q", max, len(out), out)
		}
	}
	m := newManager(t)
	v, err := m.CreateMilestone(context.Background(), MilestoneCreate{Title: "m", Tasks: []MilestoneTaskInput{
		{Key: "T1", Title: "base"}, {Key: "T2", Title: "next", DependsOn: deps("T1")}}})
	if err != nil {
		t.Fatal(err)
	}
	j := &store.Job{ID: "x", MilestoneID: v.ID, TaskKey: "T2"}
	for _, n := range []int{1, 4, 40, 100, 120, 400} {
		m.Cfg.MaxBriefBytes = n
		if s := m.PriorResults(j); len(s) > n/4 {
			t.Errorf("max_brief_bytes %d: the section is %d bytes, the cap %d: %q", n, len(s), n/4, s)
		}
	}
}

// The docs show the JSON the code returns: the tree example leaves out empty omitempty fields and names
// repo_id, the list carries a cursor, and the task status rules match TaskStatuses.
func TestMilestoneDocsMatchCode(t *testing.T) {
	guide, err := os.ReadFile("../../docs/GUIDE.md")
	if err != nil {
		t.Fatal(err)
	}
	readme, err := os.ReadFile("../../README.md")
	if err != nil {
		t.Fatal(err)
	}
	_, rest, ok := strings.Cut(string(guide), "**Tree** (`milestone_get`")
	_, rest, ok2 := strings.Cut(rest, "```json\n")
	block, _, ok3 := strings.Cut(rest, "```")
	if !ok || !ok2 || !ok3 {
		t.Fatal("GUIDE.md has no tree example")
	}
	var ex map[string]any
	if err := json.Unmarshal([]byte(block), &ex); err != nil {
		t.Fatalf("the tree example is not JSON: %v", err)
	}
	omit := func(v any) map[string]bool {
		out := map[string]bool{}
		rt := reflect.TypeOf(v)
		for i := 0; i < rt.NumField(); i++ {
			name, opts, _ := strings.Cut(rt.Field(i).Tag.Get("json"), ",")
			out[name] = strings.Contains(opts, "omitempty")
		}
		return out
	}
	check := func(what string, obj map[string]any, fields map[string]bool) {
		for k, val := range obj {
			o, known := fields[k]
			if !known {
				t.Errorf("%s: %q is not a field of the code", what, k)
			}
			if s, isStr := val.(string); o && isStr && s == "" {
				t.Errorf("%s: %q is shown empty but is left out when empty", what, k)
			}
		}
	}
	check("tree", ex, omit(MilestoneView{}))
	if _, ok := ex["repo_id"]; !ok {
		t.Error("the tree example lacks repo_id")
	}
	for _, task := range ex["tasks"].([]any) {
		tm := task.(map[string]any)
		check("task", tm, omit(MilestoneTaskView{}))
		for _, j := range tm["jobs"].([]any) {
			check("job", j.(map[string]any), omit(MilestoneJobView{}))
		}
	}
	if !strings.Contains(string(guide), `{"milestones": [<milestone>, …], "cursor": "`) {
		t.Error("GUIDE.md's list shape lacks the cursor")
	}
	for name, text := range map[string]string{"GUIDE.md": string(guide), "README.md": string(readme)} {
		if !regexp.MustCompile("`done` \\([^)]*`accepted`, `extended` or `reworked`").MatchString(text) {
			t.Errorf("%s: done is not accepted, extended or reworked", name)
		}
		if strings.Contains(text, "or its outcome is `discarded` or `reworked`") {
			t.Errorf("%s: reworked still counts as failed", name)
		}
	}
}
