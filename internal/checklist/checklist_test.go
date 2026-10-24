package checklist

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// noRedact is a Redactor that changes nothing.
func noRedact(s string) (string, int) { return s, 0 }

// fakeRedact replaces the word SECRETVALUE, standing in for the Wave D redactor.
func fakeRedact(s string) (string, int) {
	n := strings.Count(s, "SECRETVALUE")
	return strings.ReplaceAll(s, "SECRETVALUE", "[redacted:secret]"), n
}

const sample = `# Checklist

- id: demo
- brief: brief/BRIEF.md
- version: 2
- approved: no

Free text outside the table is not hashed.

| ID | Brief (verbatim) | Contract | Basis |
|---|---|---|---|
| R01 | «a login page» | live | brief |
| R02i | «sessions survive a restart» | live placeholder-ok | implied by R01 |
| G01 | «dark mode \| light mode» | deferred | owner: «later» |
| D01 | builds on Go 1.27 | live | build proof |
`

func TestParseSample(t *testing.T) {
	c, err := Parse(sample)
	if err != nil {
		t.Fatal(err)
	}
	if c.ID != "demo" || c.Version != 2 || c.Approval != nil || c.ApprovalErr != "" || len(c.Rows) != 4 {
		t.Fatalf("header %+v", c)
	}
	if r := c.Rows[1]; r.ID != "R02i" || !r.PlaceholderOK || r.Contract != ContractLive {
		t.Fatalf("row 2 %+v", r)
	}
	if r := c.Rows[2]; r.Verbatim != "«dark mode | light mode»" || r.Contract != ContractDeferred {
		t.Fatalf("escaped pipe %+v", r)
	}
	// Live requirements: R / Ri / G with live; D rows never, deferred neither.
	if got := strings.Join(c.LiveIDs(), ","); got != "R01,R02i" {
		t.Fatalf("live %s", got)
	}
	// Render → Parse is stable.
	again, err := Parse(Render(c))
	if err != nil {
		t.Fatal(err)
	}
	if Hash(again.Rows, nil) != Hash(c.Rows, nil) {
		t.Fatal("render changed the hashed content")
	}
}

func TestParseErrors(t *testing.T) {
	for name, tc := range map[string]struct{ text, want string }{
		"agent idea":   {strings.Replace(sample, "| D01 |", "| A01 |", 1), "agent ideas"},
		"bad id":       {strings.Replace(sample, "| D01 |", "| X1 |", 1), "the ID must be"},
		"dup id":       {strings.Replace(sample, "| D01 |", "| R01 |", 1), "duplicate ID R01"},
		"contract":     {strings.Replace(sample, "| live | brief |", "| maybe | brief |", 1), "unknown word"},
		"two":          {strings.Replace(sample, "| live | brief |", "| live dropped | brief |", 1), "more than one"},
		"drop basis":   {strings.Replace(sample, "| deferred | owner: «later» |", "| dropped |  |", 1), "owner's quote in Basis"},
		"cells":        {strings.Replace(sample, "| live | brief |", "| live |", 1), "4 cells"},
		"version":      {strings.Replace(sample, "version: 2", "version: x", 1), "positive integer"},
		"no approved":  {strings.Replace(sample, "- approved: no\n", "", 1), "header approved is missing"},
		"brief path":   {strings.Replace(sample, "brief: brief/BRIEF.md", "brief: other.md", 1), "must be brief/BRIEF.md"},
		"table header": {strings.Replace(sample, "| Basis |\n", "| Why |\n", 1), "table header"},
		"empty quote":  {strings.Replace(sample, "| «a login page» |", "|   |", 1), "is empty"},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := Parse(tc.text)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err %v, want %q", err, tc.want)
			}
		})
	}
}

// The hash covers ID, verbatim, contract, placeholder-ok and Basis of every row and BRIEF.md; it is
// insensitive to white-space layout and to the unhashed header and free text.
func TestHashFields(t *testing.T) {
	c, err := Parse(sample)
	if err != nil {
		t.Fatal(err)
	}
	brief := []byte("# Brief\n\nwords\n")
	base := Hash(c.Rows, brief)
	mut := func(f func(r *Row)) string {
		rows := append([]Row(nil), c.Rows...)
		f(&rows[0])
		return Hash(rows, brief)
	}
	for name, h := range map[string]string{
		"id":          mut(func(r *Row) { r.ID = "R09" }),
		"verbatim":    mut(func(r *Row) { r.Verbatim += "!" }),
		"contract":    mut(func(r *Row) { r.Contract = ContractDropped }),
		"placeholder": mut(func(r *Row) { r.PlaceholderOK = true }),
		"basis":       mut(func(r *Row) { r.Basis = "changed" }),
		"brief":       Hash(c.Rows, append(brief, '.')),
	} {
		if h == base {
			t.Errorf("%s change left the hash unchanged", name)
		}
	}
	spaced := strings.Replace(sample, "| R01 | «a login page» | live | brief |", "|R01|   «a login   page»  |live|  brief |", 1)
	spaced = strings.Replace(spaced, "version: 2", "version: 7", 1)
	spaced = strings.Replace(spaced, "Free text", "Other free text", 1)
	c2, err := Parse(spaced)
	if err != nil {
		t.Fatal(err)
	}
	if Hash(c2.Rows, brief) != base {
		t.Fatal("layout, version or free text changed the hash")
	}
}

// approvedSample is the sample with an approved line and the stored approval record it stands for.
func approvedSample(t *testing.T, brief []byte) (string, *Record) {
	t.Helper()
	c, err := Parse(sample)
	if err != nil {
		t.Fatal(err)
	}
	at := time.Date(2026, 10, 24, 8, 0, 0, 0, time.UTC)
	c.Approval = &Approval{At: at, SHA256: Hash(c.Rows, brief)}
	return Render(c), &Record{ChecklistID: c.ID, Version: c.Version, SHA256: c.Approval.SHA256,
		BriefSHA256: BriefPrefixSHA256(brief), At: at}
}

func TestStatusManualEditVoids(t *testing.T) {
	brief := []byte("# Brief\n\nwords\n")
	text, rec := approvedSample(t, brief)
	c, err := Parse(text)
	if err != nil {
		t.Fatal(err)
	}
	if ok, _, reason := c.Status(brief, rec); !ok {
		t.Fatalf("approved checklist not approved: %s", reason)
	}
	for name, edited := range map[string]string{
		"basis":    strings.Replace(text, "| implied by R01 |", "| implied |", 1),
		"contract": strings.Replace(text, "| live placeholder-ok |", "| live |", 1),
		"version":  strings.Replace(text, "- version: 2", "- version: 5", 1),
		"id":       strings.Replace(text, "- id: demo", "- id: other", 1),
	} {
		c, err := Parse(edited)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if ok, _, reason := c.Status(brief, rec); ok || !strings.HasPrefix(reason, "approval void") {
			t.Errorf("%s edit: approved=%v reason=%q", name, ok, reason)
		}
	}
	if ok, _, reason := c.Status(append(brief, 'x'), rec); ok || !strings.Contains(reason, "changed since version 2 was approved") {
		t.Errorf("BRIEF edit: approved=%v reason=%q", ok, reason)
	}
	// The unhashed header and free text do not void it; the approved line is informational: a malformed
	// or missing one changes nothing while the stored approval matches.
	for name, edited := range map[string]string{
		"free text": strings.Replace(text, "- version: 2", "- version: 2\n\nA note.", 1),
		"malformed": strings.Replace(text, " owner sha256:", " sha256:", 1),
		"no line":   strings.Replace(text, "- approved: 2026-10-24T08:00:00Z owner sha256:"+rec.SHA256, "- approved: no", 1),
	} {
		c, _ := Parse(edited)
		if ok, _, reason := c.Status(brief, rec); !ok {
			t.Errorf("%s voided the approval: %s", name, reason)
		}
	}
}

// The hash is unkeyed: whoever edits CHECKLIST.md can recompute it. An approved line with the right
// hash but no stored approval, or a stored approval of another version, approves nothing.
func TestStatusForgedApproval(t *testing.T) {
	brief := []byte("# Brief\n\nwords\n")
	text, rec := approvedSample(t, brief)
	c, err := Parse(text)
	if err != nil {
		t.Fatal(err)
	}
	if c.Approval == nil || c.Approval.SHA256 != Hash(c.Rows, brief) {
		t.Fatal("the forged line does not name the current hash")
	}
	if ok, _, reason := c.Status(brief, nil); ok || !strings.HasPrefix(reason, "approval void") ||
		!strings.Contains(reason, "not backed by an approval") {
		t.Fatalf("forged line: approved=%v %q", ok, reason)
	}
	older := *rec
	older.Version = 1
	if ok, _, reason := c.Status(brief, &older); ok || !strings.Contains(reason, "version 1 was approved") {
		t.Fatalf("other version: approved=%v %q", ok, reason)
	}
	plain, _ := Parse(sample)
	if ok, _, reason := plain.Status(brief, nil); ok || reason != "not approved" {
		t.Fatalf("no approval: approved=%v %q", ok, reason)
	}
}

func TestBriefPrefix(t *testing.T) {
	b := InitBrief("I want a login page.")
	once := AppendAdditions(b, []Addition{{Date: "2026-10-24", Quote: "also dark mode"}})
	twice := AppendAdditions(once, []Addition{{Date: "2026-10-25", Quote: "and export"}})
	if BriefPrefixSHA256([]byte(b)) != BriefPrefixSHA256([]byte(once)) || BriefPrefixSHA256([]byte(once)) != BriefPrefixSHA256([]byte(twice)) {
		t.Fatalf("additions changed the prefix: %q / %q", BriefPrefix([]byte(b)), BriefPrefix([]byte(once)))
	}
	if BriefPrefixSHA256([]byte(strings.Replace(once, "login", "logout", 1))) == BriefPrefixSHA256([]byte(once)) {
		t.Fatal("an edit above ## Additions left the prefix unchanged")
	}
	fenced := "# Brief\n\n```\n## Additions\n```\nwords\n"
	if BriefPrefix([]byte(fenced)) != strings.TrimRight(fenced, "\n") {
		t.Fatalf("a fenced heading cut the prefix: %q", BriefPrefix([]byte(fenced)))
	}
}

func TestAppendAdditions(t *testing.T) {
	b := InitBrief("I want a login page.\n\n## Additions\nin a code fence:\n")
	b = strings.Replace(b, "## Additions\n", "```\n## Additions\n```\n", 1)
	once := AppendAdditions(b, []Addition{{Date: "2026-10-24", Quote: "also dark mode"}})
	if !strings.HasPrefix(once, b[:len(b)-1]) || strings.Count(once, "\n## Additions\n") != 2 {
		t.Fatalf("first addition:\n%s", once)
	}
	twice := AppendAdditions(once, []Addition{{Date: "2026-10-25", Quote: "and export"}})
	if !strings.HasPrefix(twice, once) || strings.Count(twice, "\n## Additions\n") != 2 ||
		!strings.HasSuffix(twice, "### 2026-10-25\n\nand export\n") {
		t.Fatalf("second addition:\n%s", twice)
	}
}

func TestLongBriefWarning(t *testing.T) {
	long := []byte(strings.Repeat("слово ", 300))
	if LongBriefWarning(long, 9) == "" || LongBriefWarning(long, 10) != "" || LongBriefWarning([]byte("short"), 1) != "" {
		t.Fatal("warning thresholds")
	}
}

func repoWith(t *testing.T, brief, list string) string {
	t.Helper()
	repo := t.TempDir()
	if err := os.Mkdir(filepath.Join(repo, "brief"), 0o755); err != nil {
		t.Fatal(err)
	}
	for p, s := range map[string]string{BriefPath(repo): brief, ChecklistPath(repo): list} {
		if err := os.WriteFile(p, []byte(s), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return repo
}

func TestApplyVersionApprovalAndRedaction(t *testing.T) {
	brief, list, _, err := Init("demo", "I want a login page.", noRedact)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 10, 24, 9, 0, 0, 0, time.UTC)
	f := &Files{Brief: brief, Checklist: list}
	res, err := Apply(f, Change{Rows: []Row{{ID: "R01", Verbatim: "a login page SECRETVALUE", Contract: "live", Basis: "brief"}}}, now, fakeRedact, nil)
	if err != nil {
		t.Fatal(err)
	}
	if res.List.Version != 2 || res.Approved || res.Record != nil || res.Redacted != 1 || strings.Contains(string(res.Checklist), "SECRETVALUE") {
		t.Fatalf("draft: v%d approved=%v redacted=%d\n%s", res.List.Version, res.Approved, res.Redacted, res.Checklist)
	}
	if res.Brief != nil {
		t.Fatal("BRIEF rewritten without additions")
	}
	// Approve without a change: same version, approved over the content as written; the record to store
	// names the version, the hash and the BRIEF text above ## Additions.
	f.Checklist = res.Checklist
	res, err = Apply(f, Change{Approve: true}, now, fakeRedact, nil)
	if err != nil {
		t.Fatal(err)
	}
	rec := res.Record
	if res.List.Version != 2 || !res.Approved || res.List.Approval.SHA256 != Hash(res.List.Rows, f.Brief) || rec == nil ||
		rec.Version != 2 || rec.SHA256 != res.Hash || rec.ChecklistID != "demo" || rec.BriefSHA256 != BriefPrefixSHA256(f.Brief) {
		t.Fatalf("approve: v%d approved=%v %s %+v", res.List.Version, res.Approved, res.Reason, rec)
	}
	// A call without a change keeps it (the stored record decides).
	f.Checklist = res.Checklist
	if res, err := Apply(f, Change{}, now, fakeRedact, rec); err != nil || !res.Approved || res.VersionBumped || res.Checklist != nil {
		t.Fatalf("no change: %+v %v", res, err)
	}
	// An addition with a G row: BRIEF and CHECKLIST change together, version up, approval dropped.
	res, err = Apply(f, Change{
		Rows:      []Row{{ID: "G01", Verbatim: "dark mode", Contract: "live", Basis: "addition 2026-10-25"}},
		Additions: []Addition{{Quote: "add dark mode SECRETVALUE"}},
	}, now, fakeRedact, rec)
	if err != nil {
		t.Fatal(err)
	}
	if res.Brief == nil || !strings.Contains(string(res.Brief), "### 2026-10-24\n\nadd dark mode [redacted:secret]\n") {
		t.Fatalf("BRIEF:\n%s", res.Brief)
	}
	if res.List.Version != 3 || res.Approved || !strings.HasPrefix(res.Reason, "not approved") || res.List.Approval != nil {
		t.Fatalf("after change: v%d approved=%v %q", res.List.Version, res.Approved, res.Reason)
	}
	// A new G row without the owner's words is refused.
	if _, err := Apply(f, Change{Rows: []Row{{ID: "G02", Verbatim: "x", Contract: "live"}}}, now, fakeRedact, rec); err == nil {
		t.Fatal("G row without an addition accepted")
	}
	// expect_version guards against a concurrent change.
	if _, err := Apply(f, Change{ExpectVersion: 1}, now, fakeRedact, rec); err == nil || !strings.Contains(err.Error(), "version 2") {
		t.Fatalf("expect_version: %v", err)
	}
}

func TestApplyManualEditBumpsVersion(t *testing.T) {
	brief := []byte("# Brief\n\nwords\n")
	text, rec := approvedSample(t, brief)
	edited := strings.Replace(text, "| implied by R01 |", "| implied |", 1)
	res, err := Apply(&Files{Brief: brief, Checklist: []byte(edited)}, Change{}, time.Now(), noRedact, rec)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(res.PreviousVoid, "approval void") || !res.VersionBumped || res.List.Version != 3 || res.Approved {
		t.Fatalf("manual edit: %+v", res)
	}
	// A forged approved line (right hash, nothing stored) is void too: dropped, version up.
	res, err = Apply(&Files{Brief: brief, Checklist: []byte(text)}, Change{}, time.Now(), noRedact, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(res.PreviousVoid, "approval void") || !res.VersionBumped || res.Approved || res.List.Approval != nil {
		t.Fatalf("forged line: %+v", res)
	}
}

// The BRIEF text above ## Additions is never edited after an approval: checklist_set refuses until it
// is restored; additions below it are fine.
func TestApplyRefusesEditedBriefPrefix(t *testing.T) {
	brief := []byte(InitBrief("I want a login page."))
	text, rec := approvedSample(t, brief)
	edited := []byte(strings.Replace(string(brief), "login", "logout", 1))
	_, err := Apply(&Files{Brief: edited, Checklist: []byte(text)}, Change{}, time.Now(), noRedact, rec)
	if err == nil || !strings.Contains(err.Error(), "above ## Additions") {
		t.Fatalf("edited BRIEF accepted: %v", err)
	}
	res, err := Apply(&Files{Brief: brief, Checklist: []byte(text)}, Change{Additions: []Addition{{Quote: "also export"}}}, time.Now(), noRedact, rec)
	if err != nil || res.Brief == nil {
		t.Fatalf("addition refused: %v", err)
	}
	// Without a stored approval there is nothing to compare against.
	if _, err := Apply(&Files{Brief: edited, Checklist: []byte(text)}, Change{}, time.Now(), noRedact, nil); err != nil {
		t.Fatalf("unapproved: %v", err)
	}
}

func TestWriteBothAtomic(t *testing.T) {
	repo := repoWith(t, "old brief\n", "old list\n")
	old := renameHook
	defer func() { renameHook = old }()
	calls := 0
	renameHook = func(a, b string) error {
		calls++
		if calls == 2 {
			return errors.New("disk full")
		}
		return os.Rename(a, b)
	}
	err := WriteBoth(repo, []byte("new brief\n"), []byte("new list\n"), []byte("old brief\n"))
	if err == nil {
		t.Fatal("second rename failure not reported")
	}
	f, err := Read(repo)
	if err != nil {
		t.Fatal(err)
	}
	if string(f.Brief) != "old brief\n" || string(f.Checklist) != "old list\n" {
		t.Fatalf("not both-or-neither: %q %q", f.Brief, f.Checklist)
	}
	entries, _ := os.ReadDir(filepath.Join(repo, "brief"))
	if len(entries) != 2 {
		t.Fatalf("temp files left: %v", entries)
	}
	renameHook = os.Rename
	if err := WriteBoth(repo, []byte("new brief\n"), []byte("new list\n"), []byte("old brief\n")); err != nil {
		t.Fatal(err)
	}
	if f, _ = Read(repo); string(f.Brief) != "new brief\n" || string(f.Checklist) != "new list\n" {
		t.Fatalf("write: %q %q", f.Brief, f.Checklist)
	}
}

func TestReadRefusesSymlink(t *testing.T) {
	repo := repoWith(t, "b\n", "c\n")
	target := filepath.Join(t.TempDir(), "elsewhere")
	if err := os.WriteFile(target, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(BriefPath(repo)); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, BriefPath(repo)); err != nil {
		t.Fatal(err)
	}
	if _, err := Read(repo); err == nil {
		t.Fatal("symlinked BRIEF.md read")
	}
	if err := WriteBoth(repo, []byte("n\n"), nil, nil); err == nil {
		t.Fatal("write through a symlink accepted")
	}
}

// A backslash is escaped as well as a pipe: «a \| b» and «a | b» hash differently, and a literal
// backslash-pipe or a trailing backslash survives Render → Parse.
func TestCellEscapesBackslash(t *testing.T) {
	brief := []byte("# Brief\n\nwords\n")
	lit := []Row{{ID: "R01", Verbatim: `a \| b`, Contract: ContractLive, Basis: `ends with \`}}
	pipe := []Row{{ID: "R01", Verbatim: "a | b", Contract: ContractLive, Basis: `ends with \`}}
	if Hash(lit, brief) == Hash(pipe, brief) {
		t.Fatal(`«a \| b» and «a | b» hash the same`)
	}
	for _, rows := range [][]Row{lit, pipe, {{ID: "R01", Verbatim: `x \\ y \`, Contract: ContractLive}}} {
		c := &Checklist{ID: "demo", Brief: BriefRel, Version: 1, Rows: rows}
		again, err := Parse(Render(c))
		if err != nil {
			t.Fatal(err)
		}
		if len(again.Rows) != 1 || again.Rows[0] != rows[0] {
			t.Fatalf("round trip %+v, want %+v\n%s", again.Rows, rows[0], Render(c))
		}
	}
}

// An unclosed ``` fence in the owner's words must not hide the «## Additions» heading: the prefix
// stays the owner's words after an addition, and a second addition reuses the heading.
func TestBriefPrefixUnclosedFence(t *testing.T) {
	b := InitBrief("I want a login page.\n\n```\nsome code without a closing fence")
	once := AppendAdditions(b, []Addition{{Date: "2026-10-24", Quote: "also dark mode"}})
	if BriefPrefixSHA256([]byte(b)) != BriefPrefixSHA256([]byte(once)) {
		t.Fatalf("an addition after an unclosed fence changed the prefix: %q / %q", BriefPrefix([]byte(b)), BriefPrefix([]byte(once)))
	}
	twice := AppendAdditions(once, []Addition{{Date: "2026-10-25", Quote: "and export"}})
	if strings.Count(twice, AdditionsHeading+"\n") != 1 || BriefPrefixSHA256([]byte(twice)) != BriefPrefixSHA256([]byte(b)) {
		t.Fatalf("second addition:\n%s", twice)
	}
	// A heading found outside a fence before an unclosed one (in an addition's quote) is kept.
	quoted := AppendAdditions(b, []Addition{{Date: "2026-10-24", Quote: "```\nunclosed in the quote"}})
	if BriefPrefix([]byte(quoted)) != BriefPrefix([]byte(b)) {
		t.Fatalf("an unclosed fence in an addition moved the prefix: %q", BriefPrefix([]byte(quoted)))
	}
}

// A quote that leaves a fence open is closed after it: a later heading or addition is not swallowed.
func TestAppendAdditionsClosesQuoteFence(t *testing.T) {
	b := InitBrief("I want a login page.")
	once := AppendAdditions(b, []Addition{{Date: "2026-10-24", Quote: "```\nunclosed"}})
	if !strings.HasSuffix(once, "unclosed\n```\n") || strings.Count(once, "```")%2 != 0 {
		t.Fatalf("quote fence left open:\n%s", once)
	}
	twice := AppendAdditions(once, []Addition{{Date: "2026-10-25", Quote: "and export"}})
	if strings.Count(twice, AdditionsHeading+"\n") != 1 || BriefPrefix([]byte(twice)) != BriefPrefix([]byte(b)) {
		t.Fatalf("second addition:\n%s", twice)
	}
}

// After an approval of a brief with an unclosed fence, additions keep the stored prefix: the next call
// is not refused as an edit of the owner's words.
func TestApplyUnclosedFenceAdditions(t *testing.T) {
	brief := []byte(InitBrief("I want a login page.\n\n```\nunclosed"))
	text, rec := approvedSample(t, brief)
	f := &Files{Brief: brief, Checklist: []byte(text)}
	res, err := Apply(f, Change{Additions: []Addition{{Quote: "also export"}}}, time.Now(), noRedact, rec)
	if err != nil {
		t.Fatal(err)
	}
	f.Brief, f.Checklist = res.BriefAfter, res.Checklist
	if _, err := Apply(f, Change{Additions: []Addition{{Quote: "and dark mode"}}}, time.Now(), noRedact, rec); err != nil {
		t.Fatalf("second addition refused: %v", err)
	}
}
