package store

import (
	"path/filepath"
	"testing"
	"time"
)

// A waiver is bound to the repo, the checklist hash and the sha; a second one for the same key
// replaces the quote; another hash sees none; the table survives a reopen.
func TestWaiversBoundToHashAndSHA(t *testing.T) {
	path := filepath.Join(t.TempDir(), "jobs.db")
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	at := time.Date(2026, 10, 24, 9, 0, 0, 0, time.UTC)
	w := Waiver{Repo: "/r", ChecklistID: "demo", ChecklistSHA256: "h1", SHA: "s1", Requirement: "R02", Quote: "ok without it", CreatedAt: at}
	for _, x := range []Waiver{w, {Repo: "/r", ChecklistID: "demo", ChecklistSHA256: "h1", SHA: "s1", Requirement: "R01", Quote: "q", CreatedAt: at},
		{Repo: "/r", ChecklistID: "demo", ChecklistSHA256: "h2", SHA: "s1", Requirement: "R02", Quote: "other hash", CreatedAt: at}} {
		if err := s.PutWaiver(x); err != nil {
			t.Fatal(err)
		}
	}
	w.Quote = "new quote"
	if err := s.PutWaiver(w); err != nil {
		t.Fatal(err)
	}
	s.Close()
	if s, err = Open(path); err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	got, err := s.Waivers("/r", "h1")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0].Requirement != "R01" || got[1].Quote != "new quote" || !got[1].CreatedAt.Equal(at) {
		t.Fatalf("waivers %+v", got)
	}
	if got, _ := s.Waivers("/other", "h1"); len(got) != 0 {
		t.Fatalf("other repo %+v", got)
	}
	for _, bad := range []Waiver{{Repo: "/r", ChecklistSHA256: "h1", SHA: "s1", Requirement: "R01"}, {Repo: "", ChecklistSHA256: "h", SHA: "s", Requirement: "R01", Quote: "q"}} {
		if err := s.PutWaiver(bad); err == nil {
			t.Fatalf("incomplete waiver stored: %+v", bad)
		}
	}
}
