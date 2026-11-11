package store

import (
	"database/sql"
	"path/filepath"
	"testing"
	"time"
)

// The fix-loop bypass columns (v0.8.2 G05) are additive: an old database migrates with empty defaults,
// and a job's design / loop_override survive a reopen.
func TestFixLoopBypassMigrationAndRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "jobs.db")
	raw, err := sql.Open("sqlite", "file:"+path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := raw.Exec(stage1Schema); err != nil {
		t.Fatal(err)
	}
	_ = raw.Close()
	for i := range 2 {
		s, err := Open(path)
		if err != nil {
			t.Fatal(err)
		}
		if i == 0 {
			j, err := s.Get("old-1")
			if err != nil {
				t.Fatal(err)
			}
			if j.LoopDesign != "" || j.LoopOverride != "" {
				t.Fatalf("migration defaults: design %q override %q", j.LoopDesign, j.LoopOverride)
			}
			j.ID, j.LoopDesign, j.LoopOverride = "bypass-job", "design-job", "owner approved a third loop"
			if err := s.Insert(j); err != nil {
				t.Fatal(err)
			}
		}
		j, err := s.Get("bypass-job")
		if err != nil || j.LoopDesign != "design-job" || j.LoopOverride != "owner approved a third loop" {
			t.Fatalf("round trip: %+v %v", j, err)
		}
		_ = s.Close()
	}
}

// FixByControlJob finds the fix job of an agent whose control review is the given job, and nothing for
// a review no fix queued or a fix of another agent.
func TestFixByControlJob(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "jobs.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	now := time.Unix(1770000000, 0)
	for _, j := range []*Job{
		{ID: "fix-a", Agent: "x", Title: "t", Kind: "fix", Status: StatusSucceeded, CreatedAt: now, ControlJob: "ctl-a"},
		{ID: "fix-b", Agent: "y", Title: "t", Kind: "fix", Status: StatusSucceeded, CreatedAt: now, ControlJob: "ctl-b"},
	} {
		if err := s.Insert(j); err != nil {
			t.Fatal(err)
		}
	}
	if j, err := s.FixByControlJob("x", "ctl-a"); err != nil || j == nil || j.ID != "fix-a" {
		t.Fatalf("fix of ctl-a: %+v %v", j, err)
	}
	for _, c := range []struct{ agent, id string }{{"x", "ctl-b"}, {"x", "none"}, {"y", "ctl-a"}} {
		if j, err := s.FixByControlJob(c.agent, c.id); err != nil || j != nil {
			t.Fatalf("%s/%s: want nothing, got %+v %v", c.agent, c.id, j, err)
		}
	}
}
