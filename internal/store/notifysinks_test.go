package store

import (
	"path/filepath"
	"testing"
	"time"
)

// The breaches a queued line carries are marked noted only when its row is inserted: a batch queued
// before keeps its old row (and line), so the breaches of the new line stay pending.
func TestQueueSinkDeliveryMarksNotedOnlyOnInsert(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "jobs.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	now := time.Unix(1_800_000_000, 0)
	a, err := s.AddSLOSample("deploy", 700, 600, now)
	if err != nil || a == nil {
		t.Fatalf("breach a: %v %v", a, err)
	}
	if err := s.MarkNotifiedSinks(nil, now, nil, &SinkDelivery{Ref: "job-done:x@1.1", Line: "x", Noted: []int64{a.ID}}); err != nil {
		t.Fatal(err)
	}
	b, err := s.AddSLOSample("deploy", 800, 600, now)
	if err != nil || b == nil {
		t.Fatalf("breach b: %v %v", b, err)
	}
	if err := s.MarkNotifiedSinks(nil, now, nil, &SinkDelivery{Ref: "job-done:x@1.1", Line: "x again", Noted: []int64{b.ID}}); err != nil {
		t.Fatal(err)
	}
	bs, err := s.UnnotedSLOBreaches()
	if err != nil || len(bs) != 1 || bs[0].ID != b.ID {
		t.Fatalf("unnoted: %+v %v (want only %d)", bs, err, b.ID)
	}
}
