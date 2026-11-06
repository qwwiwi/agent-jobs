package jobs

import (
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/qwwiwi/agent-jobs/internal/store"
)

// An openrouter job with file sources (both added on separate branches) is refused before any
// snapshot is taken (#146): no job folder, no row, no budget reserved, even when the daily cap
// would refuse it too.
func TestOpenRouterJobWithSourceFiles(t *testing.T) {
	m := newManager(t)
	m.Cfg.Providers.OpenRouter = orConfig(DefaultOpenRouterBaseURL, "/k")
	m.Cfg.Providers.OpenRouter.DailyCapUSD = f64(5)
	origin := sourceFixture(t, t.TempDir(), "notes.md", "notes")
	for _, budget := range []float64{4, 6} { // within the cap, and past it
		_, err := m.Enqueue(StartRequest{Title: "a", Kind: "general", Brief: "read", Model: "openrouter:" + orGLM,
			MaxBudgetUSD: budget, Sources: []string{origin}})
		if err == nil || !strings.Contains(err.Error(), "openrouter jobs take no sources") {
			t.Fatalf("budget $%.0f: %v, want the sources refusal", budget, err)
		}
	}
	if entries, err := os.ReadDir(filepath.Join(m.StateDir, "jobs")); err == nil && len(entries) > 0 {
		t.Fatalf("a refused job left a folder: %v", entries)
	} else if err != nil && !errors.Is(err, os.ErrNotExist) {
		t.Fatal(err)
	}
	if spend, err := m.Store.ProviderSpendSince(m.Agent, store.ProviderOpenRouter, time.Now().Add(-time.Hour)); err != nil || spend != 0 {
		t.Fatalf("a refused job reserved $%.2f, %v", spend, err)
	}
}

// A job queued with sources before #146 (or a row changed since) is refused again at launch: the
// wrapper never starts an openrouter job whose row carries sources.
func TestWrapperOpenRouterRefusesQueuedSources(t *testing.T) {
	jobDir := filepath.Join(t.TempDir(), "jobs", "261106-000000-0001")
	for name, j := range map[string]*store.Job{
		"source files":   {ID: "f", Model: "openrouter:" + orGLM, Kind: "general", SourceFiles: []string{"/etc/hostname"}},
		"legacy sources": {ID: "g", Model: "openrouter:" + orGLM, Kind: "general", Sources: []string{"https://github.com/a/b"}},
	} {
		orj, reason := wrapperOpenRouter(jobDir, j, io.Discard)
		if orj != nil || !strings.Contains(reason, "openrouter jobs take no sources") {
			t.Errorf("%s: started %v, reason %q", name, orj != nil, reason)
		}
	}
}
