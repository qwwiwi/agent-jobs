package checklist

import (
	"errors"
	"fmt"
	"regexp"
	"slices"
	"strings"
	"time"
)

// Redactor replaces secret-looking values in a text and returns it with the number replaced (the
// Wave D redactor, jobs.Redact).
type Redactor func(string) (string, int)

// Init is the content of a new brief/ pair: BRIEF.md with the owner's words (redacted) and an empty,
// unapproved CHECKLIST.md at version 1. The raw inputs are redacted before anything else touches them.
func Init(id, words string, redact Redactor) (brief, list []byte, redacted int, err error) {
	id, n := redact(id)
	words, k := redact(words)
	redacted = n + k
	id = NormalizeField(id)
	if id == "" {
		return nil, nil, 0, errors.New("checklist id is empty")
	}
	if strings.TrimSpace(words) == "" {
		return nil, nil, 0, errors.New("brief is empty: give the owner's words verbatim")
	}
	b, n := redact(InitBrief(words))
	c := &Checklist{ID: id, Brief: BriefRel, Version: 1}
	l, k := redact(Render(c))
	return []byte(b), []byte(l), redacted + n + k, nil
}

// redactRaw runs redact over every field of an input row as given, before NormalizeRow collapses its
// line breaks: a multi-line secret (a PEM block or a fragment of one) is only recognized whole.
func redactRaw(r Row, redact Redactor) (Row, int) {
	n := 0
	for _, f := range []*string{&r.ID, &r.Verbatim, &r.Contract, &r.Basis} {
		var k int
		*f, k = redact(*f)
		n += k
	}
	return r, n
}

// Change is one checklist_set call.
type Change struct {
	// Rows are upserts: a row with a new ID is appended, one with an existing ID replaces it in place.
	Rows []Row
	// Additions are appended to BRIEF.md under «## Additions» in the same operation.
	Additions []Addition
	// Approve records the owner's approval of the resulting content.
	Approve bool
	// ExpectVersion, when > 0, refuses the call unless the file is at that version.
	ExpectVersion int
}

// Result is what Apply computed. Brief / Checklist are nil when that file does not change.
type Result struct {
	Brief, Checklist []byte
	List             *Checklist
	// BriefAfter is BRIEF.md as it will be after the call (written or not).
	BriefAfter []byte
	Hash       string
	Approved   bool
	Reason     string
	// VersionBumped: the hashed content changed (or a void approval was dropped).
	VersionBumped bool
	// PreviousVoid is why the approval on disk was void before the call ("" = it was valid or absent).
	PreviousVoid string
	Redacted     int
	Warnings     []string
}

var dateRe = regexp.MustCompile(`^[0-9]{4}-[0-9]{2}-[0-9]{2}$`)

// Apply computes checklist_set's new BRIEF.md and CHECKLIST.md from the files on disk: rows upserted,
// additions appended, everything redacted before the hash is taken (every raw input and the files as
// read first, before any normalization, then the results again); the version goes up when the
// hashed content changes (or a void approval is dropped); an approval is kept only while the hash it
// names stays the hash, and Approve sets a new one over the result.
func Apply(f *Files, ch Change, now time.Time, redact Redactor) (*Result, error) {
	if f.Checklist == nil || f.Brief == nil {
		return nil, ErrNoChecklist
	}
	res := &Result{}
	// The file as read is redacted before it is parsed (a manual edit may have put a secret there).
	text, n := redact(string(f.Checklist))
	res.Redacted += n
	cur, err := Parse(text)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", ChecklistRel, err)
	}
	if ch.ExpectVersion > 0 && ch.ExpectVersion != cur.Version {
		return nil, fmt.Errorf("expect_version %d, the checklist is at version %d (changed meanwhile; read it again)", ch.ExpectVersion, cur.Version)
	}
	_, oldHash, reason := cur.Status(f.Brief)
	if cur.Approval != nil || cur.ApprovalErr != "" {
		if strings.HasPrefix(reason, "approval void") {
			res.PreviousVoid = reason
		}
	}
	// BRIEF: additions appended; the whole file redacted (a secret must never reach it).
	for i, a := range ch.Additions {
		var k int
		a.Quote, k = redact(a.Quote)
		ch.Additions[i].Quote = a.Quote
		res.Redacted += k
		if strings.TrimSpace(a.Quote) == "" {
			return nil, fmt.Errorf("addition %d: the quote is empty", i+1)
		}
		if a.Date == "" {
			ch.Additions[i].Date = now.UTC().Format(time.DateOnly)
		} else if !dateRe.MatchString(a.Date) {
			return nil, fmt.Errorf("addition %d: date %q must be YYYY-MM-DD", i+1, a.Date)
		}
	}
	brief, k := redact(AppendAdditions(string(f.Brief), ch.Additions))
	res.Redacted += k
	res.BriefAfter = []byte(brief)
	if brief != string(f.Brief) {
		res.Brief = res.BriefAfter
	}
	// Rows: upserted and normalized.
	next := &Checklist{ID: cur.ID, Brief: BriefRel, Version: cur.Version, Approval: cur.Approval, Rows: slices.Clone(cur.Rows)}
	newG := false
	for _, in := range ch.Rows {
		in, k := redactRaw(in, redact)
		res.Redacted += k
		r, err := NormalizeRow(in)
		if err != nil {
			return nil, err
		}
		r.Verbatim, k = redactField(r.Verbatim, redact)
		res.Redacted += k
		r.Basis, k = redactField(r.Basis, redact)
		res.Redacted += k
		if i := slices.IndexFunc(next.Rows, func(x Row) bool { return x.ID == r.ID }); i >= 0 {
			next.Rows[i] = r
			continue
		}
		if strings.HasPrefix(r.ID, "G") {
			newG = true
		}
		next.Rows = append(next.Rows, r)
	}
	if newG && len(ch.Additions) == 0 {
		return nil, errors.New("a new G row is an owner addition: give the owner's words in additions in the same call")
	}
	// Rows already on disk are redacted too (a manual edit may have put a secret there).
	for i := range next.Rows {
		var k int
		next.Rows[i].Verbatim, k = redactField(next.Rows[i].Verbatim, redact)
		res.Redacted += k
		next.Rows[i].Basis, k = redactField(next.Rows[i].Basis, redact)
		res.Redacted += k
	}
	res.Hash = Hash(next.Rows, res.BriefAfter)
	if res.Hash != oldHash || res.PreviousVoid != "" {
		next.Version++
		next.Approval = nil
		res.VersionBumped = true
	}
	if ch.Approve {
		if len(next.Rows) == 0 {
			return nil, errors.New("approve: the checklist has no rows")
		}
		next.Approval = &Approval{At: now.UTC().Truncate(time.Second), SHA256: res.Hash}
	}
	list, k := redact(Render(next))
	res.Redacted += k
	if list != string(f.Checklist) {
		res.Checklist = []byte(list)
	}
	// What was written must be what is reported: parse it back.
	if res.List, err = Parse(list); err != nil {
		return nil, fmt.Errorf("internal: the rendered checklist does not parse: %w", err)
	}
	res.Approved, res.Hash, res.Reason = res.List.Status(res.BriefAfter)
	if w := LongBriefWarning(res.BriefAfter, len(res.List.Rows)); w != "" {
		res.Warnings = append(res.Warnings, w)
	}
	return res, nil
}

// redactField redacts one row field and normalizes it again (a replacement is one line).
func redactField(s string, redact Redactor) (string, int) {
	out, n := redact(s)
	return NormalizeField(out), n
}
