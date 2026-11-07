package jobs

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/qwwiwi/agent-jobs/internal/store"
)

func accept(t *testing.T, m *Manager, id string) {
	t.Helper()
	if err := m.Store.Claim(id, "acc-"+id, m.Now()); err != nil {
		t.Fatal(err)
	}
	if err := m.Store.Finish(id, "acc-"+id, store.StatusSucceeded, 0, 0, 0, "", m.Now()); err != nil {
		t.Fatal(err)
	}
	if err := m.SetOutcome(id, OutcomeAccepted, ""); err != nil {
		t.Fatal(err)
	}
}

// A milestone edit that adds a task dependency must not close a cycle with the serialized waits of
// jobs that are already queued.
func TestMilestoneEditRefusesDependencyCycle(t *testing.T) {
	ctx := context.Background()
	m, repo := newManager(t), newRepo(t)
	v, err := m.CreateMilestone(ctx, MilestoneCreate{Title: "edit", Repo: repo,
		Tasks: []MilestoneTaskInput{{Key: "T1", Title: "first"}, {Key: "T2", Title: "second"}}})
	if err != nil {
		t.Fatal(err)
	}
	onTask := func(key string, paths []string, policy string) StartRequest {
		r := overlapRequest(repo, paths, policy)
		r.Milestone, r.Task = v.ID, key
		return r
	}
	a, err := m.Enqueue(onTask("T2", []string{"src/**"}, ""))
	if err != nil {
		t.Fatal(err)
	}
	b, err := m.Enqueue(onTask("T1", []string{"src/b.go"}, "serialize"))
	if err != nil {
		t.Fatal(err)
	}
	if got := get(t, m, b.ID); len(got.WaitFor) != 1 || got.WaitFor[0] != a.ID {
		t.Fatalf("fixture: B does not wait for A: %v", got.WaitFor)
	}
	_, err = m.SetMilestone(ctx, MilestoneSet{ID: v.ID, Tasks: []MilestoneTaskInput{
		{Key: "T2", Title: "second", DependsOn: deps("T1"), DependsWhy: why("T1")}}})
	if err == nil || !strings.Contains(err.Error(), "cycle") || !strings.Contains(err.Error(), a.ID) || !strings.Contains(err.Error(), b.ID) {
		t.Fatalf("edit closing a cycle accepted: %v", err)
	}
	tree, err := m.MilestoneTree(ctx, v.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(taskOf(t, tree, "T2").DependsOn) != 0 {
		t.Fatalf("refused edit was stored: %+v", taskOf(t, tree, "T2"))
	}
	// An edit that adds no cycle still goes through.
	if _, err := m.SetMilestone(ctx, MilestoneSet{ID: v.ID, Goal: new(string)}); err != nil {
		t.Fatalf("harmless edit refused: %v", err)
	}
}

// The cycle D>A>B>C>E>F>D crosses three repositories and three milestones; D's admission must see all
// of them, not only the milestones of the jobs in its own repository.
func TestOverlapRefusesCrossRepositoryCycle(t *testing.T) {
	ctx := context.Background()
	m := newManager(t)
	repos := []string{newRepo(t), newRepo(t), newRepo(t)}
	ms := make([]int64, 3)
	for i := range ms {
		v, err := m.CreateMilestone(ctx, MilestoneCreate{Title: "ring",
			Tasks: []MilestoneTaskInput{{Key: "T1", Title: "first"}, {Key: "T2", Title: "second", DependsOn: deps("T1"), DependsWhy: why("T1")}}})
		if err != nil {
			t.Fatal(err)
		}
		ms[i] = v.ID
	}
	start := func(repo int, milestone int, task, policy string) (*store.Job, error) {
		r := overlapRequest(repos[repo], []string{"src/**"}, policy)
		r.Milestone, r.Task = ms[milestone], task
		return m.Enqueue(r)
	}
	// T1 of every milestone is accepted first, then the T2 jobs A, C, F wait on nothing.
	for i := range ms {
		j, err := start(i, i, "T1", "")
		if err != nil {
			t.Fatal(err)
		}
		accept(t, m, j.ID)
	}
	for _, s := range [][2]int{{0, 1}, {1, 2}, {2, 0}} { // A: R1 M2, C: R2 M3, F: R3 M1
		if _, err := start(s[0], s[1], "T2", ""); err != nil {
			t.Fatal(err)
		}
	}
	// B: R2 M2/T1 waits C; E: R3 M3/T1 waits F.
	for _, s := range [][2]int{{1, 1}, {2, 2}} {
		if _, err := start(s[0], s[1], "T1", "serialize"); err != nil {
			t.Fatalf("setup start refused: %v", err)
		}
	}
	// D: R1 M1/T1 waits A and closes D>A>B>C>E>F>D.
	_, err := start(0, 0, "T1", "serialize")
	if err == nil || !strings.Contains(err.Error(), "cycle") {
		t.Fatalf("cross-repository cycle admitted: %v", err)
	}
}

// B waits for A; A fails and B starts; C (serialized, overlapping) is admitted while A is failed and
// records only B. A resumed must not run next to C once B is done, whatever on_overlap policy A itself
// used (C asked for serialization) and whichever of A and C is tried first.
func TestSerializeResumeRenewsOverlapAdmission(t *testing.T) {
	for _, policy := range []string{"", "warn", "refuse", "serialize"} {
		for _, order := range []string{"A tried first", "C claimed first"} {
			t.Run("policy="+policy+"/"+order, func(t *testing.T) {
				serializeResumeScenario(t, policy, order == "A tried first")
			})
		}
	}
}

func serializeResumeScenario(t *testing.T, policy string, aTriedFirst bool) {
	m, repo := newManager(t), newRepo(t)
	a, err := m.Enqueue(overlapRequest(repo, []string{"src/**"}, policy))
	if err != nil {
		t.Fatal(err)
	}
	b, err := m.Enqueue(overlapRequest(repo, []string{"src/b.go"}, "serialize"))
	if err != nil {
		t.Fatal(err)
	}
	if got := get(t, m, b.ID); len(got.WaitFor) != 1 || got.WaitFor[0] != a.ID {
		t.Fatalf("fixture: B should record A: %v", got.WaitFor)
	}
	if err := m.Store.Claim(a.ID, "a1", m.Now()); err != nil {
		t.Fatal(err)
	}
	if err := m.Store.Finish(a.ID, "a1", store.StatusFailed, 0, 0, 0, "boom", m.Now()); err != nil {
		t.Fatal(err)
	}
	if err := m.claim(b, "b1"); err != nil {
		t.Fatal(err)
	}
	c, err := m.Enqueue(overlapRequest(repo, []string{"src/b.go"}, "serialize"))
	if err != nil {
		t.Fatal(err)
	}
	if got := get(t, m, c.ID); len(got.WaitFor) != 1 || got.WaitFor[0] != b.ID {
		t.Fatalf("fixture: C should record only B: %v", got.WaitFor)
	}
	limits := store.ResumeLimits{MaxTurns: 1, MaxBudgetUSD: 1, TimeoutMinutes: 1}
	if err := m.requeue(a.ID, limits, "", ""); err != nil {
		t.Fatal(err)
	}
	if got := get(t, m, a.ID); len(got.WaitFor) != 1 || got.WaitFor[0] != c.ID {
		t.Fatalf("resumed A must wait for C and not reciprocally for B: %v", got.WaitFor)
	}
	if err := m.Store.Finish(b.ID, "b1", store.StatusSucceeded, 0, 0, 0, "", m.Now()); err != nil {
		t.Fatal(err)
	}
	if aTriedFirst {
		if err := m.claim(a, "a2"); !errors.Is(err, store.ErrImmutable) {
			t.Fatalf("resumed A claimed while C is queued: %v", err)
		}
	}
	if err := m.claim(c, "c1"); err != nil {
		t.Fatalf("C did not start after B: %v", err)
	}
	if err := m.claim(a, "a3"); !errors.Is(err, store.ErrImmutable) {
		t.Fatalf("resumed A claimed next to running C: %v", err)
	}
	if err := m.Store.Finish(c.ID, "c1", store.StatusSucceeded, 0, 0, 0, "", m.Now()); err != nil {
		t.Fatal(err)
	}
	if err := m.claim(a, "a4"); err != nil {
		t.Fatalf("resumed A did not start after C: %v", err)
	}
}

// Resuming the successor later must not wait for a job that now waits (through C) for it.
func TestSerializeResumeDropsWaitThatClosesCycle(t *testing.T) {
	m, repo := newManager(t), newRepo(t)
	a, _ := m.Enqueue(overlapRequest(repo, []string{"src/**"}, "serialize"))
	b, _ := m.Enqueue(overlapRequest(repo, []string{"src/b.go"}, "serialize"))
	if err := m.Store.Claim(a.ID, "a1", m.Now()); err != nil {
		t.Fatal(err)
	}
	if err := m.Store.Finish(a.ID, "a1", store.StatusFailed, 0, 0, 0, "boom", m.Now()); err != nil {
		t.Fatal(err)
	}
	if err := m.claim(b, "b1"); err != nil {
		t.Fatal(err)
	}
	c, err := m.Enqueue(overlapRequest(repo, []string{"src/b.go"}, "serialize"))
	if err != nil {
		t.Fatal(err)
	}
	limits := store.ResumeLimits{MaxTurns: 1, MaxBudgetUSD: 1, TimeoutMinutes: 1}
	if err := m.requeue(a.ID, limits, "", ""); err != nil { // A now waits for C, C for B
		t.Fatal(err)
	}
	if err := m.Store.Finish(b.ID, "b1", store.StatusFailed, 0, 0, 0, "boom", m.Now()); err != nil {
		t.Fatal(err)
	}
	if err := m.requeue(b.ID, limits, "", ""); err != nil { // B>A>C>B would never start
		t.Fatal(err)
	}
	if got := get(t, m, b.ID); len(got.WaitFor) != 0 {
		t.Fatalf("resumed B keeps a wait that closes B>A>C>B: %v", got.WaitFor)
	}
	if err := m.claim(b, "b2"); err != nil {
		t.Fatalf("resumed B cannot start: %v", err)
	}
	_ = c
}
