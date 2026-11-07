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

func idsOf(t *testing.T, section string) (string, error) {
	t.Helper()
	got, err := ParseBlocking("## Summary\nVerdict: BLOCK\n\n## Findings / Changes\n\n### Blocking\n" + section + "\n### Non-blocking\nnone\n\n## Evidence\nx\n")
	if err != nil {
		return "", err
	}
	var out []string
	for _, f := range got {
		out = append(out, f.ID)
	}
	return strings.Join(out, ","), nil
}

// H1 (#197 fix loop 1): every declared blocking ID survives, wherever it stands in its item.
func TestParseBlockingKeepsEveryDeclaredID(t *testing.T) {
	cases := map[string]struct{ section, want string }{
		"two ids in one bullet":       {"- H1: first problem; H2: second problem\n", "H1,H2"},
		"three ids with commas":       {"- H1: one, H2: two, H3: three\n", "H1,H2,H3"},
		"id on a wrapped line":        {"- H1: first problem that wraps\n  H2: second problem\n", "H1,H2"},
		"id after a wrapped location": {"- `internal/x.go:12` —\n  H2: late id\n", "H2"},
		"wrapped id at column zero":   {"- H1: first\nH2: second\n", "H1,H2"},
		"location severity then id":   {"- internal/x.go:12 — High: H2 the problem\n- internal/y.go:3 — **Critical** — H3: other\n", "H2,H3"},
		"backticked location":         {"- `internal/x.go:12` — High: H2 the problem\n", "H2"},
		"paragraph location severity": {"\ninternal/x.go:12 — High: H2 the problem\nwraps here\n\ninternal/y.go:3 — Medium: H3 other\n", "H2,H3"},
		"mention of another id":       {"- H1: breaks the contract that H2 relies on\n", "H1"},
		"id range is not two ids":     {"- H1: same cause as H2-H3 below\n", "H1"},
		"second id on sub-bullet":     {"- H1: first\n  - H2: nested\n", "H1,H2"},
		"same id twice in one item":   {"- H1: first; see H1: again\n", "H1"},
	}
	for name, c := range cases {
		got, err := idsOf(t, c.section)
		if err != nil {
			t.Errorf("%s: %v", name, err)
			continue
		}
		if got != c.want {
			t.Errorf("%s: got %q, want %q", name, got, c.want)
		}
	}
}

// An item whose IDs cannot be told apart is refused with the line named, not accepted as a subset.
func TestParseBlockingAmbiguousItemIsRefused(t *testing.T) {
	cases := map[string]struct{ section, line string }{
		"id in mid-sentence declaration": {"- H1 first problem and also H2: the second one\n", "H2: the second one"},
		"id with dash mid sentence":      {"- H1: a thing, so we need H3 — the other thing\n", "H3 — the other thing"},
	}
	for name, c := range cases {
		_, err := idsOf(t, c.section)
		if err == nil || !strings.Contains(err.Error(), c.line) {
			t.Errorf("%s: want an error naming %q, got %v", name, c.line, err)
		}
	}
}

// H4 (#197 fix loop 1): a finding's body is its complete detailed entry elsewhere in the report, plus the
// Blocking line. Fixture: the real layout of docs/benchmarks/2026-11-06/reports/M-154-opus-high-review.md.
func TestParseBlockingResolvesDetailedEntryM154(t *testing.T) {
	got, err := ParseBlocking(readFixtureFile(t, "review-m154.md"))
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].ID != "H1" {
		t.Fatalf("findings: %+v", got)
	}
	body := got[0].Body
	for _, want := range []string{
		"internal/jobs/wrapper.go:133 — H1: Report-preservation condition remains open", // the Blocking line
		"Report detection can still lose the completed report",                          // the entry's heading
		"`isReport`, which only checks a case-sensitive",
		"Two verified consequences of this same open condition",
		"Summary-only notification also replaces the report",
		"**Minimal fix:** Select report candidates using the existing section parser",
		"**Verification:**", "**Brief coverage:**",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("H1 body lacks %q:\n%s", want, body)
		}
	}
	for _, not := range []string{"Closed:", "Evidence", "git diff --check", "Non-blocking"} {
		if strings.Contains(body, not) {
			t.Errorf("H1 body took unrelated text %q:\n%s", not, body)
		}
	}
	if first, _, _ := strings.Cut(body, "\n"); !strings.HasPrefix(first, "internal/jobs/wrapper.go:133") {
		t.Errorf("the Blocking line is not first: %q", first)
	}
}

// The same, for the review report that found these defects: four detailed entries under ### Code, each
// reached from its one-line Blocking item, none leaking into its neighbour.
func TestParseBlockingResolvesDetailedEntriesChain1(t *testing.T) {
	got, err := ParseBlocking(readFixtureFile(t, "review-chain1.md"))
	if err != nil {
		t.Fatal(err)
	}
	var ids []string
	byID := map[string]string{}
	for _, f := range got {
		ids = append(ids, f.ID)
		byID[f.ID] = f.Body
	}
	if strings.Join(ids, ",") != "H1,H2,H3,H4" {
		t.Fatalf("ids %v", ids)
	}
	want := map[string][]string{
		"H1": {"preserve every declared blocking ID", "only H1 becomes a tracked finding", "Extract all declared IDs across the item"},
		"H2": {"make child creation/linking recoverably idempotent", "A crash after `Enqueue` succeeds", "recover existing children on retry"},
		"H3": {"qualify the published branch ref", "shares its name with an older tag", "Add collision coverage"},
		"H4": {"include complete finding bodies", "puts H1’s summary under Blocking", "test this actual report layout"},
	}
	for id, parts := range want {
		for _, p := range parts {
			if !strings.Contains(byID[id], p) {
				t.Errorf("%s body lacks %q:\n%s", id, p, byID[id])
			}
		}
	}
	if strings.Contains(byID["H1"], "A crash after `Enqueue`") || strings.Contains(byID["H2"], "shares its name with an older tag") {
		t.Error("an entry runs into the next finding")
	}
	for id, b := range byID {
		for _, not := range []string{"Item 6: partly met", "Non-blocking", "Evidence"} {
			if strings.Contains(b, not) {
				t.Errorf("%s body took unrelated text %q:\n%s", id, not, b)
			}
		}
	}
}

// A finding with no detailed entry keeps its Blocking item as the whole body (nothing is invented).
func TestParseBlockingWithoutDetailedEntry(t *testing.T) {
	got, err := ParseBlocking(readFixtureFile(t, "review-blocking.md"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(got[0].Body, "H2") || strings.Contains(got[1].Body, "Reproduced with") {
		t.Errorf("bodies crossed:\n%s\n---\n%s", got[0].Body, got[1].Body)
	}
}
