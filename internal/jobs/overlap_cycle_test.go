package jobs

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/qwwiwi/agent-jobs/internal/store"
)

// cycleFixture is the review scenario: T2 depends on T1, T1 is accepted, and a queued job A sits on T2.
func cycleFixture(t *testing.T) (m *Manager, repo string, milestone int64, a *store.Job) {
	t.Helper()
	m, repo = newManager(t), newRepo(t)
	v, err := m.CreateMilestone(context.Background(), MilestoneCreate{Title: "chain", Repo: repo,
		Tasks: []MilestoneTaskInput{{Key: "T1", Title: "first"}, {Key: "T2", Title: "second", DependsOn: deps("T1"), DependsWhy: why("T1")}}})
	if err != nil {
		t.Fatal(err)
	}
	onTask := func(key string, paths []string, policy string) StartRequest {
		r := overlapRequest(repo, paths, policy)
		r.Milestone, r.Task = v.ID, key
		return r
	}
	first, err := m.Enqueue(onTask("T1", []string{"docs/**"}, ""))
	if err != nil {
		t.Fatal(err)
	}
	if err := m.Store.Claim(first.ID, "t1", m.Now()); err != nil {
		t.Fatal(err)
	}
	if err := m.Store.Finish(first.ID, "t1", store.StatusSucceeded, 0, 0, 0, "", m.Now()); err != nil {
		t.Fatal(err)
	}
	if err := m.SetOutcome(first.ID, OutcomeAccepted, ""); err != nil {
		t.Fatal(err)
	}
	a, err = m.Enqueue(onTask("T2", []string{"src/**"}, ""))
	if err != nil {
		t.Fatal(err)
	}
	return m, repo, v.ID, a
}

func TestOverlapRefusesMilestoneSerializeCycle(t *testing.T) {
	m, repo, ms, a := cycleFixture(t)
	r := overlapRequest(repo, []string{"src/new.go"}, "serialize")
	r.Milestone, r.Task = ms, "T1"
	_, err := m.Enqueue(r)
	if err == nil || !strings.Contains(err.Error(), "cycle") || !strings.Contains(err.Error(), a.ID) {
		t.Fatalf("serialize cycle through a milestone dependency admitted: %v", err)
	}
	// Disjoint paths wait for nobody: the same task start stays admitted.
	r.Paths = []string{"docs/new.md"}
	if _, err := m.Enqueue(r); err != nil {
		t.Fatalf("disjoint start refused: %v", err)
	}
}

func TestOverlapRefusesMilestoneParentCycle(t *testing.T) {
	m, repo, ms, a := cycleFixture(t)
	r := overlapRequest(repo, []string{"docs/new.md"}, "")
	r.Milestone, r.Task, r.Parent = ms, "T1", a.ID
	_, err := m.Enqueue(r)
	if err == nil || !strings.Contains(err.Error(), "cycle") || !strings.Contains(err.Error(), a.ID) {
		t.Fatalf("parent cycle through a milestone dependency admitted: %v", err)
	}
	r.Parent, r.Task = "", "T1"
	if _, err := m.Enqueue(r); err != nil {
		t.Fatalf("plain start refused: %v", err)
	}
}

// A resumed blocker must not run next to the serialized successor that already started.
func TestSerializeResumedBlockerWaitsForRunningSuccessor(t *testing.T) {
	m, repo := newManager(t), newRepo(t)
	a, err := m.Enqueue(overlapRequest(repo, []string{"src/**"}, "serialize"))
	if err != nil {
		t.Fatal(err)
	}
	b, err := m.Enqueue(overlapRequest(repo, []string{"src/b.go"}, "serialize"))
	if err != nil {
		t.Fatal(err)
	}
	if err := m.Store.Claim(a.ID, "a1", m.Now()); err != nil {
		t.Fatal(err)
	}
	if err := m.Store.Finish(a.ID, "a1", store.StatusFailed, 0, 0, 0, "boom", m.Now()); err != nil {
		t.Fatal(err)
	}
	if err := m.claim(b, "b1"); err != nil {
		t.Fatalf("successor did not start after the blocker failed: %v", err)
	}
	limits := store.ResumeLimits{MaxTurns: 1, MaxBudgetUSD: 1, TimeoutMinutes: 1}
	if err := m.Store.Requeue(a.ID, limits, "", ""); err != nil {
		t.Fatal(err)
	}
	if err := m.claim(a, "a2"); !errors.Is(err, store.ErrImmutable) {
		t.Fatalf("resumed blocker claimed next to its running successor: %v", err)
	}
	if wait, _, err := m.jobDependencies(get(t, m, a.ID), false); err != nil || !strings.Contains(wait, b.ID) {
		t.Fatalf("resumed blocker does not report the successor: %q %v", wait, err)
	}
	if err := m.Store.Finish(b.ID, "b1", store.StatusSucceeded, 0, 0, 0, "", m.Now()); err != nil {
		t.Fatal(err)
	}
	if err := m.claim(a, "a3"); err != nil {
		t.Fatalf("resumed blocker did not start once the successor ended: %v", err)
	}
}
