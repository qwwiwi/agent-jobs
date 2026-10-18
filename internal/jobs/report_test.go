package jobs

import (
	"strings"
	"testing"

	agentjobs "github.com/qwwiwi/agent-jobs"
)

const sampleReport = "## Summary\nok\n\n## Findings / Changes\n- a.go:1\n\n## Evidence\n```\n## not a heading\n```\n\n" +
	"## Not done / uncertain\nnothing\n\n## Progress\n- 10:00 UTC read the code\n- 10:05 UTC wrote the test\n"

// The brief asks for a Progress section with timestamped one-line steps, after Not done / uncertain.
func TestReportContractAsksForProgress(t *testing.T) {
	b, err := agentjobs.Templates.ReadFile("prompts/report-contract.md")
	if err != nil {
		t.Fatal(err)
	}
	c := string(b)
	nd, pr := strings.Index(c, "## Not done / uncertain"), strings.Index(c, "## Progress")
	if nd < 0 || pr < nd || !strings.Contains(c, "HH:MM UTC") || !strings.Contains(c, "one line per step") ||
		!strings.Contains(c, "is mandatory and is read first") {
		t.Fatalf("report contract:\n%s", c)
	}
}

func TestReportSection(t *testing.T) {
	if s, ok := ReportSection(sampleReport, SectionProgress); !ok || s != "- 10:00 UTC read the code\n- 10:05 UTC wrote the test" {
		t.Fatalf("progress: %q %v", s, ok)
	}
	if s, ok := ReportSection(sampleReport, "not done / UNCERTAIN"); !ok || s != "nothing" {
		t.Fatalf("not done: %q %v", s, ok)
	}
	if s, ok := ReportSection(sampleReport, "Evidence"); !ok || !strings.Contains(s, "## not a heading") {
		t.Fatalf("a heading in a fence split the section: %q", s)
	}
	// A missing section is not an error.
	if s, ok := ReportSection("## Summary\nok\n", SectionProgress); ok || s != "" {
		t.Fatalf("missing progress: %q %v", s, ok)
	}
}

func TestReadFirst(t *testing.T) {
	out := ReadFirst(sampleReport)
	nd, pr, su := strings.Index(out, "## Not done"), strings.Index(out, "## Progress"), strings.Index(out, "## Summary")
	if nd != 0 || pr < nd || su < pr || !strings.Contains(out, "## not a heading") || !strings.Contains(out, "- a.go:1") {
		t.Fatalf("read first:\n%s", out)
	}
	if plain := "no sections at all\n"; ReadFirst(plain) != plain {
		t.Fatal("a report without sections must stay as is")
	}
}
