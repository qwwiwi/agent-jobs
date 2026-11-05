package store

import (
	"path/filepath"
	"testing"
	"time"
)

// The provider column (#136) and the cost estimate (#127) were added on parallel branches: both round
// trip through Insert/Get, and an estimate never lowers what an openrouter job counts against the
// daily cap (an unknown cost counts the whole budget there, estimate or not: real money).
func TestProviderAndCostEstimateRoundTrip(t *testing.T) {
	st, err := Open(filepath.Join(t.TempDir(), "jobs.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	now := time.Unix(1760000000, 0)
	if err := st.Insert(&Job{ID: "or", Agent: "a", Title: "t", Kind: "fix", Brief: "b", Status: StatusFailed,
		Model: "openrouter:z-ai/glm-5.3", MaxTurns: 10, MaxBudgetUSD: 2, TimeoutMinutes: 5, CreatedAt: now,
		Billing: BillingAPI, Provider: ProviderOpenRouter}); err != nil {
		t.Fatal(err)
	}
	if err := st.SetCostUnknown("or"); err != nil {
		t.Fatal(err)
	}
	if err := st.SetCostEstimate("or", 0.3); err != nil {
		t.Fatal(err)
	}
	got, err := st.Get("or")
	if err != nil {
		t.Fatal(err)
	}
	if got.Provider != ProviderOpenRouter || !got.CostUnknown || got.CostEstimateUSD == nil || *got.CostEstimateUSD != 0.3 {
		t.Fatalf("round trip: provider %q unknown %v estimate %v", got.Provider, got.CostUnknown, got.CostEstimateUSD)
	}
	spent, err := st.ProviderSpendSince("a", ProviderOpenRouter, now.Add(-time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if spent != 2 {
		t.Fatalf("provider spend %v, want the whole $2 budget of the unknown-cost job", spent)
	}
}
