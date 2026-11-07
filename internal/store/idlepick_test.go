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
	if n, err := s.LastIdlePickEpoch("other"); err != nil || n != 0 {
		t.Fatalf("no epoch of an unknown kind: %d %v", n, err)
	}
	if n, err := s.CountIdlePickEvents(now.Add(30*time.Minute), IdleNote); err != nil || n != 1 {
		t.Fatalf("count since: %d %v", n, err)
	}
	// Events older than a week go with the next write.
	if err := s.AddIdlePickEvent(IdleEval, now.Add(8*24*time.Hour), 0); err != nil {
		t.Fatal(err)
	}
	if n, err := s.CountIdlePickEvents(time.Unix(0, 0), IdleNote, IdleEval); err != nil || n != 1 {
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

// #165 F4: the delivery record belongs to one batch, is replaced by the next one, and goes with the
// acknowledgement, in the same transaction.
func TestIdleDeliveryRecord(t *testing.T) {
	s := openIdleStore(t)
	t0 := time.Unix(1_800_000_000, 0)
	insertJob(t, s, "a", false, t0)
	if d, err := s.IdleDeliveryFor("a"); err != nil || d != nil {
		t.Fatalf("empty: %v %v", d, err)
	}
	if err := s.SaveIdleDelivery(IdleDelivery{Batch: "a", Epoch: 3, Note: "idle: x"}); err != nil {
		t.Fatal(err)
	}
	d, err := s.IdleDeliveryFor("a")
	if err != nil || d == nil || d.Epoch != 3 || d.Note != "idle: x" {
		t.Fatalf("saved record: %+v %v", d, err)
	}
	if other, err := s.IdleDeliveryFor("a,b"); err != nil || other != nil {
		t.Fatalf("record of another batch: %+v %v", other, err)
	}
	if err := s.MarkNotifiedBatch([]string{"a"}, t0, &IdleMark{Kind: IdleNote, Epoch: 3}); err != nil {
		t.Fatal(err)
	}
	if d, err := s.IdleDeliveryFor("a"); err != nil || d != nil {
		t.Fatalf("record kept after the acknowledgement: %+v %v", d, err)
	}
}

// #193: only the exact batch that owns a saved delivery may clear it; batch order is irrelevant.
func TestMarkNotifiedBatchClearsOnlyMatchingIdleDelivery(t *testing.T) {
	for _, tc := range []struct {
		name  string
		ids   []string
		idle  *IdleMark
		clear bool
	}{
		{name: "unrelated acknowledgement", ids: []string{"c"}},
		{name: "unrelated idle note", ids: []string{"c"}, idle: &IdleMark{Kind: IdleNote, Epoch: 4}},
		{name: "partial batch", ids: []string{"a"}},
		{name: "larger batch", ids: []string{"a", "b", "c"}},
		{name: "empty batch"},
		{name: "matching batch", ids: []string{"a", "b"}, idle: &IdleMark{Kind: IdleNote, Epoch: 3}, clear: true},
		{name: "matching reversed batch without note", ids: []string{"b", "a"}, clear: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := openIdleStore(t)
			now := time.Unix(1_800_000_000, 0)
			for _, id := range []string{"a", "b", "c"} {
				insertJob(t, s, id, false, now)
			}
			want := IdleDelivery{Batch: "a,b", Epoch: 3, Note: "idle: ready work"}
			if err := s.SaveIdleDelivery(want); err != nil {
				t.Fatal(err)
			}
			if err := s.MarkNotifiedBatch(tc.ids, now, tc.idle); err != nil {
				t.Fatal(err)
			}
			got, err := s.IdleDeliveryFor(want.Batch)
			if err != nil {
				t.Fatal(err)
			}
			if tc.clear {
				if got != nil {
					t.Fatalf("matching delivery record kept: %+v", got)
				}
			} else if got == nil || *got != want {
				t.Fatalf("unrelated acknowledgement changed saved delivery: got %+v, want %+v", got, want)
			}
		})
	}
}

// #165 F3: opening a database that predates the epoch counter treats the finished jobs as covered (no
// note for history), except a completion whose job-done line is still pending: that one keeps its epoch
// open and gets its note on its own line.
func TestIdlePickUpgradeCoversHistory(t *testing.T) {
	t0 := time.Unix(1_800_000_000, 0)
	for _, tc := range []struct {
		name        string
		pending     bool
		wantCovered int64
	}{
		{"delivered history only", false, 3},
		{"pending completion across the upgrade", true, 2},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "jobs.db")
			s, err := Open(path)
			if err != nil {
				t.Fatal(err)
			}
			for _, id := range []string{"a", "b", "c"} {
				insertJob(t, s, id, false, t0)
				if id != "c" || !tc.pending {
					if err := s.MarkNotified(id, t0); err != nil {
						t.Fatal(err)
					}
				}
			}
			// Back to the schema of a database that predates idle pick.
			for _, q := range []string{`DROP TRIGGER idle_pick_job_ended`, `DROP TABLE idle_pick_seq`,
				`DROP TABLE idle_pick_events`, `DROP TABLE idle_pick_delivery`} {
				if _, err := s.db.Exec(q); err != nil {
					t.Fatal(err)
				}
			}
			if err := s.Close(); err != nil {
				t.Fatal(err)
			}
			s, err = Open(path)
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = s.Close() }()
			if n, err := s.FinishSeq(); err != nil || n != 3 {
				t.Fatalf("epoch counter after the upgrade: %d %v", n, err)
			}
			if n, err := s.LastIdlePickEpoch(IdleEval, IdleNote); err != nil || n != tc.wantCovered {
				t.Fatalf("covered epoch %d, want %d (%v)", n, tc.wantCovered, err)
			}
			// A second open changes nothing.
			if err := s.Close(); err != nil {
				t.Fatal(err)
			}
			if s, err = Open(path); err != nil {
				t.Fatal(err)
			}
			if n, err := s.LastIdlePickEpoch(IdleEval, IdleNote); err != nil || n != tc.wantCovered {
				t.Fatalf("covered epoch after a reopen %d, want %d (%v)", n, tc.wantCovered, err)
			}
		})
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
