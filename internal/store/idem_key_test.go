package store

import (
	"errors"
	"path/filepath"
	"testing"
	"time"
)

func idemJob(id, key string) *Job {
	return &Job{ID: id, Agent: "test", Title: id, Kind: "general", Brief: "b", Model: "sonnet",
		Status: StatusQueued, CreatedAt: time.Now(), IdemKey: key}
}

// H2 (#197 fix loop 1): a job's idempotency key is unique, survives a reopen, and jobs without a key
// do not collide with each other.
func TestIdemKeyIsUniqueAndRoundTrips(t *testing.T) {
	dbpath := filepath.Join(t.TempDir(), "jobs.db")
	s, err := Open(dbpath)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Insert(idemJob("a", "control-review:test:fix1:1")); err != nil {
		t.Fatal(err)
	}
	if err := s.Insert(idemJob("b", "control-review:test:fix1:1")); !errors.Is(err, ErrIdemKeyExists) {
		t.Fatalf("second job with the same key: %v, want ErrIdemKeyExists", err)
	}
	if _, err := s.Get("b"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("the refused job was stored: %v", err)
	}
	for _, id := range []string{"c", "d"} { // no key: any number of them
		if err := s.Insert(idemJob(id, "")); err != nil {
			t.Fatalf("keyless job %s: %v", id, err)
		}
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	s, err = Open(dbpath)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = s.Close() }()
	got, err := s.GetByIdemKey("control-review:test:fix1:1")
	if err != nil || got.ID != "a" || got.IdemKey != "control-review:test:fix1:1" {
		t.Fatalf("GetByIdemKey: %+v, %v", got, err)
	}
	if _, err := s.GetByIdemKey("control-review:test:fix1:2"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("unknown key: %v", err)
	}
	if _, err := s.GetByIdemKey(""); !errors.Is(err, ErrNotFound) {
		t.Fatalf("empty key must find nothing: %v", err)
	}
}
