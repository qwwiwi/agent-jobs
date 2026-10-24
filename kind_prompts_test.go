package agentjobs

import (
	"strings"
	"testing"
)

// F10: the fix and general checklists keep their reference rows first and add the same executor rules:
// a REDPROOF line per new test, visible placeholders instead of made-up facts, the Requirements: commit
// trailer, and a BLOCKED: line in Not done / uncertain for a part the brief does not allow.
func TestWriteKindPromptsExecutorRules(t *testing.T) {
	first := map[string][]string{
		"prompts/kind-fix.md": {"- First reproduce the problem with a failing test.",
			"- Then make the minimal change, run the tests, and create a local commit."},
		"prompts/kind-general.md": {"- Before starting, state what counts as done.", "- At the end, prove it is done."},
	}
	for name, ref := range first {
		b, err := Templates.ReadFile(name)
		if err != nil {
			t.Fatal(err)
		}
		s := string(b)
		lines := strings.Split(s, "\n")
		// The reference rows stay first, on lines 3 and 4 (CONFORMANCE cites them by line).
		if len(lines) < 4 || lines[2] != ref[0] || lines[3] != ref[1] {
			t.Errorf("%s: reference rows not kept on lines 3-4", name)
		}
		for _, want := range []string{
			"Red proof:", "`REDPROOF` line in Evidence", "failed before your change", "`REDPROOF R01 · ...`",
			"Do not make up a fact", "visible placeholder", "`<<PLACEHOLDER: what is missing>>`", "list each one in Not done / uncertain",
			"`Requirements: R01, R03i`", "end each commit message with a trailer",
			"`BLOCKED: <requirement or item> · <why>`", "is `missing` or `partial` in the `### Requirements` block",
		} {
			if !strings.Contains(s, want) {
				t.Errorf("%s lacks %q", name, want)
			}
		}
		if strings.Contains(s, "`## ") {
			t.Errorf("%s asks for a top-level (##) report section beyond the four of the report contract", name)
		}
	}
	// The executor rules are the same text in both checklists.
	fix, _ := Templates.ReadFile("prompts/kind-fix.md")
	gen, _ := Templates.ReadFile("prompts/kind-general.md")
	tail := func(b []byte) string { return strings.Join(strings.Split(string(b), "\n")[4:], "\n") }
	if tail(fix) != tail(gen) {
		t.Error("kind-fix.md and kind-general.md differ after their reference rows")
	}
}
