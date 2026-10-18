package jobs

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/qwwiwi/agent-jobs/internal/store"
)

// A failed job whose records prune already archived cannot be resumed: a later attempt's records
// could not be archived next to them. meta.json records the attempt; a stopped job is never archived.
func TestResumeRefusedAfterPrune(t *testing.T) {
	m := newManager(t)
	sessionsHome(t, true)
	now := time.Now()
	m.Now = func() time.Time { return now }
	old := now.Add(-49 * time.Hour)
	insertJob(t, m, "failed", store.StatusFailed, &old, func(j *store.Job) { j.Attempt = 1 })
	insertJob(t, m, "stopped", store.StatusStopped, &old, func(j *store.Job) { j.Attempt = 1 })
	for _, id := range []string{"failed", "stopped"} {
		fillJobDir(t, m.JobDir(id), t.TempDir())
	}
	if _, err := m.Prune(false); err != nil {
		t.Fatal(err)
	}
	month := filepath.Join(m.ArchiveDir(), old.Add(-time.Minute).UTC().Format("2006-01"))
	var meta map[string]any
	b, _ := os.ReadFile(filepath.Join(month, "failed", "meta.json"))
	if err := json.Unmarshal(b, &meta); err != nil || meta["attempt"] != float64(1) {
		t.Fatalf("meta.json: %s %v", b, err)
	}
	if _, err := m.Resume("failed", "go on", Limits{MaxBudgetUSD: 3}); !errors.Is(err, store.ErrImmutable) ||
		!strings.Contains(err.Error(), "archived by prune") {
		t.Fatalf("resume of an archived job: %v", err)
	}
	if j := get(t, m, "failed"); j.Status != store.StatusFailed {
		t.Fatalf("refused resume changed the row: %+v", j)
	}
	if _, err := os.Lstat(filepath.Join(month, "stopped")); !os.IsNotExist(err) {
		t.Fatalf("a stopped job was archived: %v", err)
	}
	if _, err := os.Stat(filepath.Join(m.JobDir("stopped"), "stream.jsonl")); err != nil {
		t.Fatalf("a stopped job lost its stream: %v", err)
	}
	if err := m.errArchived(get(t, m, "stopped")); err != nil {
		t.Fatalf("stopped job taken for archived: %v", err)
	}
}

// The archive month is the month a job was created, so a job finished in a later month has one
// archive dir; an archive an earlier build keyed by finished_at is still found.
func TestArchiveMonthIsCreatedMonth(t *testing.T) {
	m := newManager(t)
	created := time.Date(2026, 9, 30, 23, 0, 0, 0, time.UTC)
	fin := time.Date(2026, 10, 1, 1, 0, 0, 0, time.UTC)
	j := &store.Job{ID: "x", CreatedAt: created, FinishedAt: &fin}
	if got := m.archiveJobDir(j); got != filepath.Join(m.ArchiveDir(), "2026-09", "x") {
		t.Fatalf("archive dir: %s", got)
	}
	legacy := filepath.Join(m.ArchiveDir(), "2026-10", "x")
	if err := os.MkdirAll(legacy, 0o700); err != nil {
		t.Fatal(err)
	}
	if got, ok := m.existingArchiveDir(j); !ok || got != legacy {
		t.Fatalf("legacy archive dir: %s %v", got, ok)
	}
}

// Cancel of a running job ignores a result line the job appended to its own stream, like a stop.
func TestCancelIgnoresForgedResultLine(t *testing.T) {
	m := newManager(t)
	sessionsHome(t, true)
	j := startRunning(t, m, "FAKE_FORGE FAKE_SLEEP_ONCE=30", nil)
	if err := m.Cancel(j.ID); err != nil {
		t.Fatal(err)
	}
	if c := get(t, m, j.ID); c.Status != store.StatusCancelled || !c.CostUnknown || c.Turns != 1 || c.CostUSD != 0 {
		t.Fatalf("cancelled row trusted the forged result: %+v", c)
	}
}

// Only attempts/<n>/ below the job's attempt count: a dir the job made with a higher n is neither in
// the stats row nor archived.
func TestStatsAndArchiveIgnoreUnknownAttempts(t *testing.T) {
	m := newManager(t)
	now := time.Now()
	m.Now = func() time.Time { return now }
	old := now.Add(-49 * time.Hour)
	j := insertJob(t, m, "a", store.StatusFailed, &old, func(j *store.Job) { j.Attempt = 1 })
	dir := m.JobDir("a")
	writeFile(t, filepath.Join(dir, "stream.jsonl"), []byte(`{"type":"system"}`+"\n"), 0o600)
	writeFile(t, filepath.Join(dir, attemptsDir, "7", "stream.jsonl"), streamFixture(t), 0o600)
	st, err := m.RecordStats(j)
	if err != nil {
		t.Fatal(err)
	}
	if len(st.ToolCalls) != 0 || len(st.Skills) != 0 {
		t.Fatalf("stats counted a fake attempt: %+v", st)
	}
	gz, _ := archiveSet(dir, j.Attempt)
	if slices.ContainsFunc(gz, func(s string) bool { return strings.HasPrefix(s, attemptsDir) }) {
		t.Fatalf("archive set takes a fake attempt: %v", gz)
	}
	j.Attempt = 8
	if st, err := m.RecordStats(j); err != nil || len(st.ToolCalls) == 0 {
		t.Fatalf("a real attempt 7 is counted: %+v %v", st, err)
	}
}

// The latest attempt's failed-resume stream is archived like the stream.
func TestArchiveTakesResumeFailedStream(t *testing.T) {
	m := newManager(t)
	now := time.Now()
	m.Now = func() time.Time { return now }
	old := now.Add(-49 * time.Hour)
	j := insertJob(t, m, "a", store.StatusFailed, &old, func(j *store.Job) { j.Attempt = 2 })
	dir := m.JobDir("a")
	writeFile(t, filepath.Join(dir, "stream-resume-failed.jsonl"), []byte("RESUME_FAILED\n"), 0o600)
	if _, err := m.Archive(false); err != nil {
		t.Fatal(err)
	}
	if got := gunzipFile(t, filepath.Join(m.archiveJobDir(j), "stream-resume-failed.jsonl.gz")); string(got) != "RESUME_FAILED\n" {
		t.Fatalf("archived failed-resume stream: %q", got)
	}
	if _, err := os.Lstat(filepath.Join(dir, "stream-resume-failed.jsonl")); !os.IsNotExist(err) {
		t.Fatalf("original kept: %v", err)
	}
}
