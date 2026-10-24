package store

import (
	"database/sql"
	"path/filepath"
	"testing"
)

// gate tables as an earlier build of the gate kinds created them: no attempt, no enforced_blind.
const gateTablesBeforeAttempt = `
CREATE TABLE gate_slicing (seq INTEGER PRIMARY KEY AUTOINCREMENT, job_id TEXT NOT NULL, repo TEXT NOT NULL,
	checklist_id TEXT NOT NULL, version INTEGER NOT NULL, checklist_sha256 TEXT NOT NULL, model TEXT NOT NULL,
	runner TEXT NOT NULL, result TEXT NOT NULL, created_at INTEGER NOT NULL);
CREATE TABLE gate_verdicts (seq INTEGER PRIMARY KEY AUTOINCREMENT, job_id TEXT NOT NULL, repo TEXT NOT NULL,
	sha TEXT NOT NULL, checklist_id TEXT NOT NULL, version INTEGER NOT NULL, checklist_sha256 TEXT NOT NULL,
	judge_model TEXT NOT NULL, judge_runner TEXT NOT NULL, judge_family TEXT NOT NULL, coder_families TEXT NOT NULL,
	coder_jobs TEXT NOT NULL, family_differs INTEGER NOT NULL, same_family INTEGER NOT NULL, manifest_check TEXT,
	counts TEXT NOT NULL, verdict TEXT NOT NULL, created_at INTEGER NOT NULL);
INSERT INTO gate_slicing (job_id, repo, checklist_id, version, checklist_sha256, model, runner, result, created_at)
	VALUES ('old', '/r', 'demo', 1, 'h', 'opus', 'claude', '{"unmapped":[],"half":[],"invented":[]}', 1);
`

// Gate tables without the attempt and enforced_blind columns get them on Open; their rows read as
// attempt 0 (no attempt of a job matches them) and not blind.
func TestGateColumnsMigration(t *testing.T) {
	path := filepath.Join(t.TempDir(), "jobs.db")
	raw, err := sql.Open("sqlite", "file:"+path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := raw.Exec(gateTablesBeforeAttempt); err != nil {
		t.Fatal(err)
	}
	_ = raw.Close()
	for i := 0; i < 2; i++ { // a second Open finds the columns there
		s, err := Open(path)
		if err != nil {
			t.Fatal(err)
		}
		rs, err := s.SlicingResultsFor("/r", "demo", 1, "h")
		if err != nil || len(rs) != 1 || rs[0].Attempt != 0 || rs[0].EnforcedBlind {
			t.Fatalf("old row: %+v %v", rs, err)
		}
		if r, err := s.SlicingByJob("old", 1); err != nil || r != nil {
			t.Fatalf("attempt 1 found the old row: %+v %v", r, err)
		}
		if v, err := s.VerdictByJob("old", 1); err != nil || v != nil {
			t.Fatalf("verdicts: %+v %v", v, err)
		}
		_ = s.Close()
	}
}
