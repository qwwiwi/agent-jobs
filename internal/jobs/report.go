package jobs

import "strings"

// Report section names of the report contract (prompts/report-contract.md).
const (
	SectionNotDone  = "Not done / uncertain"
	SectionProgress = "Progress"
)

// reportSection is one "## " section of a report: its heading line and its body.
type reportSection struct {
	name string // heading text after "## ", trimmed
	text string // the heading line and the body, as written
}

// splitReport splits a report into the text before the first "## " heading and its sections. A
// heading inside a ``` fence does not count.
func splitReport(report string) (string, []reportSection) {
	var head strings.Builder
	var secs []reportSection
	fence := false
	for _, line := range strings.SplitAfter(report, "\n") {
		t := strings.TrimRight(line, "\r\n")
		if strings.HasPrefix(strings.TrimSpace(t), "```") {
			fence = !fence
		}
		if !fence && strings.HasPrefix(t, "## ") {
			secs = append(secs, reportSection{name: strings.TrimSpace(t[3:])})
		}
		if len(secs) == 0 {
			head.WriteString(line)
		} else {
			secs[len(secs)-1].text += line
		}
	}
	return head.String(), secs
}

// ReportSection returns the body of the report's "## <name>" section (trimmed; heading case and
// spacing ignored) and whether the section is there. The last section of that name wins.
func ReportSection(report, name string) (string, bool) {
	_, secs := splitReport(report)
	for i := len(secs) - 1; i >= 0; i-- {
		if strings.EqualFold(secs[i].name, name) {
			_, body, _ := strings.Cut(secs[i].text, "\n")
			return strings.TrimSpace(body), true
		}
	}
	return "", false
}

// ReadFirst reorders a report for reading: "Not done / uncertain" first, "Progress" right after it,
// then the other sections in their order. A report without those sections is returned as is.
func ReadFirst(report string) string {
	head, secs := splitReport(report)
	var first, progress, rest []string
	for _, s := range secs {
		text := strings.TrimRight(s.text, "\n") + "\n"
		switch {
		case strings.EqualFold(s.name, SectionNotDone):
			first = append(first, text)
		case strings.EqualFold(s.name, SectionProgress):
			progress = append(progress, text)
		default:
			rest = append(rest, text)
		}
	}
	if len(first) == 0 && len(progress) == 0 {
		return report
	}
	var b strings.Builder
	if h := strings.TrimSpace(head); h != "" {
		b.WriteString(h + "\n\n")
	}
	b.WriteString(strings.Join(append(append(first, progress...), rest...), "\n"))
	return b.String()
}
