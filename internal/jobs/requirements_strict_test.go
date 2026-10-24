package jobs

import (
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/qwwiwi/agent-jobs/internal/store"
)

// reqBlock is a report whose Requirements block holds lines.
func reqBlock(lines ...string) string {
	return "## Summary\nok\n\n## Findings / Changes\n- x\n\n### Requirements\n" + strings.Join(lines, "\n") +
		"\n\n## Evidence\n-\n\n## Not done / uncertain\nnothing\n"
}

const doneR01 = "- R01 · done · a.go:1 · ran: go test ./... → ok"

// A done line is verified only by an exact REDPROOF marker with real proof content that names that
// requirement: a look-alike marker, an empty or negative proof, or a proof naming another ID does not
// verify it.
func TestRedProofStrict(t *testing.T) {
	verified := func(report string) bool {
		t.Helper()
		r := ParseRequirements(report, []string{"R01", "R02"})
		for _, l := range r.Lines {
			if l.ID == "R01" {
				return l.Verified
			}
		}
		t.Fatalf("no R01 line in %+v", r)
		return false
	}
	for name, report := range map[string]string{
		"block line":           reqBlock(doneR01, "REDPROOF R01 · TestLogin · FAIL before the change, ok after"),
		"block line, colon":    reqBlock(doneR01, "- REDPROOF: R01 TestLogin failed before"),
		"block, two ids":       reqBlock(doneR01, "REDPROOF R02, R01 · TestBoth failed before"),
		"inline with id":       reqBlock(doneR01 + " · REDPROOF R01: TestLogin failed before"),
		"inline, id in proof":  reqBlock(doneR01 + " · REDPROOF: TestLogin (R01) failed before"),
		"marker in backticks":  reqBlock(doneR01, "`REDPROOF R01 · TestLogin failed before`"),
		"not first, then real": reqBlock(doneR01+" · REDPROOF: none", "REDPROOF R01 · TestLogin failed before"),
		"dashed test name":     reqBlock(doneR01, "REDPROOF R01 · `npm test -- login-form` failed before"),
	} {
		if !verified(report) {
			t.Errorf("%s: not verified", name)
		}
	}
	for name, report := range map[string]string{
		"look-alike block":    reqBlock(doneR01, "REDPROOFLESS R01 · TestLogin"),
		"look-alike inline":   reqBlock(doneR01 + " · REDPROOFS: R01 TestLogin"),
		"lower case":          reqBlock(doneR01, "redproof R01 · TestLogin failed before"),
		"empty block":         reqBlock(doneR01, "REDPROOF R01"),
		"empty inline":        reqBlock(doneR01 + " · REDPROOF:"),
		"empty inline id":     reqBlock(doneR01 + " · REDPROOF R01:"),
		"none":                reqBlock(doneR01, "REDPROOF R01 · none"),
		"n/a inline":          reqBlock(doneR01 + " · REDPROOF R01: n/a"),
		"N/A upper":           reqBlock(doneR01 + " · REDPROOF R01: N/A"),
		"no":                  reqBlock(doneR01, "REDPROOF: R01 no"),
		"not run":             reqBlock(doneR01, "REDPROOF R01: not run, the test is new"),
		"dash":                reqBlock(doneR01, "REDPROOF R01 · -"),
		"em dash none":        reqBlock(doneR01, "REDPROOF R01 — none"),
		"double dash none":    reqBlock(doneR01, "REDPROOF R01 -- none"),
		"not-run":             reqBlock(doneR01 + " · REDPROOF R01: not-run"),
		"no-test":             reqBlock(doneR01, "REDPROOF R01 · no-test"),
		"NA":                  reqBlock(doneR01, "REDPROOF R01 · NA"),
		"nothing":             reqBlock(doneR01, "REDPROOF R01: nothing failed"),
		"inline names R02":    reqBlock(doneR01 + " · REDPROOF: R02 TestOther failed before"),
		"inline without id":   reqBlock(doneR01 + " · REDPROOF: TestLogin failed before"),
		"block names R02":     reqBlock(doneR01, "REDPROOF R02 · TestOther failed before"),
		"block names R01i":    reqBlock(doneR01, "REDPROOF R01i · TestOther failed before"),
		"block names R010":    reqBlock(doneR01, "REDPROOF R010 · TestOther failed before"),
		"block without an id": reqBlock(doneR01, "REDPROOF · TestLogin failed before"),
	} {
		if verified(report) {
			t.Errorf("%s: verified", name)
		}
	}
	// A negative or look-alike REDPROOF line is not recorded as a red proof.
	r := ParseRequirements(reqBlock(doneR01, "REDPROOF R01 · none", "REDPROOFLESS R01 · x", "REDPROOF R01 · TestA failed"), []string{"R01"})
	if len(r.RedProofs) != 1 || !strings.Contains(r.RedProofs[0], "TestA") {
		t.Fatalf("redproofs %v", r.RedProofs)
	}
}

// A Requirements block that is empty, or has no parsable line for any covered ID (a table, prose), is
// off contract like a missing one.
func TestRequirementsBlockWithoutLinesOffContract(t *testing.T) {
	for name, report := range map[string]string{
		"empty":       reqBlock(),
		"prose":       reqBlock("All requirements are done."),
		"table":       reqBlock("| ID | Status |", "|---|---|", "| R01 | done |"),
		"other ids":   reqBlock("- R09 · done · x · ran: y → ok"),
		"only proofs": reqBlock("REDPROOF R01 · TestA failed before"),
	} {
		n := reportContractNote(report, []string{"R01"})
		if n != NoteRequirementsEmpty || OffContractNote("x; "+n) != NoteRequirementsEmpty {
			t.Errorf("%s: note %q", name, n)
		}
	}
	if n := reportContractNote(reqBlock("- R01 · partial · form only"), []string{"R01", "R02"}); n != "" {
		t.Fatalf("a block with a covered line: %q", n)
	}
	if n := reportContractNote(reqBlock(), nil); n != "" {
		t.Fatalf("no covers: %q", n)
	}
}

// The run-wrapper fails closed: when it cannot read the job's row, it cannot know the covers, so the
// report is off contract with a note saying why.
func TestWrapperContractNoteFailsClosed(t *testing.T) {
	plain := "## Summary\nok\n\n## Findings / Changes\n- x\n\n## Not done / uncertain\nnothing\n"
	n := wrapperContractNote(plain, func() (*store.Job, error) { return nil, errors.New("database is locked") })
	if !strings.HasPrefix(n, NoteJobRowUnread) || OffContractNote("x; "+n) != NoteJobRowUnread {
		t.Fatalf("unreadable row: %q", n)
	}
	if n := wrapperContractNote(plain, func() (*store.Job, error) { return &store.Job{}, nil }); n != "" {
		t.Fatalf("no covers: %q", n)
	}
	if n := wrapperContractNote(plain, func() (*store.Job, error) { return &store.Job{Covers: []string{"R01"}}, nil }); n != NoteNoRequirements {
		t.Fatalf("covers: %q", n)
	}
}

// Only real git trailers count: a «Requirements:» line in a commit body that is not in the trailer
// block is ignored.
func TestJobRequirementsTrailersRealTrailersOnly(t *testing.T) {
	m := newManager(t)
	repo := newRepo(t)
	base := gitT(t, repo, "rev-parse", "HEAD")
	gitT(t, repo, "commit", "-q", "--allow-empty", "-m", "Add login\n\nRequirements: R09 is only mentioned in the body\nmore prose here\n\nRequirements: R01, R03i")
	gitT(t, repo, "commit", "-q", "--allow-empty", "-m", "Fix\n\nRequirements: R07 in a body paragraph\n\nNo trailers here, just text.")
	gitT(t, repo, "commit", "-q", "--allow-empty", "-m", "Tidy\n\nrequirements: G02\nCo-Authored-By: T <t@example.com>")
	head := gitT(t, repo, "rev-parse", "HEAD")
	j := &store.Job{ID: "261024-120000-beef", RepoPath: repo, BaseCommit: base}
	gitT(t, repo, "update-ref", "refs/agent-jobs/jobs/"+j.ID, head)
	got, note := m.JobRequirementsTrailers(j)
	if note != "" || !slices.Equal(got, []string{"G02", "R01", "R03i"}) {
		t.Fatalf("trailers %v note %q", got, note)
	}
}

// One strict ID matcher for validation and extraction: an ID glued to a word character (R01_other,
// xR01) is not that ID; a Markdown-wrapped or newly listed negative proof (tbd, skipped, pending,
// missing, n.a., ?) is not a proof; a negative ran: value does not verify a done line.
func TestRedProofStrictIDsAndNegatives(t *testing.T) {
	verified := func(report string) bool {
		t.Helper()
		for _, l := range ParseRequirements(report, []string{"R01", "R02"}).Lines {
			if l.ID == "R01" {
				return l.Verified
			}
		}
		t.Fatalf("no R01 line in %q", report)
		return false
	}
	for name, report := range map[string]string{
		"underscore suffix":  reqBlock(doneR01, "REDPROOF R01_other: test failed"),
		"underscore prefix":  reqBlock(doneR01, "REDPROOF x_R01: test failed"),
		"inline underscore":  reqBlock(doneR01 + " · REDPROOF R01_other: test failed"),
		"bold none":          reqBlock(doneR01, "REDPROOF R01: **none**"),
		"italic n/a":         reqBlock(doneR01, "REDPROOF R01: _n/a_"),
		"backticked none":    reqBlock(doneR01 + " · REDPROOF R01: `none`"),
		"tbd":                reqBlock(doneR01, "REDPROOF R01: TBD"),
		"skipped":            reqBlock(doneR01, "REDPROOF R01: skipped, no time"),
		"pending":            reqBlock(doneR01, "REDPROOF R01: pending"),
		"missing":            reqBlock(doneR01, "REDPROOF R01: missing"),
		"n.a.":               reqBlock(doneR01, "REDPROOF R01: n.a."),
		"question mark":      reqBlock(doneR01, "REDPROOF R01: ?"),
		"question marks":     reqBlock(doneR01, "REDPROOF R01: ???"),
		"ran none":           reqBlock("- R01 · done · a.go:1 · ran: none", "REDPROOF R01: TestLogin failed before"),
		"ran n/a":            reqBlock("- R01 · done · a.go:1 · ran: n/a", "REDPROOF R01: TestLogin failed before"),
		"ran bold skipped":   reqBlock("- R01 · done · a.go:1 · ran: **skipped**", "REDPROOF R01: TestLogin failed before"),
		"ran question mark":  reqBlock("- R01 · done · a.go:1 · ran: ?", "REDPROOF R01: TestLogin failed before"),
		"ran backticked tbd": reqBlock("- R01 · done · a.go:1 · ran: `tbd`", "REDPROOF R01: TestLogin failed before"),
	} {
		if verified(report) {
			t.Errorf("%s: verified", name)
		}
	}
	for name, report := range map[string]string{
		"plain":            reqBlock(doneR01, "REDPROOF R01: TestLogin failed before"),
		"bold id":          reqBlock(doneR01, "REDPROOF **R01**: TestLogin failed before"),
		"id list dashes":   reqBlock(doneR01, "REDPROOF R02-R01: TestBoth failed before"),
		"test named nonce": reqBlock(doneR01, "REDPROOF R01: nonce_test failed before"),
		"ran backticked":   reqBlock("- R01 · done · a.go:1 · ran: `go test ./...` → ok", "REDPROOF R01: TestLogin failed before"),
	} {
		if !verified(report) {
			t.Errorf("%s: not verified", name)
		}
	}
	if ids := idsIn("R01_other, xR02, R03, (G04) D05."); !slices.Equal(ids, []string{"R03", "G04", "D05"}) {
		t.Fatalf("idsIn: %v", ids)
	}
}

// A «ran:» value verifies only when it names a command (ranCommand) and a red proof only when it shows
// failing evidence (redEvidence): a test or command reference and a failure marker. Free text that says
// there is none, however it is worded or wrapped, is neither; every bypass of both reviews is listed.
func TestProofsPositiveStructure(t *testing.T) {
	verified := func(report string) bool {
		t.Helper()
		for _, l := range ParseRequirements(report, []string{"R01"}).Lines {
			if l.ID == "R01" {
				return l.Verified
			}
		}
		t.Fatalf("no R01 line in %q", report)
		return false
	}
	const red = "REDPROOF R01: TestY FAILS on base 813e682: want 2 got 1"
	const ran = "- R01 · done · a.go:1 · ran: go test ./internal/x -run TestY"
	// Free text: neither a command nor failing evidence.
	free := []string{
		"", "  ", "not executed", "not done", "not attempted", "no red proof", "no evidence", "**none**.", "`TBD`.",
		"[none]", "TODO", "to be done", "To be done.", "none", "None yet", "no failures", "no failures …", "not cached",
		"n/a", "_n/a_", "'n/a'", "<none>", "> none", "/none", "skipped, no time", "nothing failed", "not run", "not-run",
		"no-test", "pending", "missing", "?", "???", "…", "...", "-", "—", "no", "not", "N/A", "TODO: later",
		"the test failed", "it failed before", "go", "`go`", "x=1",
	}
	for _, neg := range free {
		if ranCommand(neg) {
			t.Errorf("ranCommand(%q)", neg)
		}
		if redEvidence(neg) {
			t.Errorf("redEvidence(%q)", neg)
		}
		if verified(reqBlock(ran, "REDPROOF R01: "+neg)) {
			t.Errorf("REDPROOF %q: verified", neg)
		}
		if verified(reqBlock(ran + " · REDPROOF R01: " + neg)) {
			t.Errorf("inline REDPROOF %q: verified", neg)
		}
		if verified(reqBlock("- R01 · done · a.go:1 · ran: "+neg, red)) {
			t.Errorf("ran: %q: verified", neg)
		}
	}
	// A reference without a failure marker, or with a negated or zero one, is no failing evidence.
	for _, s := range []string{
		"TestY", "TestY passes", "TestY did not fail", "TestY never failed", "TestY doesn't fail", "TestY didn’t fail",
		"TestY: no panic", "TestY without failing", "`go test ./x` exit code 0", "`go test ./x` → exit 0", "TestY: no error:",
		"Testing failed", "test failed", "the login test failed",
	} {
		if redEvidence(s) {
			t.Errorf("redEvidence(%q)", s)
		}
	}
	for _, s := range []string{
		"TestY FAILS on base 813e682: want 2 got 1", "TestLogin failed before", "TestLogin · FAIL before the change, ok after",
		"nonce_test failed before", "`go test ./web -run TestLogin` exit 1 before the change",
		"go test ./web -run TestLogin → exit status 2 on base", "test_login panicked before",
		"tests/test_x.py::test_y AssertionError before", "TestX: error: want 2 got 1", "not cached; TestLogin failed before",
		"login.test.ts failing on base", "`npm test -- login` exit code 1", "BenchmarkParse panic: nil map",
	} {
		if !redEvidence(s) {
			t.Errorf("redEvidence(%q) = false", s)
		}
		if !verified(reqBlock(ran, "REDPROOF R01: "+s)) {
			t.Errorf("REDPROOF %q: not verified", s)
		}
	}
	for _, s := range []string{
		"go test ./internal/x -run TestY", "`go test ./...` → ok", "**`go vet ./...`**", "scripts/check.sh → green",
		"TMPDIR=/tmp/wt GOTMPDIR=/tmp/wt scripts/check.sh → ok", "./scripts/check.sh", "$ make test → ok",
		"npm test → ok", "pytest tests/test_x.py → 3 passed", "/usr/bin/env bash x.sh", "~/bin/check", "Go test ./x",
	} {
		if !ranCommand(s) {
			t.Errorf("ranCommand(%q) = false", s)
		}
		if !verified(reqBlock("- R01 · done · a.go:1 · ran: "+s, red)) {
			t.Errorf("ran: %q: not verified", s)
		}
	}
}
