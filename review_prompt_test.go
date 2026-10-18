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
		"`## Blocking`", "`## Non-blocking`", "file:line",
		"`Verdict: SHIP`", "`Verdict: SHIP WITH FIXES`", "`Verdict: BLOCK`",
		"BLOCK if and only if Blocking is not empty",
	} {
		if !strings.Contains(s, want) {
			t.Errorf("kind-review.md lacks %q", want)
		}
	}
	// Risky areas come before the three questions; Blocking before Non-blocking.
	if strings.Index(s, "Risky areas first") > strings.Index(s, "Delivered") {
		t.Error("risky areas must be listed before the three questions")
	}
	if strings.Index(s, "## Blocking") > strings.Index(s, "## Non-blocking") {
		t.Error("Blocking must come before Non-blocking")
	}
	// Short: a checklist, not an essay.
	if n := strings.Count(s, "\n"); n > 20 {
		t.Errorf("kind-review.md has %d lines; keep it short", n)
	}
}

// The extra review sections sit inside the report contract: Not done / uncertain stays last.
func TestReviewPromptKeepsContractOrder(t *testing.T) {
	b, err := Templates.ReadFile("prompts/kind-review.md")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(b), "before Evidence") {
		t.Error("kind-review.md must place Blocking / Non-blocking before Evidence (Not done / uncertain stays last)")
	}
}
