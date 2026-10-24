package checklist_test

import (
	"strings"
	"testing"
	"time"

	"github.com/qwwiwi/agent-jobs/internal/checklist"
	"github.com/qwwiwi/agent-jobs/internal/jobs"
)

// keyLine is one full base64 line of a PEM body; bodyMark is a piece of it no output may keep.
var keyLine = strings.Repeat("Zm9v", 16)

const bodyMark = "Zm9vZm9vZm9v"

// pemFragments are a complete PEM block, one cut off before its END line, an orphan body with its
// END line but no BEGIN, and a markerless body run: each spans lines, which the normalizer collapses.
var pemFragments = map[string]string{
	"complete":   "key -----BEGIN RSA PRIVATE KEY-----\n" + keyLine + "\n" + keyLine + "\n-----END RSA PRIVATE KEY-----\nafter",
	"truncated":  "key\n-----BEGIN OPENSSH PRIVATE KEY-----\n" + keyLine + "\n" + keyLine,
	"orphan":     keyLine + "\n" + keyLine + "\n-----END RSA PRIVATE KEY-----\nafter",
	"markerless": keyLine + "\n" + keyLine + "\n" + keyLine,
}

// Every raw input field goes through the real redactor before it is normalized: a PEM fragment whose
// line breaks the normalizer collapses still never reaches BRIEF.md or CHECKLIST.md.
func TestApplyRedactsRawInputWithRealRedactor(t *testing.T) {
	now := time.Date(2026, 10, 24, 9, 0, 0, 0, time.UTC)
	for name, frag := range pemFragments {
		t.Run(name, func(t *testing.T) {
			brief, list, _, err := checklist.Init("demo", "I want a login page.\n"+frag, jobs.Redact)
			if err != nil {
				t.Fatal(err)
			}
			if strings.Contains(string(brief), bodyMark) {
				t.Fatalf("init kept the key:\n%s", brief)
			}
			f := &checklist.Files{Brief: brief, Checklist: list}
			res, err := checklist.Apply(f, checklist.Change{
				Rows: []checklist.Row{
					{ID: "R01", Verbatim: frag, Contract: "live", Basis: frag},
					{ID: "G01", Verbatim: "dark mode", Contract: "deferred", Basis: frag},
				},
				Additions: []checklist.Addition{{Quote: frag}},
			}, now, jobs.Redact)
			if err != nil {
				t.Fatal(err)
			}
			for what, b := range map[string][]byte{"BRIEF.md": res.BriefAfter, "CHECKLIST.md": res.Checklist} {
				if strings.Contains(string(b), bodyMark) || !strings.Contains(string(b), "[redacted") {
					t.Fatalf("%s kept the key or redacted nothing:\n%s", what, b)
				}
			}
			for _, r := range res.List.Rows {
				if strings.Contains(r.Verbatim+r.Basis, bodyMark) {
					t.Fatalf("row %s kept the key: %+v", r.ID, r)
				}
			}
		})
	}
}

// A secret in a cell that does not parse (an unknown contract word, a bad ID) is redacted before the
// error names the cell.
func TestApplyRedactsBeforeErrors(t *testing.T) {
	brief, list, _, err := checklist.Init("demo", "words", jobs.Redact)
	if err != nil {
		t.Fatal(err)
	}
	tok := "ghp_" + strings.Repeat("Q", 36)
	for _, r := range []checklist.Row{
		{ID: "R01", Verbatim: "x", Contract: "live " + tok},
		{ID: "R01 " + tok, Verbatim: "x", Contract: "live"},
	} {
		_, err := checklist.Apply(&checklist.Files{Brief: brief, Checklist: list}, checklist.Change{Rows: []checklist.Row{r}},
			time.Now(), jobs.Redact)
		if err == nil || strings.Contains(err.Error(), tok) {
			t.Fatalf("row %+v: err %v", r, err)
		}
	}
}
