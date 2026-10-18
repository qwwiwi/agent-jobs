package store

import (
	"database/sql"
	"errors"
	"path/filepath"
	"testing"
	"time"
)

// A stage-1 database gains the intentions table on Open and keeps its jobs; intentions round-trip,
// are edited in place, the status filter holds, and a second Open keeps them.
func TestIntentionsRoundTripOnOldDatabase(t *testing.T) {
	path := filepath.Join(t.TempDir(), "jobs.db")
	raw, err := sql.Open("sqlite", "file:"+path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := raw.Exec(stage1Schema); err != nil {
		t.Fatal(err)
	}
	_ = raw.Close()
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if j, err := s.Get("old-1"); err != nil || j.Title != "old job" {
		t.Fatalf("old job: %+v %v", j, err)
	}
	if list, err := s.ListIntentions(""); err != nil || len(list) != 0 {
		t.Fatalf("empty list: %v %v", list, err)
	}
	t0, t1 := time.Unix(1770000000, 0), time.Unix(1770000600, 0)
	a, err := s.AddIntention("ship the release", "2026-11-01", "high", t0)
	if err != nil {
		t.Fatal(err)
	}
	b, err := s.AddIntention("write the guide", "", "normal", t0)
	if err != nil {
		t.Fatal(err)
	}
	if a.ID == b.ID || a.Status != IntentionOpen || a.Due != "2026-11-01" || a.Priority != "high" ||
		!a.CreatedAt.Equal(t0) || a.ClosedAt != nil {
		t.Fatalf("added: %+v %+v", a, b)
	}
	got, err := s.SetIntention(a.ID, "", IntentionDone, t1)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != IntentionDone || got.Text != "ship the release" || got.ClosedAt == nil ||
		!got.ClosedAt.Equal(t1) || !got.UpdatedAt.Equal(t1) {
		t.Fatalf("done: %+v", got)
	}
	// Text only keeps status and closed_at; reopening clears closed_at.
	if got, err = s.SetIntention(a.ID, "ship v0.4", "", t1.Add(time.Minute)); err != nil ||
		got.Text != "ship v0.4" || got.Status != IntentionDone || !got.ClosedAt.Equal(t1) {
		t.Fatalf("text only: %+v %v", got, err)
	}
	if got, err = s.SetIntention(a.ID, "", IntentionOpen, t1); err != nil || got.ClosedAt != nil {
		t.Fatalf("reopen: %+v %v", got, err)
	}
	if _, err := s.SetIntention(a.ID, "", IntentionDropped, t1); err != nil {
		t.Fatal(err)
	}
	if _, err := s.SetIntention(999, "", IntentionDone, t1); !errors.Is(err, ErrIntentionNotFound) {
		t.Fatalf("unknown id: %v", err)
	}
	if _, err := s.GetIntention(999); !errors.Is(err, ErrIntentionNotFound) {
		t.Fatalf("unknown get: %v", err)
	}
	// The CHECK refuses a status outside the three.
	if _, err := s.SetIntention(b.ID, "", "later", t1); err == nil {
		t.Fatal("bad status stored")
	}
	_ = s.Close()

	s, err = Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	for status, want := range map[string][]int64{"": {a.ID, b.ID}, IntentionOpen: {b.ID}, IntentionDropped: {a.ID}, IntentionDone: nil} {
		list, err := s.ListIntentions(status)
		if err != nil {
			t.Fatal(err)
		}
		if len(list) != len(want) {
			t.Fatalf("status %q: %d rows, want %v", status, len(list), want)
		}
		for i, it := range list {
			if it.ID != want[i] {
				t.Fatalf("status %q: row %d is %d, want %v", status, i, it.ID, want)
			}
		}
	}
}
