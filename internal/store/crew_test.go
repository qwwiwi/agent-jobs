package store

import (
	"errors"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

// The crew column round-trips, and concurrent claims never pass a cap: of many claims at once for a
// job with two calls, exactly one wins (one at a time), and after two finished calls none does.
func TestCrewRoundTripAndConcurrentClaims(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "jobs.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	now := time.Unix(1_800_000_000, 0)
	j := &Job{ID: "261106-120000-crew", Agent: "a", Title: "t", Kind: "general", Brief: "b", Status: StatusQueued,
		Model: "sonnet", CreatedAt: now, Crew: &Crew{AdvisorModel: "opus", AdvisorMaxCalls: 2, AdvisorMaxBudgetUSD: 1, PairedWith: "x"}}
	if err := s.Insert(j); err != nil {
		t.Fatal(err)
	}
	plain := &Job{ID: "261106-120000-plan", Agent: "a", Title: "t", Kind: "general", Brief: "b", Status: StatusQueued,
		Model: "sonnet", CreatedAt: now}
	if err := s.Insert(plain); err != nil {
		t.Fatal(err)
	}
	got, err := s.Get(j.ID)
	if err != nil || got.Crew == nil || *got.Crew != *j.Crew {
		t.Fatalf("crew round trip: %+v %v", got, err)
	}
	if p, err := s.Get(plain.ID); err != nil || p.Crew != nil {
		t.Fatalf("plain job crew: %+v %v", p, err)
	}
	if cj, err := s.CrewJobs(t.Context()); err != nil || len(cj) != 1 || cj[0].ID != j.ID {
		t.Fatalf("crew jobs: %v %v", cj, err)
	}
	claim := AdvisorClaim{Model: "opus", MaxCalls: 2, MaxBudget: 1, StaleAfter: time.Hour}
	if _, err := s.ClaimAdvisorCall(j.ID, now, claim); err == nil {
		t.Fatal("claim for a queued job")
	}
	if err := s.Claim(j.ID, "tok", now); err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	var mu sync.Mutex
	won, limited := 0, 0
	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := s.ClaimAdvisorCall(j.ID, now, claim)
			mu.Lock()
			defer mu.Unlock()
			switch {
			case err == nil:
				won++
			case errors.Is(err, ErrAdvisorLimit):
				limited++
			default:
				t.Errorf("claim: %v", err)
			}
		}()
	}
	wg.Wait()
	if won != 1 || limited != 7 {
		t.Fatalf("concurrent claims: %d won, %d limited", won, limited)
	}
	end := now.Add(time.Minute)
	finish := func(n int) {
		if err := s.FinishAdvisorCall(&AdvisorCall{JobID: j.ID, N: n, EndedAt: &end, Outcome: AdvisorAnswered, CostUSD: 0.1}); err != nil {
			t.Fatal(err)
		}
	}
	finish(1)
	if _, err := s.ClaimAdvisorCall(j.ID, end, claim); err != nil {
		t.Fatalf("second call: %v", err)
	}
	finish(2)
	if _, err := s.ClaimAdvisorCall(j.ID, end, claim); !errors.Is(err, ErrAdvisorLimit) {
		t.Fatalf("third call: %v", err)
	}
	calls, err := s.AdvisorCalls(j.ID, end, time.Hour)
	if err != nil || len(calls) != 2 || AdvisorSpend(calls) < 0.199 || AdvisorSpend(calls) > 0.201 {
		t.Fatalf("calls: %+v %v", calls, err)
	}
}
