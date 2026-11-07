package store

import (
	"path/filepath"
	"strings"
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
	if open, _ := s.UninvestigatedSLOBreaches(); len(open) != 0 {
		t.Fatalf("still open: %+v", open)
	}
	if err := s.SetSLOBreachInvestigated(9999, now, "", "x"); err == nil {
		t.Fatal("unknown breach accepted")
	}
}

// The claim reserves the breach, counts the reservation, and judges the cooldown on the latest
// started investigation however long ago it was (a cooldown beyond the 24 hours of the daily cap).
func TestClaimSLOInvestigation(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "jobs.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	t0 := time.Unix(1_800_000_000, 0)
	add := func(at time.Time) int64 {
		b, err := s.AddSLOSample("deploy", 700, 600, at)
		if err != nil || b == nil {
			t.Fatalf("breach: %+v %v", b, err)
		}
		return b.ID
	}
	claim := func(id int64, at time.Time) (bool, string) {
		ok, skip, err := s.ClaimSLOInvestigation(id, at, 48*time.Hour, 5)
		if err != nil {
			t.Fatal(err)
		}
		return ok, skip
	}
	first := add(t0)
	if ok, skip := claim(first, t0); !ok || skip != "" {
		t.Fatalf("first: %v %q", ok, skip)
	}
	if ok, skip := claim(first, t0); ok || skip != "" {
		t.Fatalf("decided breach claimed again: %v %q", ok, skip)
	}
	bs, _ := s.UnnotedSLOBreaches()
	if bs[0].InvestigateJob != InvestigatePending {
		t.Fatalf("not reserved: %+v", bs[0])
	}
	second := add(t0.Add(25 * time.Hour))
	if ok, skip := claim(second, t0.Add(25*time.Hour)); ok || !strings.HasPrefix(skip, "cooldown") {
		t.Fatalf("25h into a 48h cooldown: %v %q", ok, skip)
	}
	third := add(t0.Add(49 * time.Hour))
	if ok, _ := claim(third, t0.Add(49*time.Hour)); !ok {
		t.Fatal("past the cooldown")
	}
}

// The pending-breach queries are served by the partial indexes, not by a scan of the whole history.
func TestSLOBreachQueriesUsePartialIndexes(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "jobs.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	for q, index := range map[string]string{
		`SELECT id FROM slo_breaches WHERE noted_at IS NULL ORDER BY id`:                         "slo_breaches_unnoted",
		`SELECT id FROM slo_breaches WHERE investigated_at IS NULL ORDER BY id`:                  "slo_breaches_uninvestigated",
		`SELECT MAX(investigated_at) FROM slo_breaches WHERE investigate_job <> ''`:              "slo_breaches_started",
		`SELECT COUNT(*) FROM slo_breaches WHERE investigate_job <> '' AND investigated_at >= 5`: "slo_breaches_started",
	} {
		rows, err := s.db.Query(`EXPLAIN QUERY PLAN ` + q)
		if err != nil {
			t.Fatal(err)
		}
		var plan []string
		for rows.Next() {
			var id, parent, unused int
			var detail string
			if err := rows.Scan(&id, &parent, &unused, &detail); err != nil {
				t.Fatal(err)
			}
			plan = append(plan, detail)
		}
		_ = rows.Close()
		if !strings.Contains(strings.Join(plan, "|"), index) {
			t.Errorf("%s\nplan %q lacks %s", q, plan, index)
		}
	}
}
