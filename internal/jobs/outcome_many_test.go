package jobs

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/qwwiwi/agent-jobs/internal/store"
)

func insertOutcomeJob(t *testing.T, m *Manager, id, agent, status string) {
	t.Helper()
	fin := time.Now().Add(-time.Hour)
	j := &store.Job{ID: id, Agent: agent, Title: "t " + id, Kind: "general", Brief: "b", Status: status,
		Model: "sonnet", CreatedAt: fin}
	if status != store.StatusQueued && status != store.StatusRunning {
		j.FinishedAt = &fin
	}
	if err := m.Store.Insert(j); err != nil {
		t.Fatal(err)
	}
}

// One failing item never aborts the rest: unknown ids, other agents' jobs, unfinished jobs, a bad
// outcome and an over-long note are reported per item; valid items are recorded; a repeat overwrites.
func TestSetOutcomesMixedItems(t *testing.T) {
	m := newManager(t)
	insertOutcomeJob(t, m, "ok1", m.Agent, store.StatusSucceeded)
	insertOutcomeJob(t, m, "ok2", m.Agent, store.StatusFailed)
	insertOutcomeJob(t, m, "run1", m.Agent, store.StatusRunning)
	insertOutcomeJob(t, m, "alien", "someone-else", store.StatusSucceeded)

	res, err := m.SetOutcomes([]OutcomeItem{
		{ID: "ok1", Outcome: OutcomeAccepted, Note: "used as is"},
		{ID: "nope", Outcome: OutcomeAccepted},
		{ID: "alien", Outcome: OutcomeAccepted},
		{ID: "run1", Outcome: OutcomeAccepted},
		{ID: "ok2", Outcome: "great"},
		{ID: "ok2", Outcome: OutcomeDiscarded, Note: strings.Repeat("x", MaxOutcomeNoteRunes+1)},
		{ID: "ok2", Outcome: OutcomeRerun},
	})
	if err != nil {
		t.Fatal(err)
	}
	wantErr := []string{"", "not found", "belongs to agent", "is running", "must be one of", "characters", ""}
	if len(res) != len(wantErr) {
		t.Fatalf("%d results for %d items", len(res), len(wantErr))
	}
	for i, w := range wantErr {
		r := res[i]
		if w == "" && (!r.OK || r.Error != "") || w != "" && (r.OK || !strings.Contains(r.Error, w)) {
			t.Errorf("item %d: %+v, want error containing %q", i, r, w)
		}
	}
	if res[0].Outcome != OutcomeAccepted || res[0].OutcomeNote != "used as is" || res[6].Outcome != OutcomeRerun {
		t.Fatalf("recorded values: %+v %+v", res[0], res[6])
	}
	for id, want := range map[string]string{"ok1": OutcomeAccepted, "ok2": OutcomeRerun, "run1": "", "alien": ""} {
		j, err := m.Store.Get(id)
		if err != nil {
			t.Fatal(err)
		}
		if j.Outcome != want {
			t.Errorf("%s: outcome %q, want %q", id, j.Outcome, want)
		}
	}
	// Idempotent overwrite: the same call again succeeds and leaves the same state; another value replaces it.
	res, err = m.SetOutcomes([]OutcomeItem{{ID: "ok1", Outcome: OutcomeAccepted, Note: "used as is"}, {ID: "ok1", Outcome: OutcomeReworked}})
	if err != nil || !res[0].OK || !res[1].OK {
		t.Fatalf("repeat: %+v %v", res, err)
	}
	if j, _ := m.Store.Get("ok1"); j.Outcome != OutcomeReworked || j.OutcomeNote != "" {
		t.Fatalf("overwrite: %+v", j)
	}
}

// A succeeded job gets no outcome by itself: a batch only records what it names.
func TestSetOutcomesNeverInfersAcceptance(t *testing.T) {
	m := newManager(t)
	insertOutcomeJob(t, m, "a", m.Agent, store.StatusSucceeded)
	insertOutcomeJob(t, m, "b", m.Agent, store.StatusSucceeded)
	if _, err := m.SetOutcomes([]OutcomeItem{{ID: "a", Outcome: OutcomeAccepted}}); err != nil {
		t.Fatal(err)
	}
	if j, _ := m.Store.Get("b"); j.Outcome != "" {
		t.Fatalf("unnamed job got outcome %q", j.Outcome)
	}
}

func TestSetOutcomesBatchBounds(t *testing.T) {
	m := newManager(t)
	if _, err := m.SetOutcomes(nil); err == nil || !strings.Contains(err.Error(), "at least one") {
		t.Fatalf("empty batch: %v", err)
	}
	items := make([]OutcomeItem, MaxOutcomeBatch+1)
	for i := range items {
		items[i] = OutcomeItem{ID: fmt.Sprintf("j%d", i), Outcome: OutcomeAccepted}
	}
	if _, err := m.SetOutcomes(items); err == nil || !strings.Contains(err.Error(), "at most") {
		t.Fatalf("oversized batch: %v", err)
	}
}

// An item that was committed is acknowledged with the values it wrote, whatever happens to the row
// afterwards: a concurrent overwrite, a cleared outcome or a failing read must neither substitute another
// outcome, nor empty it, nor turn a recorded item into a failure.
func TestSetOutcomesAcknowledgesCommittedValues(t *testing.T) {
	for name, after := range map[string]func(*testing.T, *Manager, string){
		"overwritten": func(t *testing.T, m *Manager, id string) {
			if err := m.Store.SetOutcome(id, m.Agent, OutcomeDiscarded, "other request", time.Now()); err != nil {
				t.Fatal(err)
			}
		},
		"cleared": func(t *testing.T, m *Manager, id string) {
			if err := m.Store.SetOutcome(id, m.Agent, "", "", time.Now()); err != nil {
				t.Fatal(err)
			}
		},
		"read fails": func(t *testing.T, m *Manager, id string) {
			if err := m.Store.Close(); err != nil { // every later read of the store fails
				t.Fatal(err)
			}
		},
	} {
		t.Run(name, func(t *testing.T) {
			m := newManager(t)
			insertOutcomeJob(t, m, "j1", m.Agent, store.StatusSucceeded)
			m.afterOutcome = func(id string) { after(t, m, id) }
			res, err := m.SetOutcomes([]OutcomeItem{{ID: " j1 ", Outcome: " " + OutcomeAccepted + " ", Note: " shipped "}})
			if err != nil {
				t.Fatal(err)
			}
			if r := res[0]; !r.OK || r.Error != "" || r.ID != "j1" || r.Outcome != OutcomeAccepted || r.OutcomeNote != "shipped" {
				t.Fatalf("result %+v, want ok with accepted/shipped", r)
			}
		})
	}
}

// An incomplete item (no id, no outcome) fails on its own and never stops the others.
func TestSetOutcomesIncompleteItems(t *testing.T) {
	m := newManager(t)
	insertOutcomeJob(t, m, "ok1", m.Agent, store.StatusSucceeded)
	res, err := m.SetOutcomes([]OutcomeItem{{ID: "ok1", Outcome: OutcomeAccepted}, {ID: "ok1"}, {Outcome: OutcomeAccepted}})
	if err != nil {
		t.Fatal(err)
	}
	if !res[0].OK || res[1].OK || !strings.Contains(res[1].Error, "outcome") || res[2].OK || !strings.Contains(res[2].Error, "id is required") {
		t.Fatalf("results: %+v", res)
	}
}
