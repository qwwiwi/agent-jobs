package checklist

import (
	"errors"
	"fmt"
	"regexp"
	"slices"
	"strconv"
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
	if fenceOpen(words) {
		return nil, nil, 0, fmt.Errorf("brief has %w", ErrUnclosedFence)
	}
	// A «## Additions» line in the owner's words (in a fence or not) would be ambiguous with the heading
	// additions go under, and the text above it is all that stays bound.
	for _, l := range strings.Split(words, "\n") {
		if strings.TrimSpace(l) == AdditionsHeading {
			return nil, nil, 0, fmt.Errorf("brief has a line %q: it is the heading later additions go under; quote "+
				"it differently (for example indent it in a quote or write «Additions:»)", AdditionsHeading)
		}
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
	for _, f := range []*string{&r.ID, &r.Verbatim, &r.Contract, &r.Rationale} {
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
	// Approve approves the resulting content: Result.Record is then the approval for the caller to store.
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
	// Approved and Reason: Status of the result against Record when the call approves, else against the
	// stored approval Apply was given.
	Approved bool
	Reason   string
	// Record is the approval to store (Approve only): the result's id, version, hash and BRIEF prefix hash.
	Record *Record
	// VersionBumped: the hashed content changed (or a void approval was dropped).
	VersionBumped bool
	// PreviousVoid is why the approval on disk was void before the call ("" = it was valid or absent).
	PreviousVoid string
	Redacted     int
	Warnings     []string
}

var dateRe = regexp.MustCompile(`^[0-9]{4}-[0-9]{2}-[0-9]{2}$`)

// Binding is what the agent-jobs store holds for one checklist id of the repository: the sha256 of the
// BRIEF text above «## Additions» the id was created with (checklist_init) or last approved with, and
// whether it was ever approved. nil = the store has no record of the id.
type Binding struct {
	BriefSHA256 string
	Approved    bool
}

// ErrOtherStore marks a checklist whose id the agent-jobs store has no record of while its file carries
// an approved line: another agent (another store) manages it. One agent manages a repository's checklist.
var ErrOtherStore = errors.New("the checklist is managed by another agent: its approval is in another agent-jobs store " +
	"(one managing agent per repository); use that agent's checklist tools")

// Apply computes checklist_set's new BRIEF.md and CHECKLIST.md from the files on disk: rows upserted,
// additions appended, everything redacted before the hash is taken (every raw input and the files as
// read first, before any normalization, then the results again); the version goes up when the
// hashed content changes (or a void approval is dropped); last is the latest stored approval of the
// repository (nil = none): the approved line is kept only while last backs it, and Approve sets a new
// one over the result.
//
// bindingOf returns the store's Binding of the file's checklist id. Without one (an id this store never
// created: a hand-edited id, or a checklist another agent manages) a call that changes nothing writes
// nothing and any other is refused (ErrOtherStore when the file carries an approved line). With one, the
// BRIEF text above «## Additions» is the owner's words and is never edited: a BRIEF.md whose prefix
// differs from the binding is refused, and so is a result whose prefix would (a redactor rewriting it).
func Apply(f *Files, ch Change, now time.Time, redact Redactor, last *Record, bindingOf func(id string) (*Binding, error)) (*Result, error) {
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
	bound, err := bindingOf(cur.ID)
	if err != nil {
		return nil, err
	}
	if bound == nil {
		if len(ch.Rows) > 0 || len(ch.Additions) > 0 || ch.Approve {
			if cur.Approval != nil || cur.ApprovalErr != "" {
				return nil, fmt.Errorf("checklist %q: %w", cur.ID, ErrOtherStore)
			}
			return nil, fmt.Errorf("checklist %q has no checklist_init or approval record in this agent-jobs store: a "+
				"hand-edited id, or a checklist another agent manages (one managing agent per repository); restore the id, "+
				"or start over with checklist_init and a new id", cur.ID)
		}
		// Nothing to change and nothing of this store's to judge it by: read only, never a write.
		res.List, res.BriefAfter = cur, f.Brief
		res.Approved, res.Hash, res.Reason = cur.Status(f.Brief, last)
		return res, nil
	}
	// A hand-written BRIEF.md whose owner prefix leaves a fence open is refused: the fence would hide
	// the «## Additions» heading (checklist_init never writes one).
	if fenceOpen(BriefPrefix(f.Brief)) {
		return nil, fmt.Errorf("%s: the text above %s has %w", BriefRel, AdditionsHeading, ErrUnclosedFence)
	}
	if BriefPrefixSHA256(f.Brief) != bound.BriefSHA256 {
		return nil, prefixChanged(cur.ID, bound)
	}
	prevApproved, oldHash, reason := cur.Status(f.Brief, last)
	if strings.HasPrefix(reason, "approval void") {
		res.PreviousVoid = reason
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
	if BriefPrefixSHA256(res.BriefAfter) != bound.BriefSHA256 {
		return nil, fmt.Errorf("%s: redaction would rewrite the text above %s (the owner's words of checklist %q): "+
			"nothing written; remove the secret-looking value from it by hand and start over with checklist_init and a new id",
			BriefRel, AdditionsHeading, cur.ID)
	}
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
		r.Rationale, k = redactField(r.Rationale, redact)
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
		next.Rows[i].Rationale, k = redactField(next.Rows[i].Rationale, redact)
		res.Redacted += k
	}
	res.Hash = Hash(next.Rows, res.BriefAfter)
	rec := last
	switch {
	case res.Hash != oldHash || res.PreviousVoid != "":
		next.Version++
		next.Approval = nil
		res.VersionBumped = true
	case prevApproved:
		// The informational line follows the stored approval.
		next.Approval = &Approval{At: last.At.UTC(), SHA256: last.SHA256}
	}
	if ch.Approve {
		if len(next.Rows) == 0 {
			return nil, errors.New("approve: the checklist has no rows")
		}
		at := now.UTC().Truncate(time.Second)
		next.Approval = &Approval{At: at, SHA256: res.Hash}
		res.Record = &Record{ChecklistID: next.ID, Version: next.Version, SHA256: res.Hash,
			BriefSHA256: BriefPrefixSHA256(res.BriefAfter), At: at}
		rec = res.Record
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
	res.Approved, res.Hash, res.Reason = res.List.Status(res.BriefAfter, rec)
	if w := LongBriefWarning(res.BriefAfter, len(res.List.Rows)); w != "" {
		res.Warnings = append(res.Warnings, w)
	}
	return res, nil
}

// prefixChanged is the refusal of a BRIEF.md whose text above «## Additions» is not the bound one.
func prefixChanged(id string, bound *Binding) error {
	since := "checklist_init created checklist " + strconv.Quote(id)
	if bound.Approved {
		since = "checklist " + strconv.Quote(id) + " was last approved"
	}
	return fmt.Errorf("%s: the text above %s changed since %s; it is the owner's words and is never edited: restore it "+
		"(later owner words go in additions)", BriefRel, AdditionsHeading, since)
}

// redactField redacts one row field and normalizes it again (a replacement is one line).
func redactField(s string, redact Redactor) (string, int) {
	out, n := redact(s)
	return NormalizeField(out), n
}
