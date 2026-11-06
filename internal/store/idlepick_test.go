package store

import (
	"path/filepath"
	"testing"
	"time"
)

func TestIdlePickEvents(t *testing.T) {
	s := openIdleStore(t)
	now := time.Unix(1_800_000_000, 0)
	if at, err := s.LastIdlePickEvent(IdleNote); err != nil || at != nil {
		t.Fatalf("empty: %v %v", at, err)
	}
	for _, e := range []struct {
		kind string
		at   time.Time
	}{{IdleNote, now}, {IdleEval, now.Add(time.Hour)}, {IdleNote, now.Add(2 * time.Hour)}} {
		if err := s.AddIdlePickEvent(e.kind, e.at); err != nil {
			t.Fatal(err)
		}
	}
	if at, err := s.LastIdlePickEvent(IdleNote); err != nil || at == nil || !at.Equal(now.Add(2*time.Hour)) {
		t.Fatalf("last note: %v %v", at, err)
	}
	if at, err := s.LastIdlePickEvent(IdleNote, IdleEval, IdleRecovery); err != nil || !at.Equal(now.Add(2*time.Hour)) {
		t.Fatalf("last of three kinds: %v %v", at, err)
	}
	if n, err := s.CountIdlePickEvents(now.Add(30*time.Minute), IdleNote); err != nil || n != 1 {
		t.Fatalf("count since: %d %v", n, err)
	}
	// Events older than a week go with the next write.
	if err := s.AddIdlePickEvent(IdleScan, now.Add(8*24*time.Hour)); err != nil {
		t.Fatal(err)
	}
	if n, err := s.CountIdlePickEvents(time.Unix(0, 0), IdleNote, IdleEval); err != nil || n != 0 {
		t.Fatalf("old events kept: %d %v", n, err)
	}
}

func TestCountActiveAndLastFinished(t *testing.T) {
	s := openIdleStore(t)
	if n, err := s.CountActive(); err != nil || n != 0 {
		t.Fatalf("empty: %d %v", n, err)
	}
	if at, err := s.LastFinishedAt(); err != nil || at != nil {
		t.Fatalf("no finished job: %v %v", at, err)
	}
}

func openIdleStore(t *testing.T) *Store {
	t.Helper()
	s, err := Open(filepath.Join(t.TempDir(), "jobs.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}
