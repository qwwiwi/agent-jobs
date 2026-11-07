package jobs

import (
	"errors"
	"os"
	"strings"
	"testing"
)

func readFixtureFile(t *testing.T, name string) string {
	t.Helper()
	b, err := os.ReadFile("testdata/" + name)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// A real review report: every blocking ID arrives with its whole body, and nothing from the
// Non-blocking section leaks in. A fenced line that looks like a heading does not end the section.
func TestParseBlockingFindings(t *testing.T) {
	report := readFixtureFile(t, "review-blocking.md")
	got, err := ParseBlocking(report)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0].ID != "H1" || got[1].ID != "H2" {
		t.Fatalf("findings: %+v", got)
	}
	for _, want := range []string{"truncates `events`", "Reproduced with", "validate first, then truncate", "this fenced line is body text"} {
		if !strings.Contains(got[0].Body, want) {
			t.Errorf("H1 body lacks %q:\n%s", want, got[0].Body)
		}
	}
	for _, want := range []string{"advisory lock is taken after the read", "sub-point: the lock file", "take the lock before reading"} {
		if !strings.Contains(got[1].Body, want) {
			t.Errorf("H2 body lacks %q:\n%s", want, got[1].Body)
		}
	}
	if strings.Contains(got[1].Body, "M1") || strings.Contains(got[1].Body, "typo") {
		t.Errorf("H2 body took the Non-blocking section:\n%s", got[1].Body)
	}
	if v := ReviewVerdict(report); v != "BLOCK" {
		t.Errorf("verdict %q", v)
	}
}

// SHIP with «none»: nothing to fix, said as such.
func TestParseBlockingShipIsNothingToFix(t *testing.T) {
	report := readFixtureFile(t, "review-ship.md")
	if _, err := ParseBlocking(report); !errors.Is(err, ErrNothingToFix) {
		t.Fatalf("want ErrNothingToFix, got %v", err)
	}
	if v := ReviewVerdict(report); v != "SHIP" {
		t.Errorf("verdict %q", v)
	}
}

func TestParseBlockingShapes(t *testing.T) {
	wrap := func(section string) string {
		return "## Summary\nVerdict: BLOCK\n\n## Findings / Changes\n\n### Blocking\n" + section + "\n### Non-blocking\nnone\n\n## Evidence\nx\n"
	}
	ids := func(s string) string {
		got, err := ParseBlocking(s)
		if err != nil {
			return "error: " + err.Error()
		}
		var out []string
		for _, f := range got {
			out = append(out, f.ID)
		}
		return strings.Join(out, ",")
	}
	cases := map[string]struct{ report, want string }{
		"id first":            {wrap("- F1: first\n- F2. second\n- **C1** third\n- [M1] fourth\n"), "F1,F2,C1,M1"},
		"location then id":    {wrap("- `a.go:1` — H1: one\n- b.go:2 — H2: two\n- `c.go:3`: open H3 — three\n"), "H1,H2,H3"},
		"paragraph items":     {wrap("\na.go:1 — H1: one\nmore of one\n\nb.go:2 — H2: two\n"), "H1,H2"},
		"numbered":            {wrap("1. H1: one\n2. H2: two\n"), "H1,H2"},
		"heading in a fence":  {wrap("- H1: one\n```\n## Evidence\n```\n"), "H1"},
		"missing section":     {"## Summary\nVerdict: BLOCK\n", "error: " + ErrNoBlockingSection.Error()},
		"bullet without id":   {wrap("- `a.go:1` — H1: one\n- `b.go:2` — no id here\n"), "error:"},
		"no id at all":        {wrap("- `a.go:1` — first\n- `b.go:2` — second\n"), "error:"},
		"duplicate id":        {wrap("- H1: one\n- H1: again\n"), "error:"},
		"none with markup":    {wrap("**None.**\n"), "error: " + ErrNothingToFix.Error()},
		"empty section":       {wrap(""), "error: " + ErrNothingToFix.Error()},
		"lead-in has no id":   {wrap("The following must be fixed:\n- H1: one\n"), "error:"},
		"lowercase not an id": {wrap("- h1: one\n"), "error:"},
	}
	for name, c := range cases {
		got := ids(c.report)
		if strings.HasSuffix(c.want, "error:") {
			if !strings.HasPrefix(got, "error:") {
				t.Errorf("%s: got %q, want an error", name, got)
			}
			continue
		}
		if got != c.want {
			t.Errorf("%s: got %q, want %q", name, got, c.want)
		}
	}
	// The error for a bullet without an ID names the line, so the orchestrator can see what was lost.
	_, err := ParseBlocking(wrap("- `a.go:1` — H1: one\n- `b.go:2` — no id here\n"))
	if err == nil || !strings.Contains(err.Error(), "no id here") {
		t.Errorf("error does not name the line: %v", err)
	}
}
