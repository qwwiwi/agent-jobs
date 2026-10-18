package brain

import (
	"errors"
	"fmt"
	"regexp"
	"strings"
)

// maxNotePathLen caps a note path taken from the brain's answer to a write.
const maxNotePathLen = 512

var (
	reNotePath    = regexp.MustCompile(`^[A-Za-z0-9._/-]+\.md$`)
	reWriteAnswer = regexp.MustCompile(`^([a-z]+): (\S+)$`)
)

// WriteResult is what the brain said about a note it was asked to write.
type WriteResult struct {
	// Verb is the brain's word for what happened ("created", "exists", ...); "" when the answer
	// carried the path as a field.
	Verb string
	// Path is the note's path relative to the vault, validated by ValidNotePath.
	Path string
}

// Written reports whether the note at Path now holds the content just sent: "created" or "updated",
// or an answer with a path field and no verb.
func (r WriteResult) Written() bool {
	return r.Verb == "" || r.Verb == "created" || r.Verb == "updated"
}

// ParseWriteResult extracts the note path from a Write answer: an object with a "path" string, or
// a "result" string "<word>: <path>" (the real brain answers {"result": "created: 80-x/y.md"}).
func ParseWriteResult(o Object) (WriteResult, error) {
	if p, ok := o["path"].(string); ok {
		if err := ValidNotePath(p); err != nil {
			return WriteResult{}, err
		}
		return WriteResult{Path: p}, nil
	}
	s, ok := o["result"].(string)
	if !ok {
		return WriteResult{}, errors.New("brain answer to a write carries no note path")
	}
	m := reWriteAnswer.FindStringSubmatch(strings.TrimSpace(s))
	if m == nil {
		return WriteResult{}, errors.New("brain answer to a write is not \"<word>: <path>\"")
	}
	if err := ValidNotePath(m[2]); err != nil {
		return WriteResult{}, err
	}
	return WriteResult{Verb: m[1], Path: m[2]}, nil
}

// ValidNotePath accepts a vault-relative markdown path: no leading "/", no "..", only
// [A-Za-z0-9._/-], ending in .md, at most maxNotePathLen bytes.
func ValidNotePath(p string) error {
	if len(p) > maxNotePathLen || strings.HasPrefix(p, "/") || !reNotePath.MatchString(p) {
		return fmt.Errorf("note path %q is not a vault-relative .md path", p)
	}
	for _, seg := range strings.Split(p, "/") {
		if seg == ".." || seg == "." || seg == "" {
			return fmt.Errorf("note path %q is not a vault-relative .md path", p)
		}
	}
	return nil
}
