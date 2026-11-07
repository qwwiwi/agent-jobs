package jobs

import (
	"fmt"
	"strings"
	"unicode/utf8"
)

// Report section names of the report contract (prompts/report-contract.md).
const (
	SectionNotDone  = "Not done / uncertain"
	SectionProgress = "Progress"
	SectionForks    = "Forks"
)

// reportSection is one "## " section of a report: its heading line and its body.
type reportSection struct {
	name  string // heading text after "## ", trimmed
	text  string // the heading line and the body, as written (a slice of the report)
	start int    // offset of the heading line in the report
}

// splitReport splits a report into the text before the first "## " heading and its sections. A
// heading inside a ``` fence does not count. One pass over the text; the sections are slices of it.
func splitReport(report string) (string, []reportSection) {
	var secs []reportSection
	headEnd := len(report)
	fence := false
	for off := 0; off < len(report); {
		end := strings.IndexByte(report[off:], '\n') + 1
		if end == 0 {
			end = len(report) - off
		}
		t := strings.TrimRight(report[off:off+end], "\r\n")
		if strings.HasPrefix(strings.TrimSpace(t), "```") {
			fence = !fence
		}
		if !fence && strings.HasPrefix(t, "## ") {
			if len(secs) == 0 {
				headEnd = off
			} else {
				secs[len(secs)-1].text = report[secs[len(secs)-1].start:off]
			}
			secs = append(secs, reportSection{name: strings.TrimSpace(t[3:]), start: off})
		}
		off += end
	}
	if len(secs) > 0 {
		secs[len(secs)-1].text = report[secs[len(secs)-1].start:]
	}
	return report[:headEnd], secs
}

// ReportSection returns the body of the report's "## <name>" section (trimmed; heading case and
// spacing ignored) and whether the section is there. The last section of that name wins.
func ReportSection(report, name string) (string, bool) {
	_, secs := splitReport(report)
	return sectionBody(secs, name)
}

// ReportSections returns the bodies of several sections (as ReportSection; "" when missing),
// parsing the report once.
func ReportSections(report string, names ...string) []string {
	_, secs := splitReport(report)
	out := make([]string, len(names))
	for i, n := range names {
		out[i], _ = sectionBody(secs, n)
	}
	return out
}

func sectionBody(secs []reportSection, name string) (string, bool) {
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

// Notes of a claude job whose report breaks the report contract (config report_contract). The first
// says the model most likely ended its turn mid-task: it wrote what it would do next and stopped.
const (
	NoteNoSummary = "report has no Summary section: likely stopped mid-task"
	NoteNoNotDone = "report has no Not done / uncertain section"
)

// maxContractReportBytes bounds how much of report.md the contract check reads.
const maxContractReportBytes = 1 << 20

// reportContractNote returns the note for a report without a "## Summary" or a "## Not done /
// uncertain" section, or, for a job with covers (Wave F, F4), without a "### Requirements" block in
// Findings / Changes ("" when all are there).
func reportContractNote(report string, covers []string) string {
	_, secs := splitReport(report)
	if _, ok := sectionBody(secs, strings.TrimPrefix(reportHeading, "## ")); !ok {
		return NoteNoSummary
	}
	if _, ok := sectionBody(secs, SectionNotDone); !ok {
		return NoteNoNotDone
	}
	return requirementsNote(report, covers)
}

// OffContractNote returns the report contract note a job's error carries, "" when none.
func OffContractNote(errMsg string) string {
	for _, n := range []string{NoteNoSummary, NoteNoNotDone, NoteNoRequirements, NoteRequirementsEmpty, NoteJobRowUnread} {
		if strings.Contains(errMsg, n) {
			return n
		}
	}
	return ""
}

// SectionSummary is the report's Summary section name.
const SectionSummary = "Summary"

// SectionSize is one report section and its size in bytes (heading line included).
type SectionSize struct {
	Name  string `json:"name"`
	Bytes int    `json:"bytes"`
}

// ReportSectionSizes lists the "## " sections of a report in order with their sizes.
func ReportSectionSizes(report string) []SectionSize {
	_, secs := splitReport(report)
	out := make([]SectionSize, 0, len(secs))
	for _, s := range secs {
		out = append(out, SectionSize{Name: s.name, Bytes: len(s.text)})
	}
	return out
}

// ReportSectionTexts returns the named sections as written (heading line included, report order,
// names matched case-insensitively, the last section of a name wins) and the names that are not in the report.
func ReportSectionTexts(report string, names ...string) (string, []string) {
	_, secs := splitReport(report)
	var b strings.Builder
	var missing []string
	for _, n := range names {
		n = strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(n), "## "))
		found := -1
		for i := len(secs) - 1; i >= 0; i-- {
			if strings.EqualFold(secs[i].name, n) {
				found = i
				break
			}
		}
		if found < 0 {
			missing = append(missing, n)
			continue
		}
		if b.Len() > 0 {
			b.WriteString("\n")
		}
		b.WriteString(strings.TrimRight(secs[found].text, "\n") + "\n")
	}
	return b.String(), missing
}

// CapText cuts s to at most max bytes at a rune boundary and appends a marker saying how much was
// left out; s is returned as is when it fits. The marker is not counted in max.
func CapText(s string, max int) (string, bool) {
	if max < 0 || len(s) <= max {
		return s, false
	}
	cut := max
	for cut > 0 && cut < len(s) && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return fmt.Sprintf("%s\n[cut: %d of %d bytes shown]", s[:cut], cut, len(s)), true
}

// ReportDigest is the compact view of a report: the sections an orchestrator reads first, each
// capped, and the size of every section so it can ask for the rest.
type ReportDigest struct {
	NotDone, Summary, Forks string
	Sizes                   []SectionSize
	Bytes                   int
	// Cut lists the digest fields that were cut at the cap.
	Cut []string
}

// DigestReport builds the digest of a report; each of the three text fields is capped at maxField bytes
// (a cut one ends with a marker). "Not done / uncertain" is never left out, only capped.
func DigestReport(report string, maxField int) ReportDigest {
	secs := ReportSections(report, SectionNotDone, SectionSummary, SectionForks)
	d := ReportDigest{Sizes: ReportSectionSizes(report), Bytes: len(report)}
	fields := []struct {
		name string
		src  string
		dst  *string
	}{{SectionNotDone, secs[0], &d.NotDone}, {SectionSummary, secs[1], &d.Summary}, {SectionForks, secs[2], &d.Forks}}
	for _, f := range fields {
		var cut bool
		*f.dst, cut = CapText(f.src, maxField)
		if cut {
			d.Cut = append(d.Cut, f.name)
		}
	}
	return d
}

// SizesHint is one line naming every section with its size, for the compact view.
func (d ReportDigest) SizesHint() string {
	parts := make([]string, len(d.Sizes))
	for i, s := range d.Sizes {
		parts[i] = fmt.Sprintf("%s %d B", s.Name, s.Bytes)
	}
	return fmt.Sprintf("report %d B: %s", d.Bytes, strings.Join(parts, ", "))
}
