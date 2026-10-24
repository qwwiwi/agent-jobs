package store

import (
	"path/filepath"
	"testing"
	"time"
)

// A waiver is bound to the repo, the checklist hash and version, and the sha; a second one for the same
// key replaces the quote; another hash or version sees none; the table survives a reopen.
func TestWaiversBoundToHashVersionAndSHA(t *testing.T) {
	path := filepath.Join(t.TempDir(), "jobs.db")
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	at := time.Date(2026, 10, 24, 9, 0, 0, 0, time.UTC)
	w := Waiver{Repo: "/r", ChecklistID: "demo", ChecklistSHA256: "h1", ChecklistVersion: 2, SHA: "s1", Requirement: "R02", Quote: "ok without it", CreatedAt: at}
	for _, x := range []Waiver{w,
		{Repo: "/r", ChecklistID: "demo", ChecklistSHA256: "h1", ChecklistVersion: 2, SHA: "s1", Requirement: "R01", Quote: "q", CreatedAt: at},
		{Repo: "/r", ChecklistID: "demo", ChecklistSHA256: "h2", ChecklistVersion: 2, SHA: "s1", Requirement: "R02", Quote: "other hash", CreatedAt: at},
		{Repo: "/r", ChecklistID: "demo", ChecklistSHA256: "h1", ChecklistVersion: 5, SHA: "s1", Requirement: "R02", Quote: "other version", CreatedAt: at},
	} {
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
	got, err := s.Waivers("/r", "h1", 2)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0].Requirement != "R01" || got[1].Quote != "new quote" || !got[1].CreatedAt.Equal(at) || got[1].ChecklistVersion != 2 {
		t.Fatalf("waivers %+v", got)
	}
	if got, _ := s.Waivers("/other", "h1", 2); len(got) != 0 {
		t.Fatalf("other repo %+v", got)
	}
	if got, _ := s.Waivers("/r", "h1", 5); len(got) != 1 || got[0].Quote != "other version" {
		t.Fatalf("other version %+v", got)
	}
	for _, bad := range []Waiver{
		{Repo: "/r", ChecklistSHA256: "h1", ChecklistVersion: 2, SHA: "s1", Requirement: "R01"},
		{Repo: "", ChecklistSHA256: "h", ChecklistVersion: 2, SHA: "s", Requirement: "R01", Quote: "q"},
		{Repo: "/r", ChecklistSHA256: "h", SHA: "s", Requirement: "R01", Quote: "q"},
	} {
		if err := s.PutWaiver(bad); err == nil {
			t.Fatalf("incomplete waiver stored: %+v", bad)
		}
	}
}

// Approvals are a log per repository: the latest one is in force; none for an unknown repo; an
// incomplete one is refused; they survive a reopen.
func TestChecklistApprovalsLatest(t *testing.T) {
	path := filepath.Join(t.TempDir(), "jobs.db")
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if a, err := s.LatestApproval("/r"); a != nil || err != nil {
		t.Fatalf("empty store: %+v %v", a, err)
	}
	at := time.Date(2026, 10, 24, 9, 0, 0, 0, time.UTC)
	for _, v := range []int{2, 4} {
		a := ChecklistApproval{Repo: "/r", ChecklistID: "demo", Version: v, ChecklistSHA256: "h", BriefSHA256: "b",
			Quote: "approved", ApprovedAt: at.Add(time.Duration(v) * time.Minute)}
		if err := s.PutApproval(a); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.PutApproval(ChecklistApproval{Repo: "/other", ChecklistID: "x", Version: 9, ChecklistSHA256: "h", BriefSHA256: "b",
		Quote: "q", ApprovedAt: at}); err != nil {
		t.Fatal(err)
	}
	s.Close()
	if s, err = Open(path); err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	a, err := s.LatestApproval("/r")
	if err != nil || a == nil || a.Version != 4 || a.ChecklistID != "demo" || a.BriefSHA256 != "b" || !a.ApprovedAt.Equal(at.Add(4*time.Minute)) {
		t.Fatalf("latest %+v %v", a, err)
	}
	if err := s.PutApproval(ChecklistApproval{Repo: "/r", ChecklistID: "demo", Version: 1, ChecklistSHA256: "h", BriefSHA256: "b"}); err == nil {
		t.Fatal("approval without a quote stored")
	}
}
