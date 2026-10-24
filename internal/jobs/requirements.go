package jobs

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"unicode"

	"github.com/qwwiwi/agent-jobs/internal/checklist"
	"github.com/qwwiwi/agent-jobs/internal/store"
)

// Requirement statuses of a «### Requirements» line (Wave F, F4).
var RequirementStatuses = []string{"done", "partial", "missing", "placeholder", "not-touched"}

// MaxCovers caps job_start's covers.
const MaxCovers = 200

// SectionFindings is the report section the Requirements block lives in.
const SectionFindings = "Findings / Changes"

// requirementsHeading is the block's heading inside Findings / Changes.
const requirementsHeading = "### Requirements"

// RequirementLine is one line of a «### Requirements» block:
// «R01 · done|partial|missing|placeholder|not-touched · evidence · ran: <cmd → result>», optionally with
// a «REDPROOF R01: …» field.
type RequirementLine struct {
	ID       string `json:"id"`
	Status   string `json:"status"`
	Evidence string `json:"evidence,omitempty"`
	Ran      string `json:"ran,omitempty"`
	RedProof string `json:"redproof,omitempty"`
	// Verified: a «done» line whose «ran:» names a command (ranCommand) and with a red proof of this ID
	// that shows failing evidence (its own REDPROOF field or a REDPROOF line of the block; see redProof);
	// a «done» line without either is claimed, not done.
	Verified bool `json:"verified"`
}

// Requirements is the «### Requirements» block of a report, read against the job's covers.
type Requirements struct {
	// Block: the report has the block inside its Findings / Changes section.
	Block bool              `json:"block"`
	Lines []RequirementLine `json:"lines"`
	// RedProofs are the block's REDPROOF lines that are proofs (one per new test that failed before the
	// code); one without failing evidence (redEvidence) is left out.
	RedProofs []string `json:"redproofs,omitempty"`
	// Claimed are the IDs of «done» lines without a «ran:» or without a REDPROOF.
	Claimed []string `json:"claimed,omitempty"`
	// MissingLines are covered IDs the block has no line for; Uncovered are lines for IDs not covered.
	MissingLines []string `json:"missing_lines,omitempty"`
	Uncovered    []string `json:"uncovered,omitempty"`
	// Invalid are lines that start with a requirement ID but do not follow the line format.
	Invalid []string `json:"invalid,omitempty"`
}

// NoteNoRequirements is the report contract note of a job with covers whose report has no
// «### Requirements» block in Findings / Changes; it counts as report_off_contract.
const NoteNoRequirements = "report has no ### Requirements block in Findings / Changes (the job has covers)"

// NoteRequirementsEmpty is the note of a job with covers whose «### Requirements» block has no line for
// any covered ID (empty, prose or a table); it counts as report_off_contract.
const NoteRequirementsEmpty = "report's ### Requirements block has no line for any covered ID (empty, prose or a table; the job has covers)"

// redProofRe is the exact REDPROOF marker at the start of a line or field: «REDPROOFLESS» is not one.
var redProofRe = regexp.MustCompile(`^REDPROOF(?:[^A-Za-z0-9_]|$)`)

// idWordChar reports a character of an ID token: an ID is a whole run of these (R01 is not an ID inside
// R01i, R010, R01_other or x_R01).
func idWordChar(r rune) bool {
	return r >= '0' && r <= '9' || r >= 'A' && r <= 'Z' || r >= 'a' && r <= 'z' || r == '_'
}

// idTokens is the one ID matcher of the Requirements block (validation and extraction): it returns
// the checklist IDs that are whole tokens of s (runs of letters, digits and «_»), in order, once each,
// and s with each of them replaced by a space.
func idTokens(s string) (ids []string, rest string) {
	var b strings.Builder
	for i := 0; i < len(s); {
		j := i
		for j < len(s) && idWordChar(rune(s[j])) {
			j++
		}
		if j == i {
			b.WriteByte(s[i])
			i++
			continue
		}
		if tok := s[i:j]; checklist.ValidID(tok) {
			if !slices.Contains(ids, tok) {
				ids = append(ids, tok)
			}
			b.WriteByte(' ')
		} else {
			b.WriteString(tok)
		}
		i = j
	}
	return ids, b.String()
}

// runners are the programs a command may start with (matched in any case) without a path; any other
// program is named by its path. A positive list: an unknown program is not a command, never a bypass.
var runners = []string{
	"go", "gofmt", "golangci-lint", "staticcheck", "make", "npm", "npx", "pnpm", "yarn", "bun", "deno", "node", "tsc",
	"vitest", "jest", "eslint", "python", "python3", "pip", "pip3", "pytest", "uv", "poetry", "tox", "ruff", "mypy",
	"cargo", "rustc", "bash", "sh", "zsh", "git", "gh", "curl", "docker", "podman", "kubectl", "terraform", "ruby",
	"bundle", "rake", "rspec", "mvn", "gradle", "dotnet", "swift", "xcodebuild", "php", "composer", "phpunit", "mix",
	"sqlite3", "psql", "jq", "shellcheck",
}

// scriptExts are the extensions of a relative script path that is a program without «./».
var scriptExts = []string{".sh", ".bash", ".py", ".js", ".mjs", ".cjs", ".ts", ".rb", ".pl"}

// pathPrefixes start a program path.
var pathPrefixes = []string{"./", "../", "/", "~/"}

// assignRe is a leading «VAR=value» of a command line.
var assignRe = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*=`)

// wrapChar is white space, a Markdown wrapper, a bracket or a quote around a word.
func wrapChar(r rune) bool {
	return unicode.IsSpace(r) || strings.ContainsRune("*_`~()[]{}<>\"'«»“”„‘’", r)
}

// tailChar is a wrapChar or trailing punctuation.
func tailChar(r rune) bool { return wrapChar(r) || strings.ContainsRune(".,;:!?…", r) }

// cleanWord strips wrappers from both ends of a word and trailing punctuation («**none**.» is «none»;
// «./x» keeps its dot).
func cleanWord(w string) string {
	return strings.TrimRightFunc(strings.TrimLeftFunc(w, wrapChar), tailChar)
}

// isArrow reports a result arrow between a command and its result.
func isArrow(w string) bool { return w == "→" || w == "->" || w == "=>" }

// isProgramPath reports a program named by its path: a path starting with «./», «../», «~/» or «/» (an
// absolute one with a directory) with a letter or digit after the prefix, or a relative path «dir/name»
// ending in a script extension.
func isProgramPath(w string) bool {
	for _, p := range pathPrefixes {
		if rest, ok := strings.CutPrefix(w, p); ok {
			if p == "/" && !strings.Contains(rest, "/") {
				return false // an absolute path names its directory («/usr/bin/x»; «/none» is not one)
			}
			return strings.IndexFunc(rest, func(r rune) bool { return unicode.IsLetter(r) || unicode.IsDigit(r) }) >= 0
		}
	}
	return strings.Contains(w, "/") && slices.ContainsFunc(scriptExts, func(e string) bool {
		return len(w) > len(e) && strings.HasSuffix(strings.ToLower(w), e)
	})
}

// ranCommand reports whether s names a command invocation (a «ran:» value, a backticked command or the
// start of a red proof). Its words are cleaned (cleanWord); a leading «$» prompt and «VAR=value»
// assignments are skipped; the first other word must then be a runner followed by an argument (a word
// that is not a result arrow) or a program path (isProgramPath). Free text («not executed», «none», «no
// failures») is not a command.
func ranCommand(s string) bool {
	words := strings.Fields(s)
	for i, w := range words {
		w = cleanWord(w)
		if w == "" || w == "$" || assignRe.MatchString(w) {
			continue
		}
		if !slices.Contains(runners, strings.ToLower(w)) {
			return isProgramPath(w)
		}
		for _, a := range words[i+1:] {
			if a = cleanWord(a); a != "" {
				return !isArrow(a)
			}
		}
		return false
	}
	return false
}

// testRefRe is a test reference: a Go test, benchmark, fuzz or example function (TestLogin), a pytest
// style test_name, a Go test file stem (login_test), a JS/TS test file (login.test.ts, x.spec.js) or a
// node id with «::» (tests/x.py::test_y).
var testRefRe = regexp.MustCompile(`\b(?:(?:Test|Benchmark|Fuzz|Example)[A-Z0-9_]\w*|test_\w+|\w+_test|[\w-]+\.(?:test|spec)\.\w+|\w+::\w+)\b`)

// backtickRe is a backticked span.
var backtickRe = regexp.MustCompile("`([^`]+)`")

// failRe is a failure marker (any case): fail / fails / failed / failing, panic / panics / panicked,
// assertion / AssertionError, «error:», or «exit», «exit code» or «exit status» with its number (group 1).
var failRe = regexp.MustCompile(`(?i)\b(?:fail|fails|failed|failing|panic|panics|panicked|assertion|assertionerror)\b|\berror:|\bexit(?:\s+code|\s+status)?[\s:=]*([0-9]+)\b`)

// negators negate a failure marker right after them («did not fail»); a word ending in «n't» does too
// («doesn't fail»).
var negators = []string{"no", "not", "never", "without", "cannot", "nor", "zero"}

// failMarker reports whether s holds a failure marker (failRe) that the word right before it does not
// negate (negators) and, for an exit code, whose number is not 0.
func failMarker(s string) bool {
	for _, m := range failRe.FindAllStringSubmatchIndex(s, -1) {
		if m[2] >= 0 && strings.Trim(s[m[2]:m[3]], "0") == "" {
			continue // exit code 0
		}
		if before := strings.Fields(s[:m[0]]); len(before) > 0 {
			w := strings.ToLower(cleanWord(before[len(before)-1]))
			if slices.Contains(negators, w) || strings.HasSuffix(w, "n't") || strings.HasSuffix(w, "n’t") {
				continue
			}
		}
		return true
	}
	return false
}

// proofLead is what may stand before a red proof's text: separators and punctuation («: », «· », «— »).
func proofLead(r rune) bool { return tailChar(r) || strings.ContainsRune("-–—·", r) }

// redEvidence reports whether a red proof s (its IDs left out) shows failing evidence: a test or command
// reference (testRefRe, a backticked command, or s starting with a command; ranCommand) and a failure
// marker (failMarker). Free text («no red proof», «not cached», «none») shows none.
func redEvidence(s string) bool {
	ref := testRefRe.MatchString(s) || ranCommand(strings.TrimLeftFunc(s, proofLead))
	for _, m := range backtickRe.FindAllStringSubmatch(s, -1) {
		ref = ref || ranCommand(m[1])
	}
	return ref && failMarker(s)
}

// redProof reads a REDPROOF line or field s: the IDs it names and whether it is a proof. It is one only
// with the exact marker and failing evidence in what is left without the IDs (redEvidence).
func redProof(s string) (ids []string, ok bool) {
	if !redProofRe.MatchString(s) {
		return nil, false
	}
	ids, rest := idTokens(s[len("REDPROOF"):])
	if !redEvidence(rest) {
		return nil, false
	}
	return ids, true
}

// ValidateCovers trims covers, drops repeats (first one kept) and checks every ID is a checklist row ID.
func ValidateCovers(covers []string) ([]string, error) {
	var out []string
	for _, c := range covers {
		c = strings.TrimSpace(c)
		if !checklist.ValidID(c) {
			return nil, fmt.Errorf("covers: %q is not a checklist requirement ID (R<nn>, R<nn>i, G<nn>, D<nn>)", c)
		}
		if !slices.Contains(out, c) {
			out = append(out, c)
		}
	}
	if len(out) > MaxCovers {
		return nil, fmt.Errorf("covers: %d IDs, at most %d", len(out), MaxCovers)
	}
	return out, nil
}

// requirementsBlock returns the lines of the «### Requirements» block of the Findings / Changes
// section (the last such section; the block ends at the next heading), and whether it is there. A
// heading inside a ``` fence does not count.
func requirementsBlock(report string) ([]string, bool) {
	body, ok := ReportSection(report, SectionFindings)
	if !ok {
		return nil, false
	}
	var lines []string
	in, found, fence := false, false, false
	for _, l := range strings.Split(body, "\n") {
		t := strings.TrimSpace(strings.TrimRight(l, "\r"))
		if strings.HasPrefix(t, "```") {
			fence = !fence
			if in {
				lines = append(lines, t)
			}
			continue
		}
		if !fence && isHeading(t) {
			in = strings.EqualFold(NormalizeHeading(t), requirementsHeading)
			found = found || in
			continue
		}
		if in && !fence {
			lines = append(lines, t)
		}
	}
	return lines, found
}

// isHeading reports whether a trimmed line is a markdown heading («#» to «######» and a space).
func isHeading(t string) bool {
	h := strings.TrimLeft(t, "#")
	return len(t)-len(h) >= 1 && len(t)-len(h) <= 6 && strings.HasPrefix(h, " ")
}

// NormalizeHeading collapses the white space of a heading line.
func NormalizeHeading(s string) string { return strings.Join(strings.Fields(s), " ") }

// stripItem drops a list marker («- », «* », «1. ») from a line.
func stripItem(s string) string {
	s = strings.TrimSpace(s)
	for _, p := range []string{"- ", "* ", "+ "} {
		if strings.HasPrefix(s, p) {
			return strings.TrimSpace(s[len(p):])
		}
	}
	if i := strings.IndexByte(s, '.'); i > 0 && i < 4 && strings.Trim(s[:i], "0123456789") == "" && strings.HasPrefix(s[i+1:], " ") {
		return strings.TrimSpace(s[i+1:])
	}
	return s
}

// idsIn returns the checklist IDs named in s (whole tokens, idTokens), in order, once each.
func idsIn(s string) []string {
	ids, _ := idTokens(s)
	return ids
}

// ParseRequirements reads the «### Requirements» block of report against covers. Fields of a line are
// separated by «·»; the first is the ID, the second the status; a field starting with «ran:» is the
// command and its result, one starting with «REDPROOF» the red proof, the others the evidence.
func ParseRequirements(report string, covers []string) Requirements {
	r := Requirements{Lines: []RequirementLine{}}
	lines, ok := requirementsBlock(report)
	if !ok {
		return r
	}
	r.Block = true
	proved := map[string]bool{}
	for _, raw := range lines {
		l := stripItem(strings.Trim(raw, "`"))
		if l == "" {
			continue
		}
		if redProofRe.MatchString(l) {
			if ids, ok := redProof(l); ok {
				r.RedProofs = append(r.RedProofs, l)
				for _, id := range ids {
					proved[id] = true
				}
			}
			continue
		}
		parts := strings.Split(l, "·")
		id := strings.TrimSpace(parts[0])
		if !checklist.ValidID(id) {
			continue // prose inside the block
		}
		if len(parts) < 2 || !slices.Contains(RequirementStatuses, strings.ToLower(strings.TrimSpace(parts[1]))) {
			r.Invalid = append(r.Invalid, Printable(l, 200))
			continue
		}
		line := RequirementLine{ID: id, Status: strings.ToLower(strings.TrimSpace(parts[1]))}
		var evidence []string
		for _, p := range parts[2:] {
			p = strings.TrimSpace(p)
			switch {
			case strings.HasPrefix(strings.ToLower(p), "ran:"):
				line.Ran = strings.TrimSpace(p[len("ran:"):])
			case redProofRe.MatchString(p):
				// An inline proof counts only when it names this line's ID; the first such one is kept.
				if ids, ok := redProof(p); ok && slices.Contains(ids, id) && line.RedProof == "" {
					line.RedProof = strings.TrimSpace(strings.TrimLeft(p[len("REDPROOF"):], ": "))
				}
			case p != "":
				evidence = append(evidence, p)
			}
		}
		line.Evidence = strings.Join(evidence, " · ")
		r.Lines = append(r.Lines, line)
	}
	seen := map[string]bool{}
	for i := range r.Lines {
		l := &r.Lines[i]
		seen[l.ID] = true
		if l.Status == "done" {
			l.Verified = ranCommand(l.Ran) && (l.RedProof != "" || proved[l.ID])
			if !l.Verified && !slices.Contains(r.Claimed, l.ID) {
				r.Claimed = append(r.Claimed, l.ID)
			}
		}
		if !slices.Contains(covers, l.ID) && !slices.Contains(r.Uncovered, l.ID) {
			r.Uncovered = append(r.Uncovered, l.ID)
		}
	}
	for _, c := range covers {
		if !seen[c] {
			r.MissingLines = append(r.MissingLines, c)
		}
	}
	return r
}

// requirementsNote is the report contract note for a job with covers: NoteNoRequirements when the
// report has no block, NoteRequirementsEmpty when the block has no line for any covered ID, "" otherwise
// (and always "" without covers).
func requirementsNote(report string, covers []string) string {
	if len(covers) == 0 {
		return ""
	}
	if _, ok := requirementsBlock(report); !ok {
		return NoteNoRequirements
	}
	if !slices.ContainsFunc(ParseRequirements(report, covers).Lines, func(l RequirementLine) bool { return slices.Contains(covers, l.ID) }) {
		return NoteRequirementsEmpty
	}
	return ""
}

// NoteJobRowUnread is the run-wrapper's note when it cannot read the job's row: without the covers the
// Requirements block cannot be checked, so the report counts as off contract (fail closed).
const NoteJobRowUnread = "job row unreadable: the report's ### Requirements block was not checked (fail closed)"

// wrapperContractNote is the run-wrapper's report contract note: the sections first, then the
// Requirements block against the covers in the job's row (get), NoteJobRowUnread when it cannot be read.
func wrapperContractNote(report string, get func() (*store.Job, error)) string {
	if n := reportContractNote(report, nil); n != "" {
		return n
	}
	j, err := get()
	if err != nil {
		return NoteJobRowUnread + ": " + RedactPrintable(err.Error(), 160)
	}
	return reportContractNote(report, j.Covers)
}

// RequirementsTrailers returns the IDs of «Requirements» trailer values (one value per line, as
// `git log --format=%(trailers:key=Requirements,valueonly)` prints them), in order, once each (report
// only: nothing enforces them).
func RequirementsTrailers(values string) []string {
	var out []string
	for _, l := range strings.Split(values, "\n") {
		for _, id := range idsIn(l) {
			if !slices.Contains(out, id) {
				out = append(out, id)
			}
		}
	}
	return out
}

// JobRequirementsTrailers returns the «Requirements:» trailer IDs of the job's own commits (base..head;
// head is the job's worktree HEAD while it is there, else refs/agent-jobs/jobs/<id>), and a note when
// they could not be read ("" for a job without a repository or without commits).
func (m *Manager) JobRequirementsTrailers(j *store.Job) ([]string, string) {
	if j.RepoPath == "" || j.BaseCommit == "" {
		return nil, ""
	}
	ctx, cancel := context.WithTimeout(context.Background(), MergeCheckTimeout)
	defer cancel()
	head := ""
	work := filepath.Join(m.JobDir(j.ID), "work")
	if fi, err := os.Lstat(work); err == nil && fi.IsDir() {
		if err := m.verifyWorktree(j, work); err != nil {
			return nil, "commit trailers not read: the job's worktree is not verified"
		}
		h, err := gitCtxOK(ctx, work, "rev-parse", "--verify", "HEAD^{commit}")
		if err != nil {
			return nil, "commit trailers not read: " + RedactPrintable(err.Error(), 160)
		}
		head = h
	} else if h, err := gitCtxOK(ctx, j.RepoPath, "rev-parse", "--verify", "-q", "refs/agent-jobs/jobs/"+j.ID+"^{commit}"); err == nil {
		head = h
	}
	if head == "" || head == j.BaseCommit {
		return nil, ""
	}
	// Real trailers only (git's own trailer parsing): a «Requirements:» line in a body paragraph is not one.
	vals, err := gitCtxOK(ctx, j.RepoPath, "log", "--no-merges", "--no-show-signature",
		"--format=%(trailers:key=Requirements,valueonly)", j.BaseCommit+".."+head)
	if err != nil {
		return nil, "commit trailers not read: " + RedactPrintable(err.Error(), 160)
	}
	return RequirementsTrailers(vals), ""
}
