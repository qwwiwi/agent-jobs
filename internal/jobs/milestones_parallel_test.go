package jobs

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/qwwiwi/agent-jobs/internal/store"
)

// #119: a depends_on edge is kept only with a one-line reason; an edge without one is dropped and
// reported, so tasks that merely sit near each other in the plan do not wait for one another.
func TestMilestoneEdgesNeedReasons(t *testing.T) {
	ctx := context.Background()
	t.Run("an edge without a reason is dropped and reported, one with a reason is kept", func(t *testing.T) {
		m := newManager(t)
		v, err := m.CreateMilestone(ctx, MilestoneCreate{Title: "m", Tasks: []MilestoneTaskInput{
			{Key: "T1", Title: "orders"},
			{Key: "T2", Title: "margin", DependsOn: deps("T1")}, // no reason
			{Key: "T3", Title: "history", DependsOn: deps("T1", "T2"),
				DependsWhy: &map[string]string{"T1": "both edit internal/orders/place.go"}}, // T2 has none
		}})
		if err != nil {
			t.Fatal(err)
		}
		if want := []string{"T2 -> T1", "T3 -> T2"}; !slices.Equal(v.DependsDropped, want) {
			t.Fatalf("dropped %v, want %v", v.DependsDropped, want)
		}
		if got := taskOf(t, v, "T2"); len(got.DependsOn) != 0 || got.Status != TaskReady {
			t.Fatalf("T2 kept an edge with no reason: %+v", got)
		}
		t3 := taskOf(t, v, "T3")
		if !slices.Equal(t3.DependsOn, []string{"T1"}) || t3.DependsWhy["T1"] != "both edit internal/orders/place.go" || t3.Status != TaskBlocked {
			t.Fatalf("T3: %+v", t3)
		}
		// The reason is stored: a fresh read of the tree has it, and no dropped list.
		again := tree(t, m, v.ID)
		if taskOf(t, again, "T3").DependsWhy["T1"] == "" || len(again.DependsDropped) != 0 {
			t.Fatalf("after reload: %+v", again)
		}
		if !slices.Equal(again.Ready, []string{"T1", "T2"}) {
			t.Fatalf("ready %v", again.Ready)
		}
	})

	t.Run("milestone_set keeps the stored reason of an edge it repeats and drops a new bare one", func(t *testing.T) {
		m := newManager(t)
		v, err := m.CreateMilestone(ctx, MilestoneCreate{Title: "m", Tasks: []MilestoneTaskInput{
			{Key: "T1", Title: "a"}, {Key: "T2", Title: "b"},
			{Key: "T3", Title: "c", DependsOn: deps("T1"), DependsWhy: why("T1")},
		}})
		if err != nil {
			t.Fatal(err)
		}
		v, err = m.SetMilestone(ctx, MilestoneSet{ID: v.ID, Tasks: []MilestoneTaskInput{{Key: "T3", DependsOn: deps("T1", "T2")}}})
		if err != nil {
			t.Fatal(err)
		}
		t3 := taskOf(t, v, "T3")
		if !slices.Equal(t3.DependsOn, []string{"T1"}) || t3.DependsWhy["T1"] != "shares files" || !slices.Equal(v.DependsDropped, []string{"T3 -> T2"}) {
			t.Fatalf("T3 %+v dropped %v", t3, v.DependsDropped)
		}
		// Clearing the list clears the reasons too.
		v, err = m.SetMilestone(ctx, MilestoneSet{ID: v.ID, Tasks: []MilestoneTaskInput{{Key: "T3", DependsOn: deps()}}})
		if err != nil {
			t.Fatal(err)
		}
		if t3 := taskOf(t, v, "T3"); len(t3.DependsOn) != 0 || len(t3.DependsWhy) != 0 || t3.Status != TaskReady {
			t.Fatalf("T3 after clear: %+v", t3)
		}
	})

	t.Run("a reason for a key outside depends_on, or without depends_on, is refused", func(t *testing.T) {
		m := newManager(t)
		cases := map[string]MilestoneTaskInput{
			"stray key":  {Key: "T2", Title: "b", DependsOn: deps("T1"), DependsWhy: &map[string]string{"T9": "x"}},
			"no edges":   {Key: "T2", Title: "b", DependsWhy: &map[string]string{"T1": "x"}},
			"multi-line": {Key: "T2", Title: "b", DependsOn: deps("T1"), DependsWhy: &map[string]string{"T1": "a\nb"}},
			"too long":   {Key: "T2", Title: "b", DependsOn: deps("T1"), DependsWhy: &map[string]string{"T1": strings.Repeat("x", MaxDependsWhyRunes+1)}},
		}
		for name, task := range cases {
			_, err := m.CreateMilestone(ctx, MilestoneCreate{Title: "m", Tasks: []MilestoneTaskInput{{Key: "T1", Title: "a"}, task}})
			if err == nil {
				t.Errorf("%s: accepted", name)
			}
		}
	})

	t.Run("a cycle and an unknown key are still refused when the edges carry reasons", func(t *testing.T) {
		m := newManager(t)
		if _, err := m.CreateMilestone(ctx, MilestoneCreate{Title: "m", Tasks: []MilestoneTaskInput{
			{Key: "T1", Title: "a", DependsOn: deps("T2"), DependsWhy: why("T2")},
			{Key: "T2", Title: "b", DependsOn: deps("T1"), DependsWhy: why("T1")}}}); err == nil || !strings.Contains(err.Error(), "cycle") {
			t.Fatalf("cycle: %v", err)
		}
		if _, err := m.CreateMilestone(ctx, MilestoneCreate{Title: "m", Tasks: []MilestoneTaskInput{
			{Key: "T1", Title: "a", DependsOn: deps("T9"), DependsWhy: why("T9")}}}); err == nil || !strings.Contains(err.Error(), "not a task") {
			t.Fatalf("unknown: %v", err)
		}
	})
}

// #119 acceptance: on a milestone with independent tasks and no max_parallel, jobs start up to
// max_running without manual edits; only a justified edge holds a task back.
func TestMilestoneIndependentTasksFillSlots(t *testing.T) {
	m := newManager(t)
	five := 5
	m.Cfg.MaxRunning = &five
	v, err := m.CreateMilestone(context.Background(), MilestoneCreate{Title: "train", Tasks: []MilestoneTaskInput{
		{Key: "T1", Title: "order code"}, {Key: "T2", Title: "session"}, {Key: "T3", Title: "history"}, {Key: "T4", Title: "ui"},
		{Key: "T5", Title: "money path", DependsOn: deps("T1"), DependsWhy: why("T1")},
		{Key: "T6", Title: "isolated margin", DependsOn: deps("T2")}, // chained out of habit: no reason, dropped
	}})
	if err != nil {
		t.Fatal(err)
	}
	if v.MaxParallel != nil {
		t.Fatalf("max_parallel defaulted to %d: the milestone must have no cap of its own", *v.MaxParallel)
	}
	// The task behind a justified edge cannot be started yet; the other five fill the five slots.
	if _, err := m.Enqueue(StartRequest{Title: "t", Kind: "general", Brief: "T5", Model: "sonnet", Milestone: v.ID, Task: "T5"}); err == nil ||
		!strings.Contains(err.Error(), "blocked") {
		t.Fatalf("job on T5 while T1 is not done: %v", err)
	}
	var ids []string
	for _, k := range []string{"T1", "T2", "T3", "T4", "T6"} {
		ids = append(ids, enqueue(t, m, k, func(r *StartRequest) { r.Milestone, r.Task = v.ID, k }).ID)
	}
	if err := m.Tick(); err != nil {
		t.Fatal(err)
	}
	for _, id := range ids {
		if got := get(t, m, id).Status; got != store.StatusRunning {
			t.Errorf("job %s: %s, want running", id, got)
		}
	}
	if n, err := m.Store.CountRunning(); err != nil || n != 5 {
		t.Fatalf("running %d (%v), want 5", n, err)
	}
}

// #119: the idle-slot nudge. Free slots plus ready tasks give one line to the orchestrator; the same
// ready list is not announced twice.
func TestIdleSlotsLine(t *testing.T) {
	ctx := context.Background()
	t.Run("names the free slots and the ready tasks, not the blocked or running ones", func(t *testing.T) {
		m := newManager(t)
		three := 3
		m.Cfg.MaxRunning = &three
		v, err := m.CreateMilestone(ctx, MilestoneCreate{Title: "m", Tasks: []MilestoneTaskInput{
			{Key: "T1", Title: "a"}, {Key: "T2", Title: "b"}, {Key: "T3", Title: "c", DependsOn: deps("T1"), DependsWhy: why("T1")}}})
		if err != nil {
			t.Fatal(err)
		}
		line, _, err := m.IdleSlotsLine(ctx)
		if err != nil || line != "3 slots free, ready: T1, T2 (milestone 1)" {
			t.Fatalf("line %q (%v)", line, err)
		}
		enqueue(t, m, "a", func(r *StartRequest) { r.Milestone, r.Task = v.ID, "T1" })
		if err := m.Tick(); err != nil {
			t.Fatal(err)
		}
		if line, _, _ := m.IdleSlotsLine(ctx); line != "2 slots free, ready: T2 (milestone 1)" {
			t.Fatalf("after T1 started: %q", line)
		}
	})

	t.Run("no line without a free slot, without a ready task, or when the milestone's own cap is reached", func(t *testing.T) {
		m := newManager(t)
		one := 1
		m.Cfg.MaxRunning = &one
		v, err := m.CreateMilestone(ctx, MilestoneCreate{Title: "m", Tasks: []MilestoneTaskInput{{Key: "T1", Title: "a"}, {Key: "T2", Title: "b"}}})
		if err != nil {
			t.Fatal(err)
		}
		enqueue(t, m, "a", func(r *StartRequest) { r.Milestone, r.Task = v.ID, "T1" })
		if err := m.Tick(); err != nil {
			t.Fatal(err)
		}
		if line, _, _ := m.IdleSlotsLine(ctx); line != "" {
			t.Fatalf("no free slot, got %q", line)
		}
		m2 := newManager(t)
		capped := 1
		v2, err := m2.CreateMilestone(ctx, MilestoneCreate{Title: "m", MaxParallel: capped, Tasks: []MilestoneTaskInput{{Key: "T1", Title: "a"}, {Key: "T2", Title: "b"}}})
		if err != nil {
			t.Fatal(err)
		}
		enqueue(t, m2, "a", func(r *StartRequest) { r.Milestone, r.Task = v2.ID, "T1" })
		if err := m2.Tick(); err != nil {
			t.Fatal(err)
		}
		if line, _, _ := m2.IdleSlotsLine(ctx); line != "" {
			t.Fatalf("milestone at max_parallel, got %q", line)
		}
	})

	t.Run("the daemon types the line once per ready list", func(t *testing.T) {
		m := newManager(t)
		logf := filepath.Join(t.TempDir(), "tmux.log")
		t.Setenv("FAKE_TMUX_LOG", logf)
		m.Cfg.NotifyTmuxTarget = "fake-session"
		if _, err := m.CreateMilestone(ctx, MilestoneCreate{Title: "m", Tasks: []MilestoneTaskInput{{Key: "T1", Title: "a"}, {Key: "T2", Title: "b"}}}); err != nil {
			t.Fatal(err)
		}
		for i := 0; i < 3; i++ {
			if err := m.Tick(); err != nil {
				t.Fatal(err)
			}
		}
		b, err := os.ReadFile(logf)
		if err != nil {
			t.Fatal(err)
		}
		want := "send-keys|-t|fake-session|-l|2 slots free, ready: T1, T2 (milestone 1)|\nsend-keys|-t|fake-session|Enter|\n"
		if string(b) != want {
			t.Fatalf("tmux calls:\n%s", b)
		}
	})
}
