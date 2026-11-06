package jobs

import (
	"strings"
	"testing"
	"time"

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

// Section parsing is linear: a report of 200k short lines in one section parses well under a second.
func TestReportSectionLinear(t *testing.T) {
	big := "## Summary\nok\n## Evidence\n" + strings.Repeat("- line\n", 200000) + "## Progress\n- 10:00 UTC done\n"
	start := time.Now()
	if s, ok := ReportSection(big, SectionProgress); !ok || s != "- 10:00 UTC done" {
		t.Fatalf("progress: %q %v", s, ok)
	}
	if out := ReadFirst(big); !strings.HasPrefix(out, "## Progress") {
		t.Fatalf("read first: %q", out[:40])
	}
	if d := time.Since(start); d > time.Second {
		t.Fatalf("parsing took %v", d)
	}
}

func TestReportSections(t *testing.T) {
	got := ReportSections(sampleReport, SectionNotDone, SectionProgress, "Missing")
	if len(got) != 3 || got[0] != "nothing" || !strings.HasPrefix(got[1], "- 10:00 UTC") || got[2] != "" {
		t.Fatalf("sections: %q", got)
	}
}

const forksReport = "## Summary\nok\n\n## Not done / uncertain\nnothing\n\n## Forks\n" +
	"- polling / inotify / the daemon already polls elsewhere\n- field as text / parsed list / consumers want the raw lines\n\n" +
	"## Progress\n- 10:00 UTC done\n"

// Forks is a section of its own: found by name next to Not done and Progress, absent from a report
// without it (not an error), "none" kept as written, and a heading-looking line in a fence is no section.
func TestReportSectionForks(t *testing.T) {
	want := "- polling / inotify / the daemon already polls elsewhere\n- field as text / parsed list / consumers want the raw lines"
	if s, ok := ReportSection(forksReport, SectionForks); !ok || s != want {
		t.Fatalf("forks: %q %v", s, ok)
	}
	got := ReportSections(forksReport, SectionNotDone, SectionForks, SectionProgress)
	if got[0] != "nothing" || got[1] != want || got[2] != "- 10:00 UTC done" {
		t.Fatalf("sections: %q", got)
	}
	if s, ok := ReportSection(sampleReport, SectionForks); ok || s != "" {
		t.Fatalf("report without forks: %q %v", s, ok)
	}
	if s, ok := ReportSection("## Summary\nok\n\n## forks\nnone\n", SectionForks); !ok || s != "none" {
		t.Fatalf("none / heading case: %q %v", s, ok)
	}
	fenced := "## Summary\nok\n\n## Evidence\n```\n## Forks\n- fake / fake / fake\n```\n\n## Not done / uncertain\nnothing\n"
	if s, ok := ReportSection(fenced, SectionForks); ok {
		t.Fatalf("a fenced heading counted as the Forks section: %q", s)
	}
	// Forks does not swallow the section after it, and ReadFirst keeps it whole.
	if out := ReadFirst(forksReport); !strings.Contains(out, "## Forks\n- polling") || strings.Index(out, "## Not done") != 0 {
		t.Fatalf("read first:\n%s", out)
	}
}

// The contract asks for a Forks section between Not done / uncertain and Progress: up to 5 entries of
// "choice / rejected alternative / reason", `none` allowed; the four-section rule stays.
func TestReportContractAsksForForks(t *testing.T) {
	b, err := agentjobs.Templates.ReadFile("prompts/report-contract.md")
	if err != nil {
		t.Fatal(err)
	}
	c := string(b)
	nd, fk, pr := strings.Index(c, "## Not done / uncertain"), strings.Index(c, "## Forks"), strings.Index(c, "## Progress")
	if !(nd >= 0 && fk > nd && pr > fk) {
		t.Fatalf("Forks must sit between Not done / uncertain and Progress:\n%s", c)
	}
	for _, want := range []string{"exactly these four sections", "at most 5", "choice / rejected alternative / reason", "`none`"} {
		if !strings.Contains(c, want) {
			t.Errorf("report-contract.md lacks %q", want)
		}
	}
}

func TestCapText(t *testing.T) {
	if got, cut := CapText("short", 10); got != "short" || cut {
		t.Fatalf("fits: %q %v", got, cut)
	}
	if got, cut := CapText("abcdefghij", 4); got != "abcd\n[cut: 4 of 10 bytes shown]" || !cut {
		t.Fatalf("cut: %q %v", got, cut)
	}
	// 3 bytes into a 2-byte rune: the cut steps back to the rune start.
	if got, _ := CapText("жжж", 3); got != "ж\n[cut: 2 of 6 bytes shown]" {
		t.Fatalf("rune: %q", got)
	}
}

func TestReportSectionTextsAndSizes(t *testing.T) {
	report := "## Summary\nok\n\n## Evidence\n```\n## Forks\n```\nlog\n\n## Not done / uncertain\nnothing\n"
	text, missing := ReportSectionTexts(report, "evidence", "## Not done / uncertain", "Forks")
	if text != "## Evidence\n```\n## Forks\n```\nlog\n\n## Not done / uncertain\nnothing\n" || len(missing) != 1 || missing[0] != "Forks" {
		t.Fatalf("texts %q missing %v", text, missing)
	}
	sizes := ReportSectionSizes(report)
	if len(sizes) != 3 || sizes[0] != (SectionSize{Name: "Summary", Bytes: len("## Summary\nok\n\n")}) || sizes[2].Name != "Not done / uncertain" {
		t.Fatalf("sizes %+v", sizes)
	}
	if got := ReportSectionSizes("no headings"); len(got) != 0 {
		t.Fatalf("no sections: %+v", got)
	}
}

func TestDigestReport(t *testing.T) {
	report := "## Summary\nok\n\n## Not done / uncertain\n" + strings.Repeat("b", 100) + "\n"
	d := DigestReport(report, 10)
	if d.Summary != "ok" || d.Forks != "" || !strings.HasPrefix(d.NotDone, strings.Repeat("b", 10)+"\n[cut: 10 of 100") ||
		len(d.Cut) != 1 || d.Cut[0] != SectionNotDone || d.Bytes != len(report) {
		t.Fatalf("digest %+v", d)
	}
	if h := d.SizesHint(); !strings.Contains(h, "Summary 15 B") || !strings.Contains(h, "Not done / uncertain ") {
		t.Fatalf("hint %q", h)
	}
}
