package jobs

import (
	"math"
	"strings"
	"testing"
	"time"

	"github.com/qwwiwi/agent-jobs/internal/store"
)

// job_stop before claude reported the cost (#127): the attempt's cost is estimated from its trusted
// token usage at list prices (the stats estimator) and recorded as an estimate; the resume accounting
// counts the estimate, not the whole budget, so a job started at the $50 ceiling and stopped early can
// still be resumed without new limits.
func TestStopRecordsCostEstimate(t *testing.T) {
	m := newManager(t)
	sessionsHome(t, true)
	j := enqueue(t, m, "FAKE_WIP FAKE_EARLY_USAGE FAKE_SLEEP_ONCE=30", func(r *StartRequest) { r.MaxBudgetUSD = CeilingMaxBudgetUSD })
	tick(t, m)
	waitFor(t, "persisted usage", 15*time.Second, func() bool {
		return m.endedResult(get(t, m, j.ID)).Tokens != (TokenUsage{})
	})
	tokens := m.endedResult(get(t, m, j.ID)).Tokens
	st, err := m.Stop(j.ID, "plan changed")
	if err != nil {
		t.Fatal(err)
	}
	p, ok := m.Config().PriceFor("sonnet")
	if !ok {
		t.Fatal("no list price for sonnet")
	}
	want := p.Cost(tokens)
	if !st.CostUnknown || st.CostEstimateUSD == nil || math.Abs(*st.CostEstimateUSD-want) > 1e-6 || want <= 0 || want >= 1 {
		t.Fatalf("stopped row: unknown %v estimate %v, want %v", st.CostUnknown, st.CostEstimateUSD, want)
	}
	note, err := m.ResumeBudget(st)
	if err != nil || strings.HasPrefix(note, "resume impossible") || !strings.Contains(note, "counts at its estimate "+EstimatedCostText(want)) {
		t.Fatalf("resume budget: %q %v", note, err)
	}
	r, err := m.Resume(j.ID, "continue", Limits{})
	if err != nil {
		t.Fatalf("resume refused: %v", err)
	}
	if r.MaxBudgetUSD != cents(CeilingMaxBudgetUSD-want) || r.CostEstimateUSD != nil {
		t.Fatalf("resumed row: budget %v estimate %v", r.MaxBudgetUSD, r.CostEstimateUSD)
	}
	atts, err := m.Store.Attempts(j.ID)
	if err != nil || len(atts) != 1 || !atts[0].CostUnknown || atts[0].CostEstimateUSD == nil || *atts[0].CostEstimateUSD != *st.CostEstimateUSD {
		t.Fatalf("archived attempt: %+v %v", atts, err)
	}
	if got := runToEnd(t, m, j.ID); got.Status != store.StatusSucceeded || got.Attempt != 2 {
		t.Fatalf("resumed: %s attempt %d %q", got.Status, got.Attempt, got.Error)
	}
}

// The estimate counts at most the attempt's budget; without one the whole budget counts, as before.
func TestChargedCostEstimate(t *testing.T) {
	est, big := 0.42, 80.0
	for _, c := range []struct {
		cost     float64
		unknown  bool
		estimate *float64
		want     float64
	}{
		{1.5, false, nil, 1.5},
		{0, true, nil, 10},
		{0, true, &est, 0.42},
		{0.5, true, &est, 0.5},
		{0, true, &big, 10},
	} {
		if got := chargedCost(c.cost, c.unknown, c.estimate, 10); got != c.want {
			t.Errorf("chargedCost(%v, %v, %v, 10) = %v, want %v", c.cost, c.unknown, c.estimate, got, c.want)
		}
	}
}
