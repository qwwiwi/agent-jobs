package agentjobs

import (
	"strings"
	"testing"
)

// The review checklist is risky-first: risky areas listed and reviewed first, three separate
// questions, Blocking / Non-blocking sections and a verdict tied to Blocking.
func TestReviewPromptRiskyFirst(t *testing.T) {
	b, err := Templates.ReadFile("prompts/kind-review.md")
	if err != nil {
		t.Fatal(err)
	}
	s := string(b)
	for _, want := range []string{
		"Risky areas first", "auth/permissions", "money", "data deletion or migrations", "secrets",
		"guard/sandbox", "concurrency", "external sends", "first and deepest",
		"Delivered", "Requirement by requirement", "Design", "Code",
		"`### Blocking`", "`### Non-blocking`", "file:line",
		"`Verdict: SHIP`", "`Verdict: SHIP WITH FIXES`", "`Verdict: BLOCK`",
		"BLOCK if and only if Blocking is not empty", "read-only", "shell heredoc",
	} {
		if !strings.Contains(s, want) {
			t.Errorf("kind-review.md lacks %q", want)
		}
	}
	// Risky areas come before the three questions; Blocking before Non-blocking.
	if strings.Index(s, "Risky areas first") > strings.Index(s, "Delivered") {
		t.Error("risky areas must be listed before the three questions")
	}
	if strings.Index(s, "### Blocking") > strings.Index(s, "### Non-blocking") {
		t.Error("Blocking must come before Non-blocking")
	}
	// Short: a checklist, not an essay.
	if n := strings.Count(s, "\n"); n > 20 {
		t.Errorf("kind-review.md has %d lines; keep it short", n)
	}
}

// The extra review sections sit inside the report contract: Blocking / Non-blocking are subsections
// of Findings / Changes, so a review report still has exactly the four top-level sections of
// report-contract.md, and the checklist adds no top-level (##) section of its own.
func TestReviewPromptKeepsContractOrder(t *testing.T) {
	b, err := Templates.ReadFile("prompts/kind-review.md")
	if err != nil {
		t.Fatal(err)
	}
	s := string(b)
	if !strings.Contains(s, "End Findings / Changes with two subsections") {
		t.Error("kind-review.md must put Blocking / Non-blocking inside Findings / Changes")
	}
	if strings.Contains(s, "`## ") {
		t.Error("kind-review.md asks for a top-level (##) report section beyond the four of the report contract")
	}
	c, err := Templates.ReadFile("prompts/report-contract.md")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(c), "exactly these four sections") {
		t.Error("report-contract.md no longer states the four-section contract this test relies on")
	}
}

// F10: for each finding the reviewer asks whether the executor's brief let it see the problem (if not,
// a brief gap), and a review of a fix looks only at the fix diff and the conditions it must close, a
// condition still defeatable another way staying open, never a new finding.
func TestReviewPromptBriefGapAndFixReview(t *testing.T) {
	b, err := Templates.ReadFile("prompts/kind-review.md")
	if err != nil {
		t.Fatal(err)
	}
	s := string(b)
	for _, want := range []string{
		"Brief gap: for each finding", "executor's brief gave it a way to see the problem",
		"`brief gap: <what the brief should have said>`",
		"Fix review:", "look only at the fix diff and the conditions the fix has to close",
		"`closed` or `open`", "can still be defeated another way stays `open`", "do not file that as a new finding",
		"Each `open` condition is a Blocking line",
	} {
		if !strings.Contains(s, want) {
			t.Errorf("kind-review.md lacks %q", want)
		}
	}
	// Both rules come before the Blocking / Non-blocking subsections they feed.
	if i := strings.Index(s, "### Blocking"); strings.Index(s, "Brief gap:") > i || strings.Index(s, "Fix review:") > i {
		t.Error("brief gap and fix review must come before the Blocking subsection")
	}
}
