package store

import (
	"errors"
	"path/filepath"
	"testing"
	"time"
)

// A claim is refused while another decision is pending or within the minimum gap; a stale pending
// claim is abandoned and then never finished; the gate row keeps only the last reason; the table
// survives a reopen (additive schema).
func TestHeartbeatClaimFinishAbandon(t *testing.T) {
	path := filepath.Join(t.TempDir(), "jobs.db")
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Unix(1770000000, 0)
	h, err := s.ClaimHeartbeat("daemon", now, time.Hour)
	if err != nil || h.Decision != HeartbeatPending {
		t.Fatalf("claim: %+v %v", h, err)
	}
	if _, err := s.ClaimHeartbeat("run-now", now, 0); !errors.Is(err, ErrHeartbeatBusy) {
		t.Fatalf("second claim while pending: %v", err)
	}
	if err := s.FinishHeartbeat(h.ID, HeartbeatWake, "go", 0.01, "", now); err != nil {
		t.Fatal(err)
	}
	if _, err := s.ClaimHeartbeat("daemon", now.Add(30*time.Minute), time.Hour); !errors.Is(err, ErrHeartbeatBusy) {
		t.Fatalf("claim within the gap: %v", err)
	}
	if err := s.SetHeartbeatDelivery(h.ID, false, "tmux down"); err != nil {
		t.Fatal(err)
	}
	h2, err := s.ClaimHeartbeat("daemon", now.Add(2*time.Hour), time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if n, err := s.AbandonHeartbeats(now.Add(3*time.Hour), now.Add(3*time.Hour)); err != nil || n != 1 {
		t.Fatalf("abandon: %d %v", n, err)
	}
	if err := s.FinishHeartbeat(h2.ID, HeartbeatWake, "late", 0, "", now); err == nil {
		t.Fatal("an abandoned decision was finished")
	}
	if err := s.SetHeartbeatGate("night", now); err != nil {
		t.Fatal(err)
	}
	if err := s.SetHeartbeatGate("eligible", now.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	if s, err = Open(path); err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	got, err := s.GetHeartbeat(h.ID)
	if err != nil || got.Decision != HeartbeatWake || got.Delivered || got.Error != "tmux down" || got.CostUSD != 0.01 {
		t.Fatalf("wake row: %+v %v", got, err)
	}
	if got, _ := s.GetHeartbeat(h2.ID); got.Decision != HeartbeatAbandoned || got.FinishedAt == nil {
		t.Fatalf("abandoned row: %+v", got)
	}
	if n, err := s.CountHeartbeatWakes(now); err != nil || n != 1 {
		t.Fatalf("wakes: %d %v", n, err)
	}
	if w, _ := s.LastHeartbeatWake(); w == nil || w.ID != h.ID {
		t.Fatalf("last wake: %+v", w)
	}
	if r, at, err := s.HeartbeatGate(); err != nil || r != "eligible" || !at.Equal(now.Add(time.Minute)) {
		t.Fatalf("gate: %q %v %v", r, at, err)
	}
}
