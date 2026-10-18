package jobs

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestMeasureBrief(t *testing.T) {
	b := "## Task\nFix `internal/jobs/finish.go:120` and docs/PLAN.md, e.g. the README.md (see https://example.com/a/b.md).\n" +
		"Also finish.go again, i.e. nothing else; version 1.5 and v2.0 are not files.\n## Done\n### sub\n"
	s := MeasureBrief(b)
	if s.Bytes != len(b) || s.Headings != 2 || s.Files != 4 { // finish.go:120 path, PLAN.md, README.md, finish.go
		t.Fatalf("measure: %+v", s)
	}
}

func TestBriefSizeCapAndWarnings(t *testing.T) {
	var c Config
	if err := c.checkBriefSize(strings.Repeat("x", DefaultMaxBriefBytes)); err != nil {
		t.Fatalf("at the cap: %v", err)
	}
	err := c.checkBriefSize(strings.Repeat("x", DefaultMaxBriefBytes+1))
	if err == nil || !strings.Contains(err.Error(), "max_brief_bytes") || !strings.Contains(err.Error(), "32768") {
		t.Fatalf("above the cap: %v", err)
	}
	c.MaxBriefBytes = 10
	if err := c.checkBriefSize("12345678901"); err == nil {
		t.Fatal("configured cap not applied")
	}

	var files, heads strings.Builder
	for i := 0; i <= DefaultBriefWarnFiles; i++ {
		fmt.Fprintf(&files, "pkg/f%d.go\n", i)
	}
	for i := 0; i <= DefaultBriefWarnHeadings; i++ {
		fmt.Fprintf(&heads, "## Part %d\n", i)
	}
	var d Config
	if w := d.BriefSizeWarning("fix a.go and b.go\n## One\n"); w != "" {
		t.Fatalf("small brief warned: %q", w)
	}
	if w := d.BriefSizeWarning(files.String()); !strings.Contains(w, "names 41 files") || !strings.Contains(w, "brief_warn_files") {
		t.Fatalf("files warning: %q", w)
	}
	if w := d.BriefSizeWarning(heads.String()); !strings.Contains(w, "9 \"## \" headings") || !strings.Contains(w, "more than one deliverable") {
		t.Fatalf("headings warning: %q", w)
	}
	d.BriefWarnFiles, d.BriefWarnHeadings = 100, 100
	if w := d.BriefSizeWarning(files.String() + heads.String()); w != "" {
		t.Fatalf("configured thresholds not applied: %q", w)
	}
}

// job_start (Enqueue) refuses a brief above the cap; nothing is queued.
func TestEnqueueRefusesOversizedBrief(t *testing.T) {
	m := newManager(t)
	_, err := m.Enqueue(StartRequest{Title: "big", Kind: "general", Model: "sonnet", Brief: strings.Repeat("x", DefaultMaxBriefBytes+1)})
	if err == nil || !strings.Contains(err.Error(), "above the cap") {
		t.Fatalf("oversized brief: %v", err)
	}
	if l, _ := m.Store.List(5); len(l) != 0 {
		t.Fatalf("queued %d jobs", len(l))
	}
}

func TestConfigBriefKeys(t *testing.T) {
	dir := t.TempDir()
	write := func(s string) {
		if err := os.WriteFile(filepath.Join(dir, "config.json"), []byte(s), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	write(`{"max_brief_bytes": 1000, "brief_warn_files": 5, "brief_warn_headings": 3, "keep_raw_reports": true}`)
	c, err := LoadConfig(dir)
	if err != nil || c.maxBriefBytes() != 1000 || c.briefWarnFiles() != 5 || c.briefWarnHeadings() != 3 || !c.KeepRawReports {
		t.Fatalf("config: %+v %v", c, err)
	}
	write(`{"max_brief_bytes": -1}`)
	if _, err := LoadConfig(dir); err == nil {
		t.Fatal("negative max_brief_bytes accepted")
	}
	write(`{}`)
	if c, err := LoadConfig(dir); err != nil || c.KeepRawReports || c.maxBriefBytes() != DefaultMaxBriefBytes {
		t.Fatalf("defaults: %+v %v", c, err)
	}
}

// Single-character stems are files ("x.go", "a.go:12", "pkg/x.ts"); "e.g" and "i.e" are not.
func TestMeasureBriefShortStems(t *testing.T) {
	s := MeasureBrief("Fix x.go and a.go:12, then pkg/x.ts. E.g. this, e.g. that, i.e. nothing; also E.G and I.E.\n")
	if s.Files != 3 {
		t.Fatalf("files: %+v", s)
	}
}
