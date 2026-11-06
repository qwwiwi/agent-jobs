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
		kind  string
		at    time.Time
		epoch int64
	}{{IdleNote, now, 1}, {IdleEval, now.Add(time.Hour), 2}, {IdleNote, now.Add(2 * time.Hour), 3}} {
		if err := s.AddIdlePickEvent(e.kind, e.at, e.epoch); err != nil {
			t.Fatal(err)
		}
	}
	if at, err := s.LastIdlePickEvent(IdleNote); err != nil || at == nil || !at.Equal(now.Add(2*time.Hour)) {
		t.Fatalf("last note: %v %v", at, err)
	}
	if at, err := s.LastIdlePickEvent(IdleNote, IdleEval); err != nil || !at.Equal(now.Add(2*time.Hour)) {
		t.Fatalf("last of two kinds: %v %v", at, err)
	}
	if n, err := s.LastIdlePickEpoch(IdleEval); err != nil || n != 2 {
		t.Fatalf("last eval epoch: %d %v", n, err)
	}
	if n, err := s.LastIdlePickEpoch(IdleNote, IdleEval); err != nil || n != 3 {
		t.Fatalf("last epoch of two kinds: %d %v", n, err)
	}
	if n, err := s.LastIdlePickEpoch(IdleScan); err != nil || n != 0 {
		t.Fatalf("no scan epoch: %d %v", n, err)
	}
	if n, err := s.CountIdlePickEvents(now.Add(30*time.Minute), IdleNote); err != nil || n != 1 {
		t.Fatalf("count since: %d %v", n, err)
	}
	// Events older than a week go with the next write.
	if err := s.AddIdlePickEvent(IdleScan, now.Add(8*24*time.Hour), 0); err != nil {
		t.Fatal(err)
	}
	if n, err := s.CountIdlePickEvents(time.Unix(0, 0), IdleNote, IdleEval); err != nil || n != 0 {
		t.Fatalf("old events kept: %d %v", n, err)
	}
}

func TestCountActive(t *testing.T) {
	s := openIdleStore(t)
	if n, err := s.CountActive(); err != nil || n != 0 {
		t.Fatalf("empty: %d %v", n, err)
	}
}

// #165: the epoch counter advances once per job that ends, however the end comes and within whatever
// second, and not for a job that is only inserted, requeued or already over.
func TestFinishSeqCountsEndedJobs(t *testing.T) {
	s := openIdleStore(t)
	t0 := time.Unix(1_800_000_000, 0)
	seq := func() int64 {
		t.Helper()
		n, err := s.FinishSeq()
		if err != nil {
			t.Fatal(err)
		}
		return n
	}
	if seq() != 0 {
		t.Fatalf("fresh store: %d", seq())
	}
	queue := func(id string) {
		t.Helper()
		if err := s.Insert(&Job{ID: id, Agent: "agent1", Title: id, Kind: "fix", Brief: "b", Status: StatusQueued,
			Model: "opus", MaxTurns: 10, MaxBudgetUSD: 1, TimeoutMinutes: 5, CreatedAt: t0}); err != nil {
			t.Fatal(err)
		}
	}
	queue("a")
	if seq() != 0 {
		t.Fatalf("insert advanced the epoch: %d", seq())
	}
	if err := s.Finish("a", "", StatusCancelled, 0, 0, 0, "", t0); err != nil {
		t.Fatal(err)
	}
	if seq() != 1 {
		t.Fatalf("after one end: %d", seq())
	}
	queue("b") // enqueued and cancelled in the same second as a
	if err := s.Finish("b", "", StatusCancelled, 0, 0, 0, "", t0); err != nil {
		t.Fatal(err)
	}
	if seq() != 2 {
		t.Fatalf("same-second end not counted: %d", seq())
	}
	if err := s.Finish("b", "", StatusCancelled, 0, 0, 0, "", t0); err == nil && seq() != 2 {
		t.Fatalf("a job that is already over counted again: %d", seq())
	}
}

// #165: the acknowledgement and the idle event are one transaction: an event that cannot be written
// leaves the job pending.
func TestMarkNotifiedBatchAtomic(t *testing.T) {
	s := openIdleStore(t)
	t0 := time.Unix(1_800_000_000, 0)
	insertJob(t, s, "a", false, t0)
	insertJob(t, s, "b", false, t0)
	if _, err := s.db.Exec(`CREATE TRIGGER fail_idle BEFORE INSERT ON idle_pick_events BEGIN SELECT RAISE(ABORT, 'disk full'); END`); err != nil {
		t.Fatal(err)
	}
	if err := s.MarkNotifiedBatch([]string{"a", "b"}, t0, &IdleMark{Kind: IdleNote, Epoch: 4}); err == nil {
		t.Fatal("event write failure not reported")
	}
	for _, id := range []string{"a", "b"} {
		if j, _ := s.Get(id); j.NotifiedAt != nil {
			t.Fatalf("%s marked notified without its note recorded", id)
		}
	}
	if _, err := s.db.Exec(`DROP TRIGGER fail_idle`); err != nil {
		t.Fatal(err)
	}
	if err := s.MarkNotifiedBatch([]string{"a", "b"}, t0, &IdleMark{Kind: IdleNote, Epoch: 4}); err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"a", "b"} {
		if j, _ := s.Get(id); j.NotifiedAt == nil {
			t.Fatalf("%s not marked notified", id)
		}
	}
	if n, err := s.LastIdlePickEpoch(IdleNote); err != nil || n != 4 {
		t.Fatalf("note epoch: %d %v", n, err)
	}
}

// #165: the recovery scan re-arms the latest delivered job that is not an owner-waits job.
func TestRearmLastNotified(t *testing.T) {
	s := openIdleStore(t)
	t0 := time.Unix(1_800_000_000, 0)
	if id, err := s.RearmLastNotified(); err != nil || id != "" {
		t.Fatalf("empty store: %q %v", id, err)
	}
	add := func(id string, ownerWaits bool, finished time.Time) {
		t.Helper()
		if err := s.Insert(&Job{ID: id, Agent: "agent1", Title: id, Kind: "fix", Brief: "b", Status: StatusSucceeded,
			Model: "opus", MaxTurns: 10, MaxBudgetUSD: 1, TimeoutMinutes: 5, CreatedAt: t0, FinishedAt: &finished,
			OwnerWaits: ownerWaits}); err != nil {
			t.Fatal(err)
		}
		if err := s.MarkNotified(id, finished); err != nil {
			t.Fatal(err)
		}
	}
	add("old", false, t0)
	add("newer", false, t0.Add(time.Minute))
	add("waits", true, t0.Add(time.Hour))
	id, err := s.RearmLastNotified()
	if err != nil || id != "newer" {
		t.Fatalf("rearmed %q %v", id, err)
	}
	if j, _ := s.Get("newer"); j.NotifiedAt != nil {
		t.Fatal("job not pending again")
	}
	if j, _ := s.Get("waits"); j.NotifiedAt == nil {
		t.Fatal("an owner-waits job was re-armed")
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
