package jobs

import (
	"fmt"
	"regexp"
	"strings"
)

// Brief size defaults (config max_brief_bytes, brief_warn_files, brief_warn_headings).
const (
	DefaultMaxBriefBytes     = 32 << 10
	DefaultBriefWarnFiles    = 40
	DefaultBriefWarnHeadings = 8
)

// BriefSize is the size estimate of a brief made at job_start.
type BriefSize struct {
	Bytes    int
	Files    int // distinct path-like tokens with a file extension
	Headings int // lines starting with "## "
}

// reFileToken is a path-like token with a file extension: "main.go", "docs/PLAN.md", "./a/b.test.ts".
// The name before the extension has at least two characters, so "e.g" and "i.e" are not files.
var reFileToken = regexp.MustCompile(`^[A-Za-z0-9_.~/-]*[A-Za-z0-9_-]{2}\.[A-Za-z][A-Za-z0-9]{0,9}$`)

// MeasureBrief estimates the size of a brief: bytes, files named, "## " headings.
func MeasureBrief(brief string) BriefSize {
	s := BriefSize{Bytes: len(brief)}
	files := map[string]bool{}
	for _, line := range strings.Split(brief, "\n") {
		if strings.HasPrefix(line, "## ") {
			s.Headings++
		}
		for _, f := range strings.FieldsFunc(line, func(r rune) bool {
			return r == ' ' || r == '\t' || r == '\r' || strings.ContainsRune("`'\"()[]{}<>,;|*", r)
		}) {
			f = strings.TrimRight(f, ".:!?")
			if strings.Contains(f, "://") {
				continue // a URL, not a file
			}
			if f, _, _ = strings.Cut(f, ":"); reFileToken.MatchString(f) { // "a.go:12" names a.go
				files[f] = true
			}
		}
	}
	s.Files = len(files)
	return s
}

func (c Config) maxBriefBytes() int {
	if c.MaxBriefBytes > 0 {
		return c.MaxBriefBytes
	}
	return DefaultMaxBriefBytes
}

func (c Config) briefWarnFiles() int {
	if c.BriefWarnFiles > 0 {
		return c.BriefWarnFiles
	}
	return DefaultBriefWarnFiles
}

func (c Config) briefWarnHeadings() int {
	if c.BriefWarnHeadings > 0 {
		return c.BriefWarnHeadings
	}
	return DefaultBriefWarnHeadings
}

// checkBriefSize refuses a brief above max_brief_bytes.
func (c Config) checkBriefSize(brief string) error {
	if n, max := len(brief), c.maxBriefBytes(); n > max {
		return fmt.Errorf("brief is %d bytes, above the cap of %d (config max_brief_bytes): split the work into "+
			"smaller jobs, or put the long material in a file in the repository and name it in the brief", n, max)
	}
	return nil
}

// BriefSizeWarning is the size_warning of job_start: why the brief looks like more than one job, or "".
func (c Config) BriefSizeWarning(brief string) string {
	s := MeasureBrief(brief)
	var w []string
	if s.Files > c.briefWarnFiles() {
		w = append(w, fmt.Sprintf("the brief names %d files (more than %d, config brief_warn_files)", s.Files, c.briefWarnFiles()))
	}
	if s.Headings > c.briefWarnHeadings() {
		w = append(w, fmt.Sprintf("the brief has %d \"## \" headings (more than %d, config brief_warn_headings): "+
			"it may ask for more than one deliverable", s.Headings, c.briefWarnHeadings()))
	}
	if len(w) == 0 {
		return ""
	}
	return strings.Join(w, "; ") + "; consider splitting it into smaller jobs"
}
