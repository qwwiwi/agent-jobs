// Package checklist is the brief-acceptance checklist of a project repository (Wave F, F1): the
// format of brief/CHECKLIST.md, its parser and normalizer, the approval hash, and the BRIEF.md
// «## Additions» writer. The MCP tools checklist_init / checklist_get / checklist_set (F2) are its
// only writers; this package does no I/O beyond the repository's brief/ directory (files.go).
//
// CHECKLIST.md:
//
//	# Checklist
//
//	- id: <checklist id>
//	- brief: brief/BRIEF.md
//	- version: <n>
//	- approved: <RFC 3339 time> owner sha256:<hex>   (or "approved: no")
//
//	| ID | Brief (verbatim) | Contract | Basis |
//	|---|---|---|---|
//	| R01 | <owner quote> | live | <why this row exists> |
//
// The hash covers the normalized contract fields of every row (ID|verbatim|contract|placeholder-ok|basis)
// and the sha256 of BRIEF.md; the header (id, version, approved) and any text outside the table are not
// hashed.
package checklist

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"
)

// BriefRel and ChecklistRel are the two files of the brief flow, relative to the repository root.
const (
	BriefRel     = "brief/BRIEF.md"
	ChecklistRel = "brief/CHECKLIST.md"
)

// Contracts of a row.
const (
	ContractLive     = "live"
	ContractDeferred = "deferred"
	ContractDropped  = "dropped"
	// PlaceholderOK is the optional contract flag: a visible placeholder is an accepted result.
	PlaceholderOK = "placeholder-ok"
)

// TableHeader is the table's header row as written.
const TableHeader = "| ID | Brief (verbatim) | Contract | Basis |"

// LongBriefRunes is the BRIEF.md size (characters) from which a checklist of fewer than
// MinRowsLongBrief rows draws a warning: a retelling, not a checklist (F1 rule of thumb).
const (
	LongBriefRunes   = 1500
	MinRowsLongBrief = 10
)

// idRe is a checklist row ID: R (from the brief), R..i (implied), G (added later), D (constraint).
var idRe = regexp.MustCompile(`^(?:R[0-9]{2,}i?|G[0-9]{2,}|D[0-9]{2,})$`)

// ValidID reports whether id is a checklist row ID (R01, R03i, G02, D01).
func ValidID(id string) bool { return idRe.MatchString(id) }

// Row is one checklist row, normalized.
type Row struct {
	ID            string `json:"id"`
	Verbatim      string `json:"verbatim"`
	Contract      string `json:"contract"`
	PlaceholderOK bool   `json:"placeholder_ok,omitempty"`
	Basis         string `json:"basis"`
}

// Live reports whether the row is a live requirement: an R, Ri or G row with contract live. Only
// live requirements count in the 100% (D rows never do).
func (r Row) Live() bool { return r.Contract == ContractLive && !strings.HasPrefix(r.ID, "D") }

// Approval is the parsed «approved:» header line.
type Approval struct {
	At     time.Time `json:"at"`
	SHA256 string    `json:"sha256"`
}

// Checklist is a parsed CHECKLIST.md.
type Checklist struct {
	ID      string
	Brief   string
	Version int
	// Approval is nil when the file says «approved: no»; ApprovalErr is set when the line is there but
	// malformed (a manual edit), which voids the approval.
	Approval    *Approval
	ApprovalErr string
	Rows        []Row
}

// Row returns the row with id, if any.
func (c *Checklist) Row(id string) (Row, bool) {
	for _, r := range c.Rows {
		if r.ID == id {
			return r, true
		}
	}
	return Row{}, false
}

// LiveIDs are the IDs of the live requirements, in file order.
func (c *Checklist) LiveIDs() []string {
	var out []string
	for _, r := range c.Rows {
		if r.Live() {
			out = append(out, r.ID)
		}
	}
	return out
}

// SHA256Hex is the hex sha256 of b.
func SHA256Hex(b []byte) string {
	s := sha256.Sum256(b)
	return hex.EncodeToString(s[:])
}

// Hash is the checklist hash: sha256 over one line per row, «ID|verbatim|contract|placeholder-ok|basis»
// (placeholder-ok is the flag or empty; a «|» inside a field stays escaped as «\|», as in the file),
// then the line «BRIEF.md|<sha256 of BRIEF.md>».
func Hash(rows []Row, brief []byte) string {
	var b strings.Builder
	for _, r := range rows {
		ph := ""
		if r.PlaceholderOK {
			ph = PlaceholderOK
		}
		fmt.Fprintf(&b, "%s|%s|%s|%s|%s\n", r.ID, escapeCell(r.Verbatim), r.Contract, ph, escapeCell(r.Basis))
	}
	fmt.Fprintf(&b, "BRIEF.md|%s\n", SHA256Hex(brief))
	return SHA256Hex([]byte(b.String()))
}

// Status is whether the checklist is approved for the BRIEF it was read with: approved only when the
// approved line is well formed and names the current hash.
func (c *Checklist) Status(brief []byte) (approved bool, hash, reason string) {
	hash = Hash(c.Rows, brief)
	switch {
	case c.ApprovalErr != "":
		return false, hash, "approval void: " + c.ApprovalErr
	case c.Approval == nil:
		return false, hash, "not approved"
	case c.Approval.SHA256 != hash:
		return false, hash, "approval void: the hashed content (a row, a Basis or BRIEF.md) changed since it was approved " +
			"(approved sha256:" + c.Approval.SHA256 + ", now sha256:" + hash + ")"
	}
	return true, hash, ""
}

// NormalizeField collapses runs of white space (line breaks included) into one space and trims it.
func NormalizeField(s string) string { return strings.Join(strings.Fields(s), " ") }

// escapeCell writes a normalized field as a table cell: «|» escaped as «\|» (an already escaped one kept).
func escapeCell(s string) string {
	return strings.ReplaceAll(strings.ReplaceAll(s, `\|`, "|"), "|", `\|`)
}

// unescapeCell is the inverse of escapeCell.
func unescapeCell(s string) string { return strings.ReplaceAll(s, `\|`, "|") }

// ParseContract reads a Contract cell: exactly one of live, deferred, dropped, plus optionally
// placeholder-ok (separated by spaces or commas, any order).
func ParseContract(cell string) (contract string, placeholderOK bool, err error) {
	for _, tok := range strings.FieldsFunc(cell, func(r rune) bool { return r == ' ' || r == ',' || r == '\t' }) {
		switch t := strings.ToLower(tok); t {
		case ContractLive, ContractDeferred, ContractDropped:
			if contract != "" {
				return "", false, fmt.Errorf("contract %q names more than one of live, deferred, dropped", cell)
			}
			contract = t
		case PlaceholderOK:
			placeholderOK = true
		default:
			return "", false, fmt.Errorf("contract %q: unknown word %q (live | deferred | dropped, optional placeholder-ok)", cell, tok)
		}
	}
	if contract == "" {
		return "", false, fmt.Errorf("contract %q: one of live, deferred, dropped is required", cell)
	}
	return contract, placeholderOK, nil
}

// NormalizeRow normalizes and validates one row: a valid ID (never an agent idea «A»), a verbatim quote,
// a contract, and a Basis for a deferred or dropped row (the owner's quote).
func NormalizeRow(r Row) (Row, error) {
	r.ID = strings.TrimSpace(r.ID)
	r.Verbatim, r.Basis = NormalizeField(r.Verbatim), NormalizeField(r.Basis)
	r.Contract = strings.ToLower(strings.TrimSpace(r.Contract))
	switch {
	case strings.HasPrefix(r.ID, "A"):
		return r, fmt.Errorf("row %q: agent ideas (A) never enter the checklist; they live in job reports", r.ID)
	case !ValidID(r.ID):
		return r, fmt.Errorf("row %q: the ID must be R<nn>, R<nn>i, G<nn> or D<nn>", r.ID)
	case r.Verbatim == "":
		return r, fmt.Errorf("row %s: Brief (verbatim) is empty", r.ID)
	}
	if r.Contract != ContractLive && r.Contract != ContractDeferred && r.Contract != ContractDropped {
		return r, fmt.Errorf("row %s: contract %q must be live, deferred or dropped", r.ID, r.Contract)
	}
	if r.Contract != ContractLive && r.Basis == "" {
		return r, fmt.Errorf("row %s: a %s row needs the owner's quote in Basis", r.ID, r.Contract)
	}
	return r, nil
}

// approvedRe is the approved header value: «<RFC 3339 time> owner sha256:<64 hex>».
var approvedRe = regexp.MustCompile(`^(\S+) owner sha256:([0-9a-f]{64})$`)

// Parse reads CHECKLIST.md. Header lines («key: value», optionally as a list item) come before the
// table; other text outside the table is allowed and ignored (not hashed). Errors name the line.
func Parse(text string) (*Checklist, error) {
	c := &Checklist{}
	seen := map[string]bool{}
	ids := map[string]bool{}
	state := 0 // 0 before the table, 1 after its header row, 2 inside it, 3 after it
	fence := false
	for i, raw := range strings.Split(text, "\n") {
		n := i + 1
		line := strings.TrimSpace(strings.TrimRight(raw, "\r"))
		if strings.HasPrefix(line, "```") {
			fence = !fence
			continue
		}
		if fence {
			continue
		}
		if strings.HasPrefix(line, "|") {
			switch state {
			case 0:
				if NormalizeField(line) != TableHeader {
					return nil, fmt.Errorf("line %d: the table header must be %q", n, TableHeader)
				}
				state = 1
			case 1:
				if strings.Trim(strings.ReplaceAll(line, " ", ""), "|-:") != "" {
					return nil, fmt.Errorf("line %d: expected the table's separator row", n)
				}
				state = 2
			case 2:
				r, err := parseRow(line)
				if err != nil {
					return nil, fmt.Errorf("line %d: %w", n, err)
				}
				if ids[r.ID] {
					return nil, fmt.Errorf("line %d: duplicate ID %s", n, r.ID)
				}
				ids[r.ID] = true
				c.Rows = append(c.Rows, r)
			case 3:
				return nil, fmt.Errorf("line %d: a second table; CHECKLIST.md has one", n)
			}
			continue
		}
		if state == 1 {
			return nil, fmt.Errorf("line %d: expected the table's separator row", n)
		}
		if state == 2 {
			state = 3
		}
		if state != 0 {
			continue
		}
		key, val, ok := strings.Cut(strings.TrimPrefix(strings.TrimPrefix(line, "- "), "* "), ":")
		if !ok {
			continue
		}
		key, val = strings.ToLower(strings.TrimSpace(key)), strings.TrimSpace(val)
		switch key {
		case "id", "brief", "version", "approved":
		default:
			continue
		}
		if seen[key] {
			return nil, fmt.Errorf("line %d: header %s given twice", n, key)
		}
		seen[key] = true
		switch key {
		case "id":
			c.ID = val
		case "brief":
			c.Brief = val
		case "version":
			v, err := strconv.Atoi(val)
			if err != nil || v < 1 {
				return nil, fmt.Errorf("line %d: version %q must be a positive integer", n, val)
			}
			c.Version = v
		case "approved":
			c.Approval, c.ApprovalErr = parseApproved(val)
		}
	}
	for _, k := range []string{"id", "brief", "version", "approved"} {
		if !seen[k] {
			return nil, fmt.Errorf("header %s is missing", k)
		}
	}
	if c.ID == "" {
		return nil, errors.New("header id is empty")
	}
	if c.Brief != BriefRel {
		return nil, fmt.Errorf("header brief is %q, must be %s", c.Brief, BriefRel)
	}
	if state == 0 {
		return nil, fmt.Errorf("no table with header %q", TableHeader)
	}
	if state == 1 {
		return nil, errors.New("the table has no separator row")
	}
	return c, nil
}

func parseApproved(val string) (*Approval, string) {
	if strings.EqualFold(val, "no") || val == "" {
		return nil, ""
	}
	m := approvedRe.FindStringSubmatch(val)
	if m == nil {
		return nil, fmt.Sprintf("approved line %q is not «<time> owner sha256:<hex>»", val)
	}
	at, err := time.Parse(time.RFC3339, m[1])
	if err != nil {
		return nil, fmt.Sprintf("approved time %q is not RFC 3339", m[1])
	}
	return &Approval{At: at.UTC(), SHA256: m[2]}, ""
}

// splitCells splits a table row on unescaped «|»; the outer pipes are dropped.
func splitCells(line string) []string {
	line = strings.TrimSpace(line)
	line = strings.TrimPrefix(line, "|")
	if strings.HasSuffix(line, "|") && !strings.HasSuffix(line, `\|`) {
		line = line[:len(line)-1]
	}
	var cells []string
	var b strings.Builder
	for i := 0; i < len(line); i++ {
		if line[i] == '\\' && i+1 < len(line) && line[i+1] == '|' {
			b.WriteString(`\|`)
			i++
			continue
		}
		if line[i] == '|' {
			cells = append(cells, b.String())
			b.Reset()
			continue
		}
		b.WriteByte(line[i])
	}
	return append(cells, b.String())
}

func parseRow(line string) (Row, error) {
	cells := splitCells(line)
	if len(cells) != 4 {
		return Row{}, fmt.Errorf("a row has 4 cells (ID, Brief, Contract, Basis), this one %d", len(cells))
	}
	contract, ph, err := ParseContract(cells[2])
	if err != nil {
		return Row{}, err
	}
	return NormalizeRow(Row{ID: cells[0], Verbatim: unescapeCell(cells[1]), Contract: contract,
		PlaceholderOK: ph, Basis: unescapeCell(cells[3])})
}

// Render writes the checklist in the canonical form (what the tools write).
func Render(c *Checklist) string {
	var b strings.Builder
	b.WriteString("# Checklist\n\n")
	fmt.Fprintf(&b, "- id: %s\n- brief: %s\n- version: %d\n", NormalizeField(c.ID), BriefRel, c.Version)
	if c.Approval != nil {
		fmt.Fprintf(&b, "- approved: %s owner sha256:%s\n", c.Approval.At.UTC().Format(time.RFC3339), c.Approval.SHA256)
	} else {
		b.WriteString("- approved: no\n")
	}
	b.WriteString("\n" + TableHeader + "\n|---|---|---|---|\n")
	for _, r := range c.Rows {
		contract := r.Contract
		if r.PlaceholderOK {
			contract += " " + PlaceholderOK
		}
		fmt.Fprintf(&b, "| %s | %s | %s | %s |\n", r.ID, escapeCell(r.Verbatim), contract, escapeCell(r.Basis))
	}
	return b.String()
}

// AdditionsHeading is the BRIEF.md section later owner additions are appended under.
const AdditionsHeading = "## Additions"

// Addition is one later owner addition to the brief: the date and the owner's words verbatim.
type Addition struct {
	Date  string `json:"date"`
	Quote string `json:"quote"`
}

// InitBrief is a new BRIEF.md: a title and the owner's words verbatim.
func InitBrief(words string) string {
	return "# Brief\n\n" + strings.TrimRight(words, "\n") + "\n"
}

// AppendAdditions appends adds under «## Additions» (added at the end when the brief has none); the
// text above is never changed. Each addition is «### <date>» and the quote verbatim.
func AppendAdditions(brief string, adds []Addition) string {
	if len(adds) == 0 {
		return brief
	}
	out := strings.TrimRight(brief, "\n") + "\n"
	if !hasHeading(brief, AdditionsHeading) {
		out += "\n" + AdditionsHeading + "\n"
	}
	for _, a := range adds {
		out += "\n### " + a.Date + "\n\n" + strings.TrimRight(a.Quote, "\n") + "\n"
	}
	return out
}

// hasHeading reports whether text has the line heading outside a ``` fence.
func hasHeading(text, heading string) bool {
	fence := false
	for _, l := range strings.Split(text, "\n") {
		t := strings.TrimSpace(l)
		if strings.HasPrefix(t, "```") {
			fence = !fence
			continue
		}
		if !fence && strings.TrimRight(l, "\r \t") == heading {
			return true
		}
	}
	return false
}

// LongBriefWarning is checklist_set's warning for fewer than MinRowsLongBrief rows from a long brief.
func LongBriefWarning(brief []byte, rows int) string {
	if rows >= MinRowsLongBrief || utf8.RuneCount(brief) < LongBriefRunes {
		return ""
	}
	return fmt.Sprintf("only %d rows from a brief of %d characters: fewer than %d rows from a long brief is a retelling, "+
		"not a checklist (one row = one thing that can be true or false on its own)", rows, utf8.RuneCount(brief), MinRowsLongBrief)
}
