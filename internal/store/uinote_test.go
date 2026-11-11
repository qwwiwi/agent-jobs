package store

import (
	"database/sql"
	"errors"
	"path/filepath"
	"testing"
	"time"
)

// A database written before jobs.ui_note gets the column on Open (a second Open finds it there): it is
// nullable with no default, the rows that were there have no note, and the job still reads as it did.
func TestUINoteColumnMigration(t *testing.T) {
	path := filepath.Join(t.TempDir(), "jobs.db")
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Insert(idemJob("old", "")); err != nil {
		t.Fatal(err)
	}
	_ = s.Close()
	raw, err := sql.Open("sqlite", "file:"+path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := raw.Exec(`ALTER TABLE jobs DROP COLUMN ui_note`); err != nil { // the schema of the build before
		t.Fatal(err)
	}
	_ = raw.Close()
	for i := 0; i < 2; i++ {
		s, err := Open(path)
		if err != nil {
			t.Fatal(err)
		}
		var typ string
		var notNull int
		var dflt sql.NullString
		if err := s.db.QueryRow(`SELECT type, "notnull", dflt_value FROM pragma_table_info('jobs') WHERE name='ui_note'`).
			Scan(&typ, &notNull, &dflt); err != nil {
			t.Fatalf("open %d: no ui_note column: %v", i+1, err)
		}
		if typ != "TEXT" || notNull != 0 || dflt.Valid {
			t.Fatalf("ui_note is %s notnull=%d default=%v, want a nullable TEXT with no default", typ, notNull, dflt)
		}
		if n, err := s.UINote("old", 0); err != nil || n != "" {
			t.Fatalf("a row from before the column has note %q, %v", n, err)
		}
		if j, err := s.Get("old"); err != nil || j.ID != "old" {
			t.Fatalf("get: %+v, %v", j, err)
		}
		_ = s.Close()
	}
}

// The stored note is bound to the attempt it was written for and is written only while that attempt
// has not ended.
func TestUINoteBoundToLiveAttempt(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "jobs.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = s.Close() }()
	if err := s.Insert(idemJob("a", "")); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec(`UPDATE jobs SET status='running', launch_token='tok', attempt=2 WHERE id='a'`); err != nil {
		t.Fatal(err)
	}
	note := func(attempt int) string {
		t.Helper()
		n, err := s.UINote("a", attempt)
		if err != nil {
			t.Fatal(err)
		}
		return n
	}
	// Not this row's attempt, not this launch, no job: nothing is written.
	for what, err := range map[string]error{
		"another attempt": s.SetUINote("a", "tok", 1, "n"),
		"another launch":  s.SetUINote("a", "other", 2, "n"),
		"unknown job":     s.SetUINote("zz", "tok", 2, "n"),
		"clear, another":  s.ClearUINote("a", "other"),
	} {
		if !errors.Is(err, ErrImmutable) {
			t.Fatalf("%s: %v, want ErrImmutable", what, err)
		}
	}
	if err := s.SetUINote("a", "tok", 2, ""); err == nil {
		t.Fatal("an empty note was stored")
	}
	if n := note(2); n != "" {
		t.Fatalf("refused writes left %q", n)
	}
	if err := s.SetUINote("a", "tok", 2, "note two"); err != nil {
		t.Fatal(err)
	}
	if n := note(2); n != "note two" {
		t.Fatalf("attempt 2 reads %q", n)
	}
	for _, other := range []int{0, 1, 3} {
		if n := note(other); n != "" {
			t.Fatalf("attempt %d reads the note of attempt 2: %q", other, n)
		}
	}
	if err := s.ClearUINote("a", "tok"); err != nil {
		t.Fatal(err)
	}
	if n := note(2); n != "" {
		t.Fatalf("a cleared note reads %q", n)
	}
	// The attempt ends with its note; after that neither write touches it.
	if err := s.SetUINote("a", "tok", 2, "final"); err != nil {
		t.Fatal(err)
	}
	if err := s.FinishAttempt("a", "tok", StatusSucceeded, 1, 0, false, nil, 1, "", time.Now()); err != nil {
		t.Fatal(err)
	}
	if err := s.ClearUINote("a", "tok"); !errors.Is(err, ErrImmutable) {
		t.Fatalf("clear after the finish: %v", err)
	}
	if err := s.SetUINote("a", "tok", 2, "late"); !errors.Is(err, ErrImmutable) {
		t.Fatalf("set after the finish: %v", err)
	}
	if n := note(2); n != "final" {
		t.Fatalf("after the finish the note is %q", n)
	}
	// A stored value that is not the daemon's format is no note, and an error for the log.
	if _, err := s.db.Exec(`UPDATE jobs SET ui_note='not json' WHERE id='a'`); err != nil {
		t.Fatal(err)
	}
	if n, err := s.UINote("a", 2); n != "" || err == nil {
		t.Fatalf("unreadable value: %q, %v", n, err)
	}
}
