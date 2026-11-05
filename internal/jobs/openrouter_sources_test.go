package jobs

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/qwwiwi/agent-jobs/internal/store"
)

// An openrouter job with file sources (both added on separate branches): the provider and the
// source origins survive the store round trip, the accepted job keeps its copied sources, and a
// job refused by the daily cap leaves no job folder behind.
func TestOpenRouterJobWithSourceFiles(t *testing.T) {
	m := newManager(t)
	m.Cfg.Providers.OpenRouter = orConfig(DefaultOpenRouterBaseURL, "/k")
	m.Cfg.Providers.OpenRouter.DailyCapUSD = f64(5)
	origin := sourceFixture(t, t.TempDir(), "notes.md", "notes")
	j, err := m.Enqueue(StartRequest{Title: "a", Kind: "general", Brief: "read", Model: "openrouter:" + orGLM,
		MaxBudgetUSD: 4, Sources: []string{origin}})
	if err != nil {
		t.Fatal(err)
	}
	got, err := m.Store.Get(j.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Provider != store.ProviderOpenRouter || got.Billing != store.BillingAPI || !slices.Equal(got.SourceFiles, []string{origin}) {
		t.Fatalf("round trip: provider %q billing %q source_files %v", got.Provider, got.Billing, got.SourceFiles)
	}
	if _, err := os.Stat(filepath.Join(m.JobDir(j.ID), SourcesDir, "notes.md")); err != nil {
		t.Fatalf("accepted job lost its sources: %v", err)
	}
	_, err = m.Enqueue(StartRequest{Title: "b", Kind: "general", Brief: "read", Model: "openrouter:" + orGLM,
		MaxBudgetUSD: 2, Sources: []string{origin}})
	if err == nil || !strings.Contains(err.Error(), "openrouter daily cap reached") {
		t.Fatalf("over the cap: %v", err)
	}
	entries, err := os.ReadDir(filepath.Join(m.StateDir, "jobs"))
	if err != nil || len(entries) != 1 {
		t.Fatalf("capped job left a folder: %v, %v", entries, err)
	}
}
