package store

import (
	"errors"
	"path/filepath"
	"testing"
	"time"
)

// The current attempt's gate lock wait moves the job's timeout (#208): ClaimTimeout holds only past
// started_at + timeout + gate_wait_sec. RaiseGateWait is bound to the running launch's token, keeps the
// larger value, and a new launch starts again from 0.
func TestGateWaitMovesTimeout(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "jobs.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	now := time.Unix(1770000000, 0)
	if err := s.Insert(&Job{ID: "a", Agent: "x", Title: "t", Kind: "fix", Status: StatusQueued, CreatedAt: now, TimeoutMinutes: 10}); err != nil {
		t.Fatal(err)
	}
	if err := s.Claim("a", "tok", now); err != nil {
		t.Fatal(err)
	}
	if err := s.RaiseGateWait("a", "other", 600); !errors.Is(err, ErrImmutable) && !errors.Is(err, ErrNotFound) {
		t.Fatalf("a foreign token must not move the timeout: %v", err)
	}
	if err := s.RaiseGateWait("a", "tok", 300); err != nil {
		t.Fatal(err)
	}
	if err := s.RaiseGateWait("a", "tok", 120); err != nil {
		t.Fatal(err)
	}
	if j, err := s.Get("a"); err != nil || j.GateWaitSec != 300 {
		t.Fatalf("gate wait %d %v, want the larger 300", j.GateWaitSec, err)
	}
	if ok, err := s.ClaimTimeout("a", "tok", now.Add(14*time.Minute)); err != nil || ok {
		t.Fatalf("timed out at 14 min with 5 min of gate lock wait credited: %v %v", ok, err)
	}
	if ok, err := s.ClaimTimeout("a", "tok", now.Add(15*time.Minute+time.Second)); err != nil || !ok {
		t.Fatalf("not timed out past timeout + gate wait: %v %v", ok, err)
	}
}

// A new launch (resume) starts its credit from 0.
func TestGateWaitResetOnLaunch(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "jobs.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	now := time.Unix(1770000000, 0)
	if err := s.Insert(&Job{ID: "a", Agent: "x", Title: "t", Kind: "fix", Status: StatusQueued, CreatedAt: now, GateWaitSec: 99}); err != nil {
		t.Fatal(err)
	}
	if err := s.Claim("a", "tok", now); err != nil {
		t.Fatal(err)
	}
	if j, err := s.Get("a"); err != nil || j.GateWaitSec != 0 {
		t.Fatalf("gate wait after launch %d %v", j.GateWaitSec, err)
	}
}
