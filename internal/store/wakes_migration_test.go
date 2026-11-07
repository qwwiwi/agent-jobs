package store

import (
	"database/sql"
	"path/filepath"
	"slices"
	"testing"
	"time"
)

// The wake schema before member identities were stored separately (#170).
const previousWakesSchema = `
CREATE TABLE orchestrator_wakes (
	id INTEGER PRIMARY KEY AUTOINCREMENT,
	at INTEGER NOT NULL,
	kind TEXT NOT NULL,
	category TEXT NOT NULL,
	bytes INTEGER NOT NULL,
	batch_size INTEGER NOT NULL DEFAULT 1,
	members TEXT NOT NULL DEFAULT '',
	ref TEXT NOT NULL DEFAULT ''
);
CREATE INDEX orchestrator_wakes_at ON orchestrator_wakes(at);
CREATE UNIQUE INDEX orchestrator_wakes_ref ON orchestrator_wakes(kind, category, ref) WHERE ref <> '';
`

func TestWakeMemberRefsBackfilledBeforeRegroupedRetry(t *testing.T) {
	for _, ref := range []string{"job-done:a@1.1800000000,b@1.1800000010", "job-done:b@1.1800000010"} {
		t.Run(ref, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "jobs.db")
			db, err := sql.Open("sqlite", path)
			if err != nil {
				t.Fatal(err)
			}
			defer db.Close()
			if _, err := db.Exec(previousWakesSchema); err != nil {
				t.Fatal(err)
			}
			// The delivery succeeded before upgrade, but its acknowledgement failed.
			if _, err := db.Exec(`INSERT INTO orchestrator_wakes
				(at, kind, category, bytes, batch_size, members, ref)
				VALUES (1800000100, 'delivery', 'job-done', 100, 2, 'a,b', ?)`, ref); err != nil {
				t.Fatal(err)
			}
			for open := 0; open < 2; open++ {
				s, err := Open(path)
				if err != nil {
					t.Fatal(err)
				}
				// a resumed before the retry, so only b's original attempt remains.
				added, err := s.AddWake(Wake{At: time.Unix(1800000200, 0), Kind: WakeDelivery,
					Category: "job-done", Bytes: 50, Members: "b", Ref: "job-done:b@1.1800000010",
					MemberRefs: []string{"b@1.1800000010"}})
				ws, readErr := s.Wakes(time.Time{})
				_ = s.Close()
				if err != nil || readErr != nil || added || len(ws) != 1 {
					t.Fatalf("open %d: regrouped retry added=%v, wakes=%+v, errors=%v/%v", open, added, ws, err, readErr)
				}
				if ws[0].Bytes != 100 || ws[0].BatchSize != 2 || ws[0].Members != "a,b" {
					t.Fatalf("historical wake changed: %+v", ws[0])
				}
			}
			// A newly finished attempt of b must still count after migration.
			s, err := Open(path)
			if err != nil {
				t.Fatal(err)
			}
			defer s.Close()
			if added, err := s.AddWake(Wake{At: time.Unix(1800000300, 0), Kind: WakeDelivery,
				Category: "job-done", Ref: "job-done:b@2.1800000300", MemberRefs: []string{"b@2.1800000300"}}); err != nil || !added {
				t.Fatalf("new attempt: added=%v, err=%v", added, err)
			}
		})
	}
}

func TestWakeMemberRefsBackfillScopeAndIdempotency(t *testing.T) {
	path := filepath.Join(t.TempDir(), "jobs.db")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Exec(previousWakesSchema); err != nil {
		t.Fatal(err)
	}
	for _, row := range []struct{ kind, category, ref string }{
		{WakeDelivery, "job-done", "job-done:a@1.1,b@1.2"},
		{WakeDelivery, "job-done", "job-done:b@1.2,c@2.3"},
		{WakeDelivery, "job-done", "job-done:d@1"}, // An old attempt without a finish timestamp.
		{WakeDelivery, "job-done", "job-done:"},
		{WakeDelivery, "job-done", ""},
		{WakeDelivery, "job-done", "other:ignored"},
		{WakeDelivery, "schedule", "job-done:wrong-category"},
		{WakeInjection, "job-done", "job-done:wrong-kind"},
	} {
		if _, err := db.Exec(`INSERT INTO orchestrator_wakes (at, kind, category, bytes, ref) VALUES (1, ?, ?, 10, ?)`, row.kind, row.category, row.ref); err != nil {
			t.Fatal(err)
		}
	}
	for open := 0; open < 2; open++ {
		s, err := Open(path)
		if err != nil {
			t.Fatal(err)
		}
		rows, err := s.db.Query(`SELECT ref FROM wake_member_refs WHERE kind = 'delivery' AND category = 'job-done' ORDER BY ref`)
		if err != nil {
			_ = s.Close()
			t.Fatal(err)
		}
		var refs []string
		for rows.Next() {
			var ref string
			if err := rows.Scan(&ref); err != nil {
				t.Fatal(err)
			}
			refs = append(refs, ref)
		}
		if err := rows.Err(); err != nil {
			t.Fatal(err)
		}
		_ = rows.Close()
		var total int
		if err := s.db.QueryRow(`SELECT COUNT(*) FROM wake_member_refs`).Scan(&total); err != nil {
			t.Fatal(err)
		}
		_ = s.Close()
		if total != 4 {
			t.Fatalf("open %d: backfilled %d identities, want 4", open, total)
		}
		if want := []string{"a@1.1", "b@1.2", "c@2.3", "d@1"}; !slices.Equal(refs, want) {
			t.Fatalf("open %d: backfilled refs %q, want %q", open, refs, want)
		}
	}
}
