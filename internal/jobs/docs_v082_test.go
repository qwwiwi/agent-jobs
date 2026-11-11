package jobs

import (
	"fmt"
	"os"
	"strings"
	"testing"
)

// The orchestrator prompt and the shipped skill state the daemon's fix-loop cap (v0.8.2 G04, G05) as
// the code enforces it: config review_max_fix_loops with its default, and the two ways past it.
func TestOrchestratorDocsStateFixLoopCap(t *testing.T) {
	for _, name := range []string{"../../prompts/orchestrator.md", "../../skills/agent-jobs/SKILL.md"} {
		b, err := os.ReadFile(name)
		if err != nil {
			t.Fatal(err)
		}
		doc := string(b)
		for _, old := range []string{"cap is yours", "does not count rounds"} {
			if strings.Contains(doc, old) {
				t.Errorf("%s still states the pre-v0.8.2 rule %q", name, old)
			}
		}
		for _, want := range []string{
			fmt.Sprintf("`review_max_fix_loops` (default %d)", DefaultReviewMaxFixLoops),
			"`design`", "`loop_override`",
		} {
			if !strings.Contains(doc, want) {
				t.Errorf("%s lacks %q", name, want)
			}
		}
	}
}

// The shipped skill names what an orchestrator uses of v0.8.2: job_start docs and its allowlist, the
// gate results source, the two card keys and the UI note of the job-done line.
func TestSkillNamesV082Terms(t *testing.T) {
	b, err := os.ReadFile("../../skills/agent-jobs/SKILL.md")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"| `docs` |", "`web_allow_hosts`", "`gate:<id>`", "`review_questions:`", "`ui:`",
		"· ui: true but no ui-check/ evidence for <files>"} {
		if !strings.Contains(string(b), want) {
			t.Errorf("SKILL.md lacks %q", want)
		}
	}
}
