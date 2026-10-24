package store

import (
	"database/sql"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// oldWaiversSchema is checklist_waivers as first released: no checklist_version, so its primary key
// does not name one.
const oldWaiversSchema = `CREATE TABLE checklist_waivers (
	repo             TEXT NOT NULL,
	checklist_id     TEXT NOT NULL,
	checklist_sha256 TEXT NOT NULL,
	sha              TEXT NOT NULL,
	requirement      TEXT NOT NULL,
	quote            TEXT NOT NULL,
	created_at       INTEGER NOT NULL,
	PRIMARY KEY (repo, checklist_sha256, sha, requirement)
)`

// An old checklist_waivers table is rebuilt with the versioned key: its rows are kept (version 0), two
// waivers that differ only in version can then coexist, and reopening is a no-op.
func TestOpenMigratesOldWaiverKey(t *testing.T) {
	path := filepath.Join(t.TempDir(), "jobs.db")
	raw, err := sql.Open("sqlite", "file:"+path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := raw.Exec(oldWaiversSchema); err != nil {
		t.Fatal(err)
	}
	if _, err := raw.Exec(`INSERT INTO checklist_waivers VALUES ('/r','demo','h1','s1','R01','old quote',1)`); err != nil {
		t.Fatal(err)
	}
	_ = raw.Close()
	for range 2 {
		s, err := Open(path)
		if err != nil {
			t.Fatal(err)
		}
		var def string
		if err := s.db.QueryRow(`SELECT sql FROM sqlite_master WHERE type='table' AND name='checklist_waivers'`).Scan(&def); err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(def, "PRIMARY KEY (repo, checklist_sha256, checklist_version, sha, requirement)") {
			t.Fatalf("old key kept: %s", def)
		}
		old, err := s.Waivers("/r", "h1", 0)
		if err != nil || len(old) != 1 || old[0].Quote != "old quote" || old[0].ChecklistID != "demo" {
			t.Fatalf("old row: %+v %v", old, err)
		}
		at := time.Date(2026, 10, 24, 9, 0, 0, 0, time.UTC)
		for _, v := range []int{2, 3} {
			if err := s.PutWaiver(Waiver{Repo: "/r", ChecklistID: "demo", ChecklistSHA256: "h1", ChecklistVersion: v, SHA: "s1",
				Requirement: "R01", Quote: "q", CreatedAt: at}); err != nil {
				t.Fatal(err)
			}
		}
		for _, v := range []int{0, 2, 3} {
			if w, err := s.Waivers("/r", "h1", v); err != nil || len(w) != 1 {
				t.Fatalf("version %d: %+v %v", v, w, err)
			}
		}
		_ = s.Close()
	}
}
