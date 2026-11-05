package store

import (
	"path/filepath"
	"testing"
	"time"
)

// B5 (#136 R3 review): an attempt's terminal status, its cost and whether that cost is unknown are one
// write. Written apart, a failed openrouter attempt was briefly a known $0 (cost_unknown still 0), and
// a concurrent job_start passed the daily cap in that gap.
func TestFinishAttemptCostUnknownIsAtomic(t *testing.T) {
	st, err := Open(filepath.Join(t.TempDir(), "jobs.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	now := time.Unix(1760000000, 0)
	for _, id := range []string{"cut", "ok"} {
		if err := st.Insert(&Job{ID: id, Agent: "a", Title: "t", Kind: "fix", Brief: "b", Status: StatusRunning,
			LaunchToken: "tok-" + id, Model: "openrouter:z-ai/glm-5.3", MaxTurns: 10, MaxBudgetUSD: 2, TimeoutMinutes: 5,
			CreatedAt: now, Billing: BillingAPI, Provider: ProviderOpenRouter}); err != nil {
			t.Fatal(err)
		}
	}
	est := 0.25
	if err := st.FinishAttempt("cut", "tok-cut", StatusFailed, 1, 0, true, &est, 3, "stream cut", now); err != nil {
		t.Fatal(err)
	}
	// No second write: right after the one call the cap counts the whole budget.
	spent, err := st.ProviderSpendSince("a", ProviderOpenRouter, now.Add(-time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if spent != 2+2 { // "ok" is still running: its whole budget too
		t.Fatalf("provider spend %v right after the finish, want 4", spent)
	}
	got, err := st.Get("cut")
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != StatusFailed || !got.CostUnknown || got.CostEstimateUSD == nil || *got.CostEstimateUSD != 0.25 {
		t.Fatalf("row: status %s unknown %v estimate %v", got.Status, got.CostUnknown, got.CostEstimateUSD)
	}
	// A known cost leaves the flags as they are, and a wrong launch token changes nothing.
	if err := st.FinishAttempt("ok", "tok-other", StatusSucceeded, 1, 0.5, false, nil, 3, "", now); err == nil {
		t.Fatal("finish with another launch's token")
	}
	if err := st.FinishAttempt("ok", "tok-ok", StatusSucceeded, 1, 0.5, false, nil, 3, "", now); err != nil {
		t.Fatal(err)
	}
	if got, _ := st.Get("ok"); got.CostUnknown || got.CostEstimateUSD != nil || got.CostUSD != 0.5 {
		t.Fatalf("known cost row: unknown %v estimate %v cost %v", got.CostUnknown, got.CostEstimateUSD, got.CostUSD)
	}
	if spent, _ := st.ProviderSpendSince("a", ProviderOpenRouter, now.Add(-time.Hour)); spent != 2.5 {
		t.Fatalf("provider spend %v, want 2.5", spent)
	}
}
