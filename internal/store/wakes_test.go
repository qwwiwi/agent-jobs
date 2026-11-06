package store

import (
	"path/filepath"
	"testing"
	"time"
)

func TestWakeRefDedupe(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "jobs.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	now := time.Unix(1_800_000_000, 0)
	w := Wake{At: now, Kind: WakeDelivery, Category: "job-done", Bytes: 10, Ref: "job-done:a"}
	for i, want := range []bool{true, false} {
		if got, err := s.AddWake(w); err != nil || got != want {
			t.Fatalf("add %d: %v %v", i, got, err)
		}
	}
	// No ref: every record counts; another category or kind with the same ref is its own delivery.
	for i := 0; i < 2; i++ {
		if got, err := s.AddWake(Wake{At: now, Kind: WakeInjection, Category: "hook:stop", Bytes: 5}); err != nil || !got {
			t.Fatalf("injection %d: %v %v", i, got, err)
		}
	}
	other := w
	other.Category = "schedule"
	if got, err := s.AddWake(other); err != nil || !got {
		t.Fatalf("other category: %v %v", got, err)
	}
	ws, err := s.Wakes(now)
	if err != nil || len(ws) != 4 {
		t.Fatalf("wakes: %+v %v", ws, err)
	}
	if early, _ := s.Wakes(now.Add(time.Second)); len(early) != 0 {
		t.Fatalf("since: %+v", early)
	}
}

func TestSLOBreachLifecycle(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "jobs.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	now := time.Unix(1_800_000_000, 0)
	if b, err := s.AddSLOSample("deploy", 600, 600, now); err != nil || b != nil {
		t.Fatalf("at the norm: %+v %v", b, err)
	}
	b, err := s.AddSLOSample("deploy", 601, 600, now)
	if err != nil || b == nil || b.Target != 600 {
		t.Fatalf("over the norm: %+v %v", b, err)
	}
	if b, err := s.AddSLOSample("free", 9999, 0, now); err != nil || b != nil {
		t.Fatalf("no norm: %+v %v", b, err)
	}
	open, _ := s.UnnotedSLOBreaches()
	if len(open) != 1 {
		t.Fatalf("breaches: %+v", open)
	}
	if err := s.MarkSLOBreachesNoted([]int64{open[0].ID}, now.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	if err := s.MarkSLOBreachesNoted(nil, now); err != nil {
		t.Fatal(err)
	}
	if open, _ = s.UnnotedSLOBreaches(); len(open) != 0 {
		t.Fatalf("still unnoted: %+v", open)
	}
	if samples, _ := s.SLOSamples(time.Time{}); len(samples) != 3 {
		t.Fatalf("samples: %+v", samples)
	}
	if err := s.SetSLOBreachInvestigated(b.ID, now, "job1", ""); err != nil {
		t.Fatal(err)
	}
	if got, _ := s.SLOInvestigationsSince(now.Add(-time.Hour)); len(got) != 1 {
		t.Fatalf("investigations: %+v", got)
	}
	if err := s.SetSLOBreachInvestigated(9999, now, "", "x"); err == nil {
		t.Fatal("unknown breach accepted")
	}
}
