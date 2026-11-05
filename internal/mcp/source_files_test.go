package mcp

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/qwwiwi/agent-jobs/internal/jobs"
)

func TestJobStartFileSources(t *testing.T) {
	m := newManager(t)
	cs := connect(t, mustServer(t, m))
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	origin := filepath.Join(dir, "report.md")
	if err := os.WriteFile(origin, []byte("review evidence"), 0o600); err != nil {
		t.Fatal(err)
	}
	var started JobView
	call(t, cs, "job_start", startArgs(func(a map[string]any) { a["sources"] = []any{origin} }), &started)
	if !slices.Equal(started.Sources, []string{origin}) {
		t.Fatalf("job_start sources: %v", started.Sources)
	}
	b, err := os.ReadFile(filepath.Join(m.JobDir(started.ID), "sources", "report.md"))
	if err != nil || string(b) != "review evidence" {
		t.Fatalf("synchronous MCP snapshot: %q, %v", b, err)
	}
	var status JobView
	call(t, cs, "job_status", map[string]any{"id": started.ID}, &status)
	if !slices.Equal(status.Sources, started.Sources) {
		t.Fatalf("job_status origins: %v", status.Sources)
	}
	j, err := m.Store.Get(started.ID)
	if err != nil {
		t.Fatal(err)
	}
	prompt, err := m.BuildPrompt(j, "", nil, jobs.MemoryNotes{})
	if err != nil || !strings.Contains(prompt, "sources/report.md – from "+origin) {
		t.Fatalf("MCP prompt listing: %v", err)
	}
	msg := callErr(t, cs, "job_start", startArgs(func(a map[string]any) {
		a["sources"] = []any{filepath.Join(dir, "missing.md")}
	}))
	if !strings.Contains(msg, "sources") || !strings.Contains(msg, "no such file") {
		t.Fatalf("missing source must reject job_start: %s", msg)
	}
}
