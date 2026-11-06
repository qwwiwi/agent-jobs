package jobs

import (
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"testing"
	"time"

	"github.com/qwwiwi/agent-jobs/internal/store"
)

// The id is a timestamp plus a 16-bit random suffix (newID), so jobs started in the same second
// draw from 65536 values only: the insert's UNIQUE constraint on jobs.id is the check, and the
// enqueue must redraw the suffix instead of failing (#129). Here the clock is frozen, so all ids
// share the timestamp prefix and uniqueness rests on the suffix alone; without the redraw this
// fails on the ~30 collisions expected among 2000 ids drawn from the same second.
func TestJobIDUniqueFrozenClock(t *testing.T) {
	m := newManager(t)
	frozen := time.Date(2026, 11, 5, 23, 0, 35, 0, time.UTC)
	m.Now = func() time.Time { return frozen }
	want := regexp.MustCompile(`^261105-230035-[0-9a-f]{4}$`)
	seen := make(map[string]bool, 2000)
	for i := 0; i < 2000; i++ {
		j, err := m.Enqueue(StartRequest{Title: "unique ids", Kind: "general", Brief: "b", Model: "sonnet"})
		if err != nil {
			t.Fatalf("enqueue %d: %v", i, err)
		}
		if !want.MatchString(j.ID) {
			t.Fatalf("enqueue %d: id %q is not <date-time>-<4 hex>", i, j.ID)
		}
		if seen[j.ID] {
			t.Fatalf("enqueue %d: duplicate id %s", i, j.ID)
		}
		seen[j.ID] = true
	}
}

// setIDSuffixes replaces the id's random source (idRandom) with a fixed sequence of 4-hex-digit
// suffixes, so a forced collision is deterministic: the first enqueue draws suffixes[0], the first
// redraw suffixes[1], and so on. A draw past the script panics instead of drawing silently.
// Restored when the test ends; no test of the package runs in parallel.
func setIDSuffixes(t *testing.T, suffixes ...string) {
	t.Helper()
	next := 0
	prev := idRandom
	idRandom = func(b []byte) (int, error) {
		if next >= len(suffixes) {
			panic(fmt.Sprintf("id suffix script exhausted at %d", next))
		}
		v, err := hex.DecodeString(suffixes[next])
		if err != nil || len(v) != len(b) {
			panic(fmt.Sprintf("bad scripted id suffix %q", suffixes[next]))
		}
		next++
		copy(b, v)
		return len(v), nil
	}
	t.Cleanup(func() { idRandom = prev })
}

// prefixTaken pre-inserts a plain queued row that owns the id <frozen>-<suffix>, with no job folder
// on disk: the folder of that id appears only if an enqueue creates it.
func prefixTaken(t *testing.T, m *Manager, frozen time.Time, suffix string) string {
	t.Helper()
	id := frozen.Format("060102-150405") + "-" + suffix
	j := &store.Job{ID: id, Agent: m.Agent, Title: "taken " + suffix, Kind: "general", Brief: "b",
		Status: store.StatusQueued, Model: "sonnet", TimeoutMinutes: 1, CreatedAt: frozen}
	if err := m.Store.Insert(j); err != nil {
		t.Fatal(err)
	}
	return id
}

// A source-bearing job that draws a taken id must land with its sources under its final id: the
// redraw re-copies the snapshots under the new id and removes the folder this attempt created under
// the taken one (#129). Before the fix the sources stayed under the taken id and the inserted job
// pointed at a missing folder.
func TestEnqueueSourcesLandUnderFinalID(t *testing.T) {
	m := newManager(t)
	frozen := time.Date(2026, 11, 6, 1, 0, 0, 0, time.UTC)
	m.Now = func() time.Time { return frozen }
	taken := prefixTaken(t, m, frozen, "0001")
	setIDSuffixes(t, "0001", "0002")
	origin := sourceFixture(t, t.TempDir(), "notes.md", "collision notes")
	j, err := m.Enqueue(StartRequest{Title: "sources under final id", Kind: "general", Brief: "read",
		Model: "sonnet", Sources: []string{origin}})
	if err != nil {
		t.Fatal(err)
	}
	if want := frozen.Format("060102-150405") + "-0002"; j.ID != want {
		t.Fatalf("final id %q, want %q", j.ID, want)
	}
	if b, err := os.ReadFile(filepath.Join(m.JobDir(j.ID), SourcesDir, "notes.md")); err != nil || string(b) != "collision notes" {
		t.Fatalf("sources under the final id: %q, %v", b, err)
	}
	// The taken id keeps its row and gets no folder of another job's sources.
	if _, err := os.Stat(m.JobDir(taken)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("taken id %s got a folder: %v", taken, err)
	}
	if got, err := m.Store.Get(taken); err != nil || got.Title != "taken 0001" {
		t.Fatalf("taken row changed: %+v, %v", got, err)
	}
}

// A candidate whose job folder already exists on disk is treated like a taken id: the enqueue redraws
// instead of failing at the folder creation, and never deletes that folder (this attempt did not
// create it). Before the fix this enqueue failed with "create job directory: file exists".
func TestEnqueueRedrawsExistingJobDir(t *testing.T) {
	m := newManager(t)
	frozen := time.Date(2026, 11, 6, 1, 0, 0, 0, time.UTC)
	m.Now = func() time.Time { return frozen }
	taken := prefixTaken(t, m, frozen, "0001")
	// The taken id's own folder, with a file another job must never touch or remove.
	if err := os.MkdirAll(filepath.Join(m.JobDir(taken), SourcesDir), 0o700); err != nil {
		t.Fatal(err)
	}
	foreign := filepath.Join(m.JobDir(taken), SourcesDir, "keep.md")
	if err := os.WriteFile(foreign, []byte("foreign"), 0o400); err != nil {
		t.Fatal(err)
	}
	setIDSuffixes(t, "0001", "0002")
	origin := sourceFixture(t, t.TempDir(), "notes.md", "existing dir notes")
	j, err := m.Enqueue(StartRequest{Title: "redraw past existing dir", Kind: "general", Brief: "read",
		Model: "sonnet", Sources: []string{origin}})
	if err != nil {
		t.Fatal(err)
	}
	if want := frozen.Format("060102-150405") + "-0002"; j.ID != want {
		t.Fatalf("final id %q, want %q", j.ID, want)
	}
	if b, err := os.ReadFile(filepath.Join(m.JobDir(j.ID), SourcesDir, "notes.md")); err != nil || string(b) != "existing dir notes" {
		t.Fatalf("sources under the final id: %q, %v", b, err)
	}
	if b, err := os.ReadFile(foreign); err != nil || string(b) != "foreign" {
		t.Fatalf("taken id's folder changed: %q, %v", b, err)
	}
}

// The codex-capped insert path redraws too, and a collision never counts twice: after one forced
// redraw the cap sees exactly one codex job, and the sources land under the final id (#129). Before
// the fix the redraw left the sources under the taken id.
func TestCodexCapCollisionCountsOnce(t *testing.T) {
	m := newManager(t)
	two := 2
	m.Cfg.CodexJobsPerDay = &two
	frozen := time.Date(2026, 11, 6, 2, 0, 0, 0, time.UTC)
	m.Now = func() time.Time { return frozen }
	taken := prefixTaken(t, m, frozen, "0001")
	setIDSuffixes(t, "0001", "0002")
	origin := sourceFixture(t, t.TempDir(), "notes.md", "codex notes")
	j, err := m.Enqueue(StartRequest{Title: "codex collision", Kind: "review", Brief: "b",
		Model: "gpt-5.5", Sources: []string{origin}})
	if err != nil {
		t.Fatal(err)
	}
	if want := frozen.Format("060102-150405") + "-0002"; j.ID != want {
		t.Fatalf("final id %q, want %q", j.ID, want)
	}
	if got, err := m.CodexJobsToday(); err != nil || got != "1/2" {
		t.Fatalf("cap counted %q, %v; want exactly one codex job", got, err)
	}
	if b, err := os.ReadFile(filepath.Join(m.JobDir(j.ID), SourcesDir, "notes.md")); err != nil || string(b) != "codex notes" {
		t.Fatalf("sources under the final id: %q, %v", b, err)
	}
	if _, err := os.Stat(m.JobDir(taken)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("taken id %s got a folder: %v", taken, err)
	}
}

// The openrouter-capped insert path redraws too, and a collision never reserves the budget twice:
// after one forced redraw the provider's spend holds exactly this job's budget (#129). An openrouter
// job takes no sources (#146), so the sources-under-the-final-id half is the claude and codex tests'.
func TestOpenRouterCapCollisionReservesOnce(t *testing.T) {
	m := newManager(t)
	m.Cfg.Providers.OpenRouter = orConfig(DefaultOpenRouterBaseURL, "/k")
	m.Cfg.Providers.OpenRouter.DailyCapUSD = f64(10)
	frozen := time.Date(2026, 11, 6, 3, 0, 0, 0, time.UTC)
	m.Now = func() time.Time { return frozen }
	taken := prefixTaken(t, m, frozen, "0001")
	setIDSuffixes(t, "0001", "0002")
	j, err := m.Enqueue(StartRequest{Title: "openrouter collision", Kind: "general", Brief: "b",
		Model: "openrouter:" + orGLM, MaxBudgetUSD: 4})
	if err != nil {
		t.Fatal(err)
	}
	if want := frozen.Format("060102-150405") + "-0002"; j.ID != want {
		t.Fatalf("final id %q, want %q", j.ID, want)
	}
	spend, err := m.Store.ProviderSpendSince(m.Agent, store.ProviderOpenRouter, frozen.Add(-time.Hour))
	if err != nil || spend != 4 {
		t.Fatalf("reserved $%.2f, %v; want exactly this job's $4.00", spend, err)
	}
	if _, err := os.Stat(m.JobDir(taken)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("taken id %s got a folder: %v", taken, err)
	}
}

// Retry exhaustion: with every candidate id taken, the enqueue returns the insert's taken-id error
// after idRedraws redraws and leaves no job folder behind (#129).
func TestEnqueueIDExhaustion(t *testing.T) {
	m := newManager(t)
	frozen := time.Date(2026, 11, 6, 4, 0, 0, 0, time.UTC)
	m.Now = func() time.Time { return frozen }
	suffixes := make([]string, idRedraws+1)
	for i := range suffixes {
		suffixes[i] = fmt.Sprintf("%04x", i+1)
		prefixTaken(t, m, frozen, suffixes[i])
	}
	setIDSuffixes(t, suffixes...)
	origin := sourceFixture(t, t.TempDir(), "notes.md", "exhaustion notes")
	_, err := m.Enqueue(StartRequest{Title: "exhausted redraws", Kind: "general", Brief: "b",
		Model: "sonnet", Sources: []string{origin}})
	if !errors.Is(err, store.ErrJobIDExists) {
		t.Fatalf("exhaustion error: %v, want job id already exists", err)
	}
	entries, err := os.ReadDir(filepath.Join(m.StateDir, "jobs"))
	if err != nil || len(entries) != 0 {
		t.Fatalf("left %d folders behind, %v; want none", len(entries), err)
	}
}
