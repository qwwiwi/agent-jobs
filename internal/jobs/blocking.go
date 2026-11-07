package jobs

import (
	"errors"
	"fmt"
	"regexp"
	"slices"
	"strings"

	"github.com/qwwiwi/agent-jobs/internal/store"
)

// The review checklist (prompts/kind-review.md) fixes the report shape: a «### Blocking» subsection
// of Findings / Changes, one finding per line, "none" when empty. It does not fix an ID scheme, so a
// fix brief is built only from a review whose Blocking items carry one (F1, H2, C1, M3: one or two
// capital letters and a number). An item may declare several IDs («- H1: a; H2: b»); each ID stands
// first on the item or right after its file:line (and a severity word), or after "; " / "|" or at the
// start of a wrapped line or sub-bullet, in each place with the same syntax (whitespace or a delimiter
// after it); after "," it needs a delimiter. An item whose IDs cannot be told from mentions, or that has
// none, is refused loudly with the line named instead of being accepted as a subset: a dropped finding
// is the very failure this parser exists to prevent.
//
// The Blocking line is usually a one-line summary; the finding's scenarios and minimal fix live in a
// detailed entry elsewhere in the report («**H1 — High — file:line: title.**» under «### Code»). A
// finding's body is the Blocking item plus every such entry for its ID.

var (
	// ErrNoBlockingSection is a report with no «### Blocking» heading.
	ErrNoBlockingSection = errors.New("the report has no «### Blocking» section")
	// ErrNothingToFix is a report whose Blocking section is empty («none»).
	ErrNothingToFix = errors.New("nothing to fix: the Blocking section is empty")
)

const blockingIDPattern = `[A-Z]{1,2}[0-9]{1,3}`

// declLocation is the «file:line —» (and severity words) that may precede an ID. Its location token
// holds a «.», «/» or «:<digit>», so a bare «H1:» is never taken for a location. \s covers a line break:
// the location may wrap.
const declLocation = "[*_`]*(?:`[^`\\n]*[./:][^`\\n]*`|[^\\s`]*(?:[./]|:[0-9])[^\\s`]*)[\\s*_`]*[:—–-]+\\s*[*_]*\\[?" +
	"(?:(?i:open|blocking|high|critical|medium|low|major|minor|severe|p[0-3]|sev[0-3])[\\s*_`:—–-]*)*"

var (
	headingLine  = regexp.MustCompile(`^(#{1,6})\s`)
	blockingHead = regexp.MustCompile(`(?i)^#{1,6}\s+blocking\s*$`)
	bulletMarker = regexp.MustCompile(`^ {0,1}(?:[-*+]|[0-9]{1,3}[.)])\s+`)
	idToken      = regexp.MustCompile(`\b` + blockingIDPattern + `\b`)
	verdictLine  = regexp.MustCompile(`(?i)^[\s*_]*verdict:\s*[*_]*\s*(SHIP WITH FIXES|SHIP|BLOCK)\b`)
	noneSynonyms = map[string]bool{"none": true, "n/a": true, "nothing": true, "no blocking findings": true, "empty": true, "no blocking issues": true}
	fenceMarker  = regexp.MustCompile("^\\s{0,3}(```|~~~)")
	indentOfLine = regexp.MustCompile(`^\s*`)

	emphasisTrims = " \t*_`()[].-"

	// The first ID of an item: nothing but the location before it, a delimiter (or the end) after it.
	declFirstPrefix = regexp.MustCompile("^(?:" + declLocation + ")?[\\s*_`\\[(]*$")
	declFirstSuffix = regexp.MustCompile("^(?:[\\]):.*_`\\s—–-]|$)")
	// A further ID at the start of a wrapped line or sub-bullet, or right after "; " / "|", takes the
	// first ID's syntax: its location before it, a delimiter or whitespace after it.
	declLaterPrefix = regexp.MustCompile("(?:[;|]|\\n[ \\t]*(?:(?:[-*+]|[0-9]{1,3}[.)])[ \\t]+)?)[ \\t]*(?:" + declLocation + ")?[\\s*_`\\[(]*$")
	// After ", " an ID is a declaration only when punctuation follows it: «M1, M2 and L3» lists mentions
	// as much as declarations, so a comma ID followed by whitespace alone makes the item ambiguous.
	declCommaPrefix = regexp.MustCompile(",[ \\t]*(?:" + declLocation + ")?[\\s*_`\\[(]*$")
	declCommaSuffix = regexp.MustCompile("^[*_`\\])]*(?:\\s*[:—–]|\\.(?:\\s|$)|\\s+-\\s)")
	// An ID followed like a declaration but standing elsewhere makes the item ambiguous.
	declLooksLike = regexp.MustCompile("^[*_`\\])]*\\s*[:—–]")

	// The start of a detailed entry: a heading or a bold paragraph opening with the ID, or with a
	// severity word and then the ID («**High — H1: …**», «### Medium: M1 …»), or a plain «H1: …» /
	// «H1 — …» paragraph. Bullets never start one (a Delivered bullet that names an ID is a status, not
	// the finding).
	entryStartMarked = regexp.MustCompile(`^(?:#{1,6}\s+(?:\*\*|__)?|\*\*|__)` +
		`(?:(?i:critical|high|medium|low|major|minor|severe|p[0-3]|sev[0-3])(?:\*\*|__)?\s*[:—–-]+\s*(?:\*\*|__)?)?` +
		`\[?(` + blockingIDPattern + `)\b`)
	entryStartPlain = regexp.MustCompile("^[`\\[(]*(" + blockingIDPattern + ")[*_`\\])]*\\s*[:—–]")
)

// ParseBlocking extracts every finding of the «### Blocking» section of a review report, each with
// its ID and its whole text: the Blocking item (continuation lines and sub-bullets included) followed
// by the finding's detailed entries elsewhere in the report. The section runs to the next heading
// outside a code fence. ErrNoBlockingSection and ErrNothingToFix are returned as they are; an item
// without a declared ID, an item whose IDs are ambiguous, or a repeated ID is an error naming the line.
func ParseBlocking(report string) ([]store.BlockingFinding, error) {
	all := strings.Split(strings.ReplaceAll(report, "\r\n", "\n"), "\n")
	head, end, found := blockingSection(all)
	if !found {
		return nil, ErrNoBlockingSection
	}
	section := all[head+1 : end]
	if blockingIsNone(section) {
		return nil, ErrNothingToFix
	}
	var items [][]string
	inFence, prevBlank := false, true
	for _, line := range section {
		line = strings.TrimRight(line, " \t")
		if fenceMarker.MatchString(line) {
			inFence = !inFence
		}
		blank := strings.TrimSpace(line) == ""
		start := false
		if !inFence && !blank && !fenceMarker.MatchString(line) {
			indent := len(indentOfLine.FindString(line))
			switch {
			case bulletMarker.MatchString(line):
				start = true
			case indent == 0 && (prevBlank || len(items) == 0 || declaresID(line)):
				start = true
			}
		}
		prevBlank = blank
		switch {
		case start:
			items = append(items, []string{line})
		case len(items) > 0:
			items[len(items)-1] = append(items[len(items)-1], line)
		case !blank:
			items = append(items, []string{line})
		}
	}
	fenced := fencedLines(all)
	seen := map[string]bool{}
	var out []store.BlockingFinding
	for _, it := range items {
		ids, err := declaredIDs(it)
		if err != nil {
			return nil, err
		}
		if len(ids) == 0 {
			return nil, fmt.Errorf("a Blocking item has no finding ID (expected F1, H2, C1 … first or after its file:line): %q",
				Printable(strings.TrimSpace(it[0]), 160))
		}
		text := strings.TrimRight(strings.Join(it, "\n"), "\n ")
		for _, id := range ids {
			if seen[id] {
				return nil, fmt.Errorf("finding ID %s appears twice in the Blocking section", id)
			}
			seen[id] = true
			body := text
			if entries := detailedEntries(all, fenced, head, end, id); len(entries) > 0 {
				body += "\n\nDetailed entry in the report:\n\n" + strings.Join(entries, "\n\n")
			}
			out = append(out, store.BlockingFinding{ID: id, Body: body})
		}
	}
	if len(out) == 0 {
		return nil, ErrNothingToFix
	}
	return out, nil
}

// declaresID reports whether a single line declares an ID (it can start an item of its own).
func declaresID(line string) bool {
	ids, err := declaredIDs([]string{line})
	return err == nil && len(ids) > 0
}

// declaredIDs is every ID a Blocking item declares, in order. An ID standing where a declaration
// would, but in no recognised place, is an error naming its line: the item is ambiguous.
func declaredIDs(lines []string) ([]string, error) {
	text := strings.Join(append([]string{bulletMarker.ReplaceAllString(lines[0], "")}, lines[1:]...), "\n")
	var ids []string
	for _, loc := range idToken.FindAllStringIndex(text, -1) {
		id, before, after := text[loc[0]:loc[1]], text[:loc[0]], text[loc[1]:]
		switch {
		case slices.Contains(ids, id):
			// Repeated inside the item: the same finding.
		case declFirstPrefix.MatchString(before) && declFirstSuffix.MatchString(after),
			declLaterPrefix.MatchString(before) && declFirstSuffix.MatchString(after),
			declCommaPrefix.MatchString(before) && declCommaSuffix.MatchString(after):
			ids = append(ids, id)
		case declLooksLike.MatchString(after),
			declCommaPrefix.MatchString(before) && declFirstSuffix.MatchString(after):
			line := lines[min(strings.Count(before, "\n"), len(lines)-1)]
			return nil, fmt.Errorf("a Blocking item declares %s in a place that cannot be told from a mention "+
				"(put each ID first on its own item, or right after its file:line): %q", id, Printable(strings.TrimSpace(line), 160))
		}
	}
	return ids, nil
}

// fencedLines marks the lines of a code fence, its marker lines included.
func fencedLines(lines []string) []bool {
	out := make([]bool, len(lines))
	in := false
	for i, line := range lines {
		if fenceMarker.MatchString(line) {
			in = !in
			out[i] = true
			continue
		}
		out[i] = in
	}
	return out
}

// entryStart is the ID a detailed entry opens with and its heading level (0 for a paragraph), "" when
// the line opens none.
func entryStart(line string) (id string, level int) {
	if m := entryStartMarked.FindStringSubmatch(line); m != nil {
		if h := headingLine.FindStringSubmatch(line); h != nil {
			return m[1], len(h[1])
		}
		return m[1], 0
	}
	if m := entryStartPlain.FindStringSubmatch(line); m != nil {
		return m[1], 0
	}
	return "", 0
}

// detailedEntries returns the entries of finding id outside the Blocking section [head, end): each
// runs from the line that opens it to the next heading (a deeper heading too, unless the entry is a
// heading itself), or to the next entry of any finding.
func detailedEntries(lines []string, fenced []bool, head, end int, id string) []string {
	var out []string
	for i := 0; i < len(lines); i++ {
		if i >= head && i < end {
			i = end - 1
			continue
		}
		if fenced[i] {
			continue
		}
		got, level := entryStart(lines[i])
		if got != id {
			continue
		}
		j := i + 1
		for ; j < len(lines) && !(j >= head && j < end); j++ {
			if fenced[j] {
				continue
			}
			if h := headingLine.FindStringSubmatch(lines[j]); h != nil && (level == 0 || len(h[1]) <= level) {
				break
			}
			if other, _ := entryStart(lines[j]); other != "" {
				break
			}
		}
		out = append(out, strings.TrimRight(strings.Join(lines[i:j], "\n"), "\n \t"))
		i = j - 1
	}
	return out
}

// blockingSection locates the first «Blocking» heading outside a code fence: its line, and the line of
// the next heading outside a fence (or the end), so the section is lines[head+1:end].
func blockingSection(lines []string) (head, end int, found bool) {
	inFence := false
	for i, line := range lines {
		if fenceMarker.MatchString(line) {
			inFence = !inFence
			continue
		}
		if inFence {
			continue
		}
		if !found {
			if blockingHead.MatchString(strings.TrimRight(line, " \t")) {
				head, found = i, true
			}
			continue
		}
		if headingLine.MatchString(line) {
			return head, i, true
		}
	}
	return head, len(lines), found
}

// blockingIsNone reports an empty section or one that says so («none», «**None.**», «n/a»).
func blockingIsNone(section []string) bool {
	text := strings.ToLower(strings.TrimSpace(strings.Join(section, " ")))
	text = strings.TrimSpace(strings.Trim(text, emphasisTrims))
	return text == "" || noneSynonyms[text]
}

// ReviewVerdict is the verdict word of a review report («SHIP», «SHIP WITH FIXES», «BLOCK»), "" if
// the report has no «Verdict:» line.
func ReviewVerdict(report string) string {
	for _, line := range strings.Split(report, "\n") {
		if m := verdictLine.FindStringSubmatch(line); m != nil {
			return strings.ToUpper(m[1])
		}
	}
	return ""
}
