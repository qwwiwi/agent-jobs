package jobs

import (
	"context"
	"database/sql"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/qwwiwi/agent-jobs/internal/store"
)

// A milestone edit that commits between a queued job's launch check and its claim (a dependency added,
// the task dropped, the milestone closed) is seen by the claim: the job does not start. The next tick
// leaves the blocked one queued and cancels the others with the reason.
func TestMilestoneClaimRechecksEligibility(t *testing.T) {
	m := newManager(t)
	ctx := context.Background()
	v, err := m.CreateMilestone(ctx, MilestoneCreate{Title: "m", Tasks: []MilestoneTaskInput{
		{Key: "T1", Title: "base"}, {Key: "T2", Title: "next"}, {Key: "T3", Title: "side"}}})
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
	edits := map[string]MilestoneSet{
		j2.ID: {ID: v.ID, Tasks: []MilestoneTaskInput{{Key: "T2", DependsOn: deps("T1")}}},
		j3.ID: {ID: v.ID, Drop: []string{"T3"}},
		j4.ID: {ID: other.ID, Status: store.MilestoneDropped},
	}
	m.beforeClaim = func(j *store.Job) {
		if in, ok := edits[j.ID]; ok {
			if _, err := m.SetMilestone(ctx, in); err != nil {
				t.Errorf("edit before the claim of %s: %v", j.ID, err)
			}
		}
	}
	if err := m.Tick(); err != nil {
		t.Fatal(err)
	}
	m.beforeClaim = nil
	for _, id := range []string{j2.ID, j3.ID, j4.ID} {
		if j := get(t, m, id); j.Status != store.StatusQueued || j.Attempt != 0 {
			t.Errorf("job %s edited out between check and claim: %s attempt %d, want queued, not started", id, j.Status, j.Attempt)
		}
	}
	if err := m.Tick(); err != nil {
		t.Fatal(err)
	}
	if j := get(t, m, j2.ID); j.Status != store.StatusQueued {
		t.Errorf("job of a task blocked before its claim: %s, want queued", j.Status)
	}
	for _, id := range []string{j3.ID, j4.ID} {
		if j := get(t, m, id); j.Status != store.StatusCancelled || !strings.Contains(j.Error, "dropped") {
			t.Errorf("job %s on the next tick: %s %q, want cancelled with the reason", id, j.Status, j.Error)
		}
	}
}

// A write with a skewed Go clock (far ahead) does not stamp its milestone ahead of later writes, and a
// milestone stamped ahead of the database clock (the clock stepped back) does not hide another
// milestone's later change: a poll with the cursor gets that change.
func TestMilestoneCursorIgnoresSkewedClocks(t *testing.T) {
	m := newManager(t)
	ctx := context.Background()
	a, err := m.CreateMilestone(ctx, MilestoneCreate{Title: "a", Tasks: []MilestoneTaskInput{{Key: "T1", Title: "x"}}})
	if err != nil {
		t.Fatal(err)
	}
	b, err := m.CreateMilestone(ctx, MilestoneCreate{Title: "b", Tasks: []MilestoneTaskInput{{Key: "T1", Title: "y"}}})
	if err != nil {
		t.Fatal(err)
	}
	poll := func(what string, change func()) {
		t.Helper()
		list, err := m.ListMilestones(ctx, MilestoneListRequest{})
		if err != nil {
			t.Fatal(err)
		}
		change()
		again, err := m.ListMilestones(ctx, MilestoneListRequest{UpdatedSince: list.Cursor})
		if err != nil {
			t.Fatal(err)
		}
		if !slices.ContainsFunc(again.Milestones, func(v MilestoneView) bool { return v.ID == b.ID }) {
			t.Errorf("%s: the change of milestone %d is not in the poll since cursor %s: %+v", what, b.ID, list.Cursor, again.Milestones)
		}
	}
	clock := m.Now
	m.Now = func() time.Time { return time.Now().Add(time.Hour) }
	if _, err := m.SetMilestone(ctx, MilestoneSet{ID: a.ID, Title: "a2"}); err != nil {
		t.Fatal(err)
	}
	m.Now = clock
	poll("Go clock ahead", func() {
		if _, err := m.SetMilestone(ctx, MilestoneSet{ID: b.ID, Title: "b2"}); err != nil {
			t.Fatal(err)
		}
	})
	db, err := sql.Open("sqlite", filepath.Join(m.StateDir, "jobs.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Exec(`UPDATE milestones SET updated_at = updated_at + 3600 WHERE id = ?`, a.ID); err != nil {
		t.Fatal(err)
	}
	poll("database clock stepped back", func() {
		if _, err := m.SetMilestone(ctx, MilestoneSet{ID: b.ID, Title: "b3"}); err != nil {
			t.Fatal(err)
		}
	})
	poll("job of the milestone added", func() {
		enqueue(t, m, "do b", func(r *StartRequest) { r.Milestone, r.Task = b.ID, "T1" })
	})
}

// A queued job whose done dependency got a new job that failed waits, and says why: job_status and
// job_list show the waiting reason, milestone_get shows it on the task. It is not cancelled.
func TestMilestoneWaitingReason(t *testing.T) {
	m := newManager(t)
	ctx := context.Background()
	v, err := m.CreateMilestone(ctx, MilestoneCreate{Title: "m", Tasks: []MilestoneTaskInput{
		{Key: "T1", Title: "base"}, {Key: "T2", Title: "next", DependsOn: deps("T1")}}})
	if err != nil {
		t.Fatal(err)
	}
	onTask := func(key string) func(*StartRequest) {
		return func(r *StartRequest) { r.Milestone, r.Task = v.ID, key }
	}
	j1 := enqueue(t, m, "do T1", onTask("T1"))
	runToEnd(t, m, j1.ID)
	if err := m.SetOutcome(j1.ID, OutcomeAccepted, ""); err != nil {
		t.Fatal(err)
	}
	j2 := enqueue(t, m, "do T2", onTask("T2"))
	rework := enqueue(t, m, "rework T1", onTask("T1"))
	if err := m.Cancel(rework.ID); err != nil {
		t.Fatal(err)
	}
	if err := m.Tick(); err != nil {
		t.Fatal(err)
	}
	j := get(t, m, j2.ID)
	if j.Status != store.StatusQueued {
		t.Fatalf("job of a task whose dependency failed again: %s %q, want queued", j.Status, j.Error)
	}
	if got := m.MilestoneWaiting(ctx, j); got != "waiting for T1 (failed)" {
		t.Errorf("waiting reason: %q", got)
	}
	tree, err := m.MilestoneTree(ctx, v.ID)
	if err != nil {
		t.Fatal(err)
	}
	if i := slices.IndexFunc(tree.Tasks, func(t MilestoneTaskView) bool { return t.Key == "T2" }); i < 0 ||
		tree.Tasks[i].Waiting != "waiting for T1 (failed)" {
		t.Errorf("milestone_get task T2: %+v", tree.Tasks)
	}
	if got := m.MilestoneWaiting(ctx, get(t, m, j1.ID)); got != "" {
		t.Errorf("waiting reason of a finished job: %q", got)
	}
}

// The prior results name the outcome of the used job: a reworked or extended job's ref does not hold
// what was done after it.
func TestMilestonePriorNamesOutcome(t *testing.T) {
	for _, tc := range []struct{ outcome, want string }{
		{OutcomeAccepted, "(outcome accepted)"},
		{OutcomeExtended, "(outcome extended: more work was added on top after the job; this ref does not contain it)"},
		{OutcomeReworked, "(outcome reworked: fixed by hand after the job; this ref does not contain the fix)"},
	} {
		m := newManager(t)
		v, err := m.CreateMilestone(context.Background(), MilestoneCreate{Title: "m", Tasks: []MilestoneTaskInput{
			{Key: "T1", Title: "base"}, {Key: "T2", Title: "next", DependsOn: deps("T1")}}})
		if err != nil {
			t.Fatal(err)
		}
		j1 := enqueue(t, m, "do T1", func(r *StartRequest) { r.Milestone, r.Task = v.ID, "T1" })
		runToEnd(t, m, j1.ID)
		if err := m.SetOutcome(j1.ID, tc.outcome, ""); err != nil {
			t.Fatal(err)
		}
		s := m.PriorResults(&store.Job{ID: "x", MilestoneID: v.ID, TaskKey: "T2"})
		if !strings.Contains(s, "Used job "+j1.ID+" "+tc.want+" · ref: ") || strings.Contains(s, "Accepted job") {
			t.Errorf("prior results of a %s job:\n%s", tc.outcome, s)
		}
	}
}
