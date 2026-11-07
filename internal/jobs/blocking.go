package jobs

import (
	"errors"
	"fmt"
	"regexp"
	"strings"

	"github.com/qwwiwi/agent-jobs/internal/store"
)

// The review checklist (prompts/kind-review.md) fixes the report shape: a «### Blocking» subsection
// of Findings / Changes, one finding per line, "none" when empty. It does not fix an ID scheme, so a
// fix brief is built only from a review whose Blocking items carry one (F1, H2, C1, M3: one or two
// capital letters and a number), either first on the item or right after its file:line location.
// An item without an ID is refused loudly instead of being dropped: a dropped finding is the very
// failure this parser exists to prevent.

var (
	// ErrNoBlockingSection is a report with no «### Blocking» heading.
	ErrNoBlockingSection = errors.New("the report has no «### Blocking» section")
	// ErrNothingToFix is a report whose Blocking section is empty («none»).
	ErrNothingToFix = errors.New("nothing to fix: the Blocking section is empty")
)

const blockingIDPattern = `[A-Z]{1,2}[0-9]{1,3}`

var (
	headingLine   = regexp.MustCompile(`^#{1,6}\s`)
	blockingHead  = regexp.MustCompile(`(?i)^#{1,6}\s+blocking\s*$`)
	bulletMarker  = regexp.MustCompile(`^ {0,1}(?:[-*+]|[0-9]{1,3}[.)])\s+`)
	idFirst       = regexp.MustCompile("^[\\s*_`\\[(]*(" + blockingIDPattern + ")(?:[\\]):.*_`\\s—–-]|$)")
	idAfterLoc    = regexp.MustCompile("^[\\s*_`]*(?:`[^`]+`|[^\\s`]+)[\\s*_`]*[:—–-]+\\s*[*_]*\\[?(?:(?i:open|blocking)\\s+)?(" + blockingIDPattern + ")(?:[\\]):.*_`\\s—–-]|$)")
	verdictLine   = regexp.MustCompile(`(?i)^[\s*_]*verdict:\s*[*_]*\s*(SHIP WITH FIXES|SHIP|BLOCK)\b`)
	noneSynonyms  = map[string]bool{"none": true, "n/a": true, "nothing": true, "no blocking findings": true, "empty": true, "no blocking issues": true}
	fenceMarker   = regexp.MustCompile("^\\s{0,3}(```|~~~)")
	indentOfLine  = regexp.MustCompile(`^\s*`)
	emphasisTrims = " \t*_`()[].-"
)

// ParseBlocking extracts every finding of the «### Blocking» section of a review report, each with
// its ID and its whole text (the item's lines, continuation lines and sub-bullets included). The
// section runs to the next heading outside a code fence. ErrNoBlockingSection and ErrNothingToFix
// are returned as they are; an item without an ID, or a repeated ID, is an error naming the line.
func ParseBlocking(report string) ([]store.BlockingFinding, error) {
	lines := strings.Split(strings.ReplaceAll(report, "\r\n", "\n"), "\n")
	section, found := blockingSection(lines)
	if !found {
		return nil, ErrNoBlockingSection
	}
	if blockingIsNone(section) {
		return nil, ErrNothingToFix
	}
	type item struct{ lines []string }
	var items []*item
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
			case indent == 0 && (prevBlank || len(items) == 0 || firstLineID(line) != ""):
				start = true
			}
		}
		prevBlank = blank
		switch {
		case start:
			items = append(items, &item{lines: []string{line}})
		case len(items) > 0:
			cur := items[len(items)-1]
			cur.lines = append(cur.lines, line)
		case !blank:
			items = append(items, &item{lines: []string{line}})
		}
	}
	seen := map[string]bool{}
	var out []store.BlockingFinding
	for _, it := range items {
		id := firstLineID(it.lines[0])
		if id == "" {
			return nil, fmt.Errorf("a Blocking item has no finding ID (expected F1, H2, C1 … first or after its file:line): %q",
				Printable(strings.TrimSpace(it.lines[0]), 160))
		}
		if seen[id] {
			return nil, fmt.Errorf("finding ID %s appears twice in the Blocking section", id)
		}
		seen[id] = true
		body := strings.TrimRight(strings.Join(it.lines, "\n"), "\n ")
		out = append(out, store.BlockingFinding{ID: id, Body: body})
	}
	if len(out) == 0 {
		return nil, ErrNothingToFix
	}
	return out, nil
}

// firstLineID is the finding ID on the first line of an item, "" if there is none.
func firstLineID(line string) string {
	rest := bulletMarker.ReplaceAllString(line, "")
	if m := idFirst.FindStringSubmatch(rest); m != nil {
		return m[1]
	}
	if m := idAfterLoc.FindStringSubmatch(rest); m != nil {
		return m[1]
	}
	return ""
}

// blockingSection returns the lines under the first «Blocking» heading outside a code fence, up to
// the next heading outside a fence.
func blockingSection(lines []string) (section []string, found bool) {
	inFence := false
	for _, line := range lines {
		if fenceMarker.MatchString(line) {
			inFence = !inFence
			if found {
				section = append(section, line)
			}
			continue
		}
		if inFence {
			if found {
				section = append(section, line)
			}
			continue
		}
		if !found {
			found = blockingHead.MatchString(strings.TrimRight(line, " \t"))
			continue
		}
		if headingLine.MatchString(line) {
			break
		}
		section = append(section, line)
	}
	return section, found
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
