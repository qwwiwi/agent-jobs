package store

import (
	"errors"
	"path/filepath"
	"testing"
	"time"
)

func TestTrustedLaunchStateCannotCrossAttempts(t *testing.T) {
	path := filepath.Join(t.TempDir(), "jobs.db")
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	now := time.Now()
	j := &Job{ID: "trusted", Agent: "agent1", Title: "test", Kind: "fix", Brief: "b", Model: "sonnet", Status: StatusQueued, CreatedAt: now}
	if err := s.Insert(j); err != nil {
		t.Fatal(err)
	}
	if err := s.Claim(j.ID, "first", now); err != nil {
		t.Fatal(err)
	}
	if err := s.SetSession(j.ID, "first", "original", RunResume); err != nil {
		t.Fatal(err)
	}
	if err := s.SetFallbackSession(j.ID, "first", "daemon-fallback"); err != nil {
		t.Fatal(err)
	}
	if err := s.SetWrapperResult(j.ID, "first", `{"Found":true}`); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	s, err = Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if got, err := s.FallbackSession(j.ID, "first"); err != nil || got != "daemon-fallback" {
		t.Fatalf("persisted fallback: %q %v", got, err)
	}
	if raw, ok, err := s.WrapperResult(j.ID, "first"); err != nil || !ok || raw != `{"Found":true}` {
		t.Fatalf("persisted result: %q %v %v", raw, ok, err)
	}
	if err := s.SetWrapperResult(j.ID, "wrong", "forged"); !errors.Is(err, ErrImmutable) {
		t.Fatalf("wrong token: %v", err)
	}
	if err := s.Finish(j.ID, "first", StatusFailed, 1, 0, 1, "", now); err != nil {
		t.Fatal(err)
	}
	if err := s.SetWrapperResult(j.ID, "first", "late"); !errors.Is(err, ErrImmutable) {
		t.Fatalf("late result: %v", err)
	}
	if err := s.Requeue(j.ID, ResumeLimits{MaxTurns: 10, MaxBudgetUSD: 1, TimeoutMinutes: 5}, "continue", ""); err != nil {
		t.Fatal(err)
	}
	if raw, err := s.AttemptWrapperResult(j.ID, 1); err != nil || raw != `{"Found":true}` {
		t.Fatalf("trusted attempt snapshot lost: %q %v", raw, err)
	}
	if err := s.Claim(j.ID, "second", now); err != nil {
		t.Fatal(err)
	}
	if got, err := s.FallbackSession(j.ID, "second"); err != nil || got != "" {
		t.Fatalf("previous launch fallback survived claim: %q %v", got, err)
	}
	for _, token := range []string{"first", "second", ""} {
		if _, ok, err := s.WrapperResult(j.ID, token); err != nil || ok {
			t.Fatalf("stale result for %q: %v %v", token, ok, err)
		}
	}
	if _, err := s.FallbackSession(j.ID, "first"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("stale fallback: %v", err)
	}
	if err := s.SetSession(j.ID, "second", "original", RunResume); err != nil {
		t.Fatal(err)
	}
	if got, err := s.FallbackSession(j.ID, "second"); err != nil || got != "" {
		t.Fatalf("fallback not cleared: %q %v", got, err)
	}
	if err := s.SetFallbackSession(j.ID, "second", "fresh"); err != nil {
		t.Fatal(err)
	}
	if err := s.SetSession(j.ID, "second", "fresh", RunFallback); err != nil {
		t.Fatal(err)
	}
	if err := s.SetFallbackSession(j.ID, "second", "planted"); !errors.Is(err, ErrImmutable) {
		t.Fatalf("fallback outside resume: %v", err)
	}
}
