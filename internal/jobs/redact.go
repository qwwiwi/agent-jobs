package jobs

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/qwwiwi/agent-jobs/internal/store"
)

// MaxRedactBytes caps the text Redact scans. RE2 is linear, so this bounds the time, not the risk of
// backtracking; what lies past the cap is not shown at all (it could not be checked). redactReport
// never reads more than this (plus one byte) of a report.
const MaxRedactBytes = 4 << 20

// RawReportName is the unredacted copy of report.md, kept (0600) only with keep_raw_reports.
const RawReportName = "report.raw.md"

// redactRule is one secret format: what a match is called and which submatch is the secret (0 = all).
type redactRule struct {
	kind  string
	re    *regexp.Regexp
	group int
}

// pemMarkerRe is a header or footer line of a PEM private key block (PKCS#1/#8, EC, OPENSSH,
// ENCRYPTED, ...) or of an armored PGP private key block: the whole line (indentation and a CR
// trimmed), so a marker quoted inside prose is none. Allowed around it is only what a file or a
// listing puts there: diff, quote or comment prefixes ("+", "-", ">", "#", "|", nested as in
// "> > "), an assignment ("KEY=",
// "key: "), quotes, an escaped line break, a trailing comma. pemInlineRe is a whole block on one line
// (a key with escaped line breaks, as in JSON); pemEscapedRe a BEGIN marker followed by an escaped
// line break, the rest of the line (such a line cut off before its END).
var (
	pemMarkerRe = regexp.MustCompile(`^(?:[-+>#|*][ \t]*)*(?:[A-Za-z_][A-Za-z0-9_.-]{0,40}[ \t]*[:=][ \t]*)?["'` + "`" + `]?` +
		`(-----(BEGIN|END) [A-Z0-9 ]{0,40}PRIVATE KEY(?: BLOCK)?-----)(?:\\r)?(?:\\n)?["'` + "`" + `]?[,;]?$`)
	pemInlineRe  = regexp.MustCompile(`-----BEGIN [A-Z0-9 ]{0,40}PRIVATE KEY(?: BLOCK)?-----[^\n]*?-----END [A-Z0-9 ]{0,40}PRIVATE KEY(?: BLOCK)?-----`)
	pemEscapedRe = regexp.MustCompile(`-----BEGIN [A-Z0-9 ]{0,40}PRIVATE KEY(?: BLOCK)?-----(?:\\r)?\\n[^\n]*`)
	// pemHeaderRe is an RFC 1421 / OpenPGP armor header line: Proc-Type, DEK-Info, Version, Comment, ...
	pemHeaderRe = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9-]{0,40}:(?:[ \t].*)?$`)
	// cutNoteRe is the note a cut leaves at the start or end of a text.
	cutNoteRe = regexp.MustCompile(`^\[(?:\.\.\. [0-9]+ earlier bytes cut|redacted:unscanned [0-9]+ bytes)`)
)

// redactRules run in this order: the specific token formats first, the generic key=value last, so a
// value already replaced by a specific rule ("[redacted:...]") is not matched again (values never
// contain '['). All are RE2 (linear time), and a variable-length value is matched whole (no upper
// bound), so a long credential is never replaced only in part. The distinctive GitHub prefixes match
// inside a word ("mcp__x__ghp_..."); the short "sk-" only at a word start ("task-..." is no key).
// A boundary before a value is a captured class (RE2 has no lookaround) outside the secret group,
// and none is matched after it, so two values one separator apart are both found.
var redactRules = []redactRule{
	// Key blocks on lines of their own are redactPEM's; this is one written on a single line.
	{"private-key", pemInlineRe, 0},
	{"private-key", pemEscapedRe, 0},
	{"anthropic", regexp.MustCompile(`\bsk-ant-[A-Za-z0-9_-]{20,}`), 0},
	{"openai", regexp.MustCompile(`\bsk-(?:proj-|svcacct-|admin-)?[A-Za-z0-9_-]{20,}`), 0},
	{"github", regexp.MustCompile(`gh[pousr]_[A-Za-z0-9]{30,}`), 0},
	{"github", regexp.MustCompile(`github_pat_[A-Za-z0-9_]{22,}`), 0},
	{"slack", regexp.MustCompile(`\bxox[abposr]-[A-Za-z0-9-]{10,}`), 0},
	{"aws-key-id", regexp.MustCompile(`\b(?:AKIA|ASIA)[0-9A-Z]{16}\b`), 0},
	{"aws-secret", regexp.MustCompile(`(?i)aws_secret_access_key["']?\s{0,4}[:=]\s{0,4}["']?([A-Za-z0-9/+=]{40})`), 1},
	{"google", regexp.MustCompile(`\bAIza[0-9A-Za-z_-]{35}`), 0},
	// The Bot API URL form puts "bot" right before the digits: /bot<TOKEN>/getMe.
	{"telegram", regexp.MustCompile(`(?:^|[^0-9A-Za-z_-]|bot)([0-9]{8,10}:[A-Za-z0-9_-]{35,})`), 1},
	{"jwt", regexp.MustCompile(`\beyJ[A-Za-z0-9_-]{8,}\.[A-Za-z0-9_-]{8,}\.[A-Za-z0-9_-]{8,}`), 0},
	{"bearer", regexp.MustCompile(`(?i)\bbearer[ \t]{1,8}([A-Za-z0-9._~+/=-]{16,})`), 1},
	{"secret", regexp.MustCompile(`(?i)[A-Za-z0-9_.-]{0,40}(?:token|secret|password|passwd|api[_-]?key)[A-Za-z0-9_.-]{0,40}["']?[ \t]{0,4}[:=][ \t]{0,4}["']?([^\s"'` + "`" + `,;\[\]{}()<>]{16,})`), 1},
}

// Redact replaces secret-looking values in s with [redacted:<kind>] and returns the text and the
// number of values replaced. Text past MaxRedactBytes is dropped with a note.
func Redact(s string) (string, int) {
	if len(s) <= MaxRedactBytes {
		return redactScanned(s, 0)
	}
	cut := scanCut(s, MaxRedactBytes)
	return redactScanned(s[:cut], int64(len(s)-cut))
}

// redactScanned redacts s, a whole text or its scanned prefix; unscanned > 0 is the size of what
// followed it, replaced by a note (and counted as one redaction).
func redactScanned(s string, unscanned int64) (string, int) {
	s, n := redactPEM(s, false)
	for _, r := range redactRules {
		var k int
		s, k = r.apply(s)
		n += k
	}
	if unscanned > 0 {
		s += fmt.Sprintf("\n[redacted:unscanned %d bytes past the %d-byte scan cap]\n", unscanned, MaxRedactBytes)
		n++
	}
	return s, n
}

// scanCut is where a text longer than max is cut for scanning: lineCut of its first max bytes.
func scanCut(s string, max int) int {
	if len(s) <= max {
		return len(s)
	}
	return lineCut(s[:max])
}

// lineCut is where a text cut at its end (more followed) is cut back to: right after its last line
// break, however far back that is, and 0 when it has none (the whole text is one incomplete line).
// The incomplete final line is always dropped whole: a value split by the cut would leave a prefix
// too short to match its format but long enough to be useful (no secret format but a PEM block spans
// lines, and a cut-off PEM block has its own rule).
func lineCut(s string) int {
	return strings.LastIndexByte(s, '\n') + 1
}

// RedactText is Redact without the count.
func RedactText(s string) string {
	out, _ := Redact(s)
	return out
}

// RedactRead redacts a text read up to a byte cap: when truncated (the file went on), the last
// incomplete line is dropped whole first (all of the text when it has no line break), so a value cut
// by the read cap is not shown in part; a PEM key block cut off by it is redacted from its BEGIN line
// on (redactPEM).
func RedactRead(s string, truncated bool) string {
	if truncated {
		s = s[:lineCut(s)]
	}
	return RedactText(s)
}

// PEM private key blocks are found line by line (redactPEM), not by a pattern over the whole text,
// so a block a cut left without its BEGIN or END line (dropping the partial line at a cut may take
// it, or an earlier build's cut did) is still found, wherever the text came from and whether or not
// it was cut this time. A marker is a line of its own (indentation, trailing blanks and a CR
// ignored); a key body line is base64, an armor header ("Proc-Type: 4,ENCRYPTED", "Version: ...")
// or blank.

// pemLineKind is what one line is to redactPEM.
type pemLineKind int

const (
	pemOther pemLineKind = iota
	pemBegin
	pemEnd
	pemBase64 // base64 characters only
	pemHeader // an armor header line
	pemBlank
	pemNote // a cut note
)

// pemMaxLine bounds a line that can be a marker (pemTracker keeps no more of a line).
const pemMaxLine = 256

// pemLine classifies a line (without its line break) and returns its trimmed content.
func pemLine(line string) (pemLineKind, string) {
	t := strings.Trim(line, " \t\r")
	if t == "" {
		return pemBlank, t
	}
	if len(t) <= pemMaxLine && strings.Contains(t, "-----") {
		if m := pemMarkerRe.FindStringSubmatch(t); m != nil {
			if m[2] == "BEGIN" {
				return pemBegin, t
			}
			return pemEnd, t
		}
	}
	p := pemStripPrefix(t)
	switch {
	case isBase64Line(pemBodyLine(t)):
		return pemBase64, t
	case p == "":
		// Only a prefix: a quoted or commented blank line ("> ", "#").
		return pemBlank, t
	case t[0] == '[' && cutNoteRe.MatchString(t):
		return pemNote, t
	case pemHeaderRe.MatchString(p):
		return pemHeader, t
	}
	return pemOther, t
}

// pemStripPrefix is t without the diff, quote or comment prefix a marker line may carry (runs of
// "-+>#|*", each with the blanks after it, nested as in "> > " or "+ > "), so marker, header and
// body lines are read alike.
func pemStripPrefix(t string) string {
	i := 0
	for i < len(t) && strings.IndexByte("-+>#|*", t[i]) >= 0 {
		i++
		for i < len(t) && (t[i] == ' ' || t[i] == '\t') {
			i++
		}
	}
	return t[i:]
}

// pemBodyLine is t without a diff, quote or comment prefix (pemStripPrefix), a leading quote and a
// trailing quote or comma, as key body lines appear in a diff, a quote or a quoted string ("+MIIE...",
// "> MIIE...", "\"MIIE...\","); t when it has none or is base64 as it is.
func pemBodyLine(t string) string {
	if isBase64Line(t) {
		return t
	}
	b := strings.TrimRight(pemStripPrefix(t), `"',`)
	if b != "" && (b[0] == '"' || b[0] == '\'') {
		b = b[1:]
	}
	if b == "" || !isBase64Line(b) {
		return t
	}
	return b
}

// pemChecksum says a line is the PGP armor checksum line ("=Ab12"): a base64 line starting with "=".
func pemChecksum(kind pemLineKind, t string) bool {
	return kind == pemBase64 && pemBodyLine(t)[0] == '='
}

func isBase64Line(t string) bool {
	for i := 0; i < len(t); i++ {
		c := t[i]
		if !('A' <= c && c <= 'Z' || 'a' <= c && c <= 'z' || '0' <= c && c <= '9' || c == '+' || c == '/' || c == '=') {
			return false
		}
	}
	return true
}

// A markerless run is base64 lines with no marker around them, the middle of a key whose both
// markers fell outside a window: it is a key's when it touches the text's start or end or a cut note
// and holds at least pemMinFullLines lines of a full key line's length.
const (
	pemMinFullLines = 2
	pemFullMin      = 40
	pemFullMax      = 80
)

// redactPEM replaces each PEM private key block of s, and what a cut left of one, with
// [redacted:private-key], in one pass over its lines (linear). open says s starts inside a block (its
// BEGIN line came before s). Redacted are:
//   - a block, from its BEGIN line through its END line (a BEGIN inside a block changes nothing);
//   - a BEGIN line with no END line after it: through the end of s;
//   - an END line with no BEGIN line before it: with the run of key body lines right above it (from
//     the start of s when the run reaches it);
//   - a markerless run of base64 lines (see pemMinFullLines) at the start or end of s or next to a
//     cut note.
//
// It returns the text and the number of spans replaced.
func redactPEM(s string, open bool) (string, int) {
	type span struct{ from, to int }
	var spans []span
	add := func(from, to int) {
		for k := len(spans) - 1; k >= 0 && from <= spans[k].to; k-- {
			from, to = min(from, spans[k].from), max(to, spans[k].to)
			spans = spans[:k]
		}
		spans = append(spans, span{from, to})
	}
	openFrom := 0
	bodyFrom := -1                      // start of the current run of key body lines (first non-blank one), -1: none
	atStart := true                     // every line so far was a key body line
	b64From, b64To, b64Full := -1, 0, 0 // the current run of base64 lines
	b64Edge := false                    // it began at the start of s or right after a cut note
	markerless := func(atEdge bool) {
		if b64From >= 0 && (b64Edge || atEdge) && b64Full >= pemMinFullLines {
			add(b64From, b64To)
		}
		b64From, b64Full = -1, 0
	}
	prev := pemNote // the start of s counts as a cut: a run there can be a key's middle
	short := false  // the line before was a base64 line shorter than a full key line
	for start := 0; start < len(s); {
		end := strings.IndexByte(s[start:], '\n')
		if end < 0 {
			end = len(s)
		} else {
			end += start
		}
		line := s[start:end]
		kind, t := pemLine(line)
		from := start + len(line) - len(strings.TrimLeft(line, " \t\r"))
		to := from + len(t)
		if open {
			if kind == pemEnd {
				add(openFrom, to)
				open = false
			}
		} else {
			switch kind {
			case pemBegin:
				markerless(false)
				open, openFrom = true, from
				bodyFrom = -1
			case pemEnd:
				switch {
				case atStart:
					add(0, to)
				case bodyFrom >= 0:
					add(bodyFrom, to)
				default:
					add(from, to)
				}
				bodyFrom, b64From, b64Full = -1, -1, 0
			case pemBase64, pemHeader, pemBlank:
				if short && kind != pemBlank && !pemChecksum(kind, t) {
					// A short base64 line is a key's last body line only right above its END line;
					// with more lines after it, it is text ("ok"), and the run starts after it.
					markerless(false)
					bodyFrom, atStart = -1, false
				}
				if kind != pemBlank && bodyFrom < 0 {
					bodyFrom = from
				}
				if kind == pemBase64 {
					if b64From < 0 {
						b64From, b64Edge = from, prev == pemNote
					}
					b64To = to
					if n := len(pemBodyLine(t)); pemFullMin <= n && n <= pemFullMax {
						b64Full++
					}
				} else {
					markerless(false)
				}
			case pemNote:
				markerless(true)
				bodyFrom = -1
			default:
				markerless(false)
				bodyFrom = -1
			}
		}
		if kind != pemBase64 && kind != pemHeader && kind != pemBlank {
			atStart = false
		}
		// The PGP checksum line ("=Ab12") is short too, and it can follow the body. A short line stays
		// pending across blank lines: it is the last body line if the checksum or END line comes next.
		if kind != pemBlank {
			short = !open && kind == pemBase64 && len(pemBodyLine(t)) < pemFullMin && !pemChecksum(kind, t)
		}

		if kind != pemBlank || prev != pemNote {
			prev = kind
		}
		start = end + 1
	}
	if open {
		add(openFrom, len(strings.TrimRight(s, "\r\n")))
	} else {
		markerless(true)
	}
	if len(spans) == 0 {
		return s, 0
	}
	var b strings.Builder
	last := 0
	for _, sp := range spans {
		b.WriteString(s[last:sp.from])
		b.WriteString("[redacted:private-key]")
		last = sp.to
	}
	b.WriteString(s[last:])
	return b.String(), len(spans)
}

// pemTracker follows the PEM key block markers of a byte stream, line by line, keeping no more than
// pemMaxLine bytes of a line: open says the stream is inside a block (a BEGIN line with no END line
// after it yet).
type pemTracker struct {
	open bool
	line []byte
	long bool // the current line is longer than pemMaxLine: no marker
}

func (p *pemTracker) feed(b []byte) {
	for len(b) > 0 {
		i := bytes.IndexByte(b, '\n')
		chunk := b
		if i >= 0 {
			chunk = b[:i]
		}
		if !p.long {
			if len(p.line)+len(chunk) > pemMaxLine {
				p.long, p.line = true, p.line[:0]
			} else {
				p.line = append(p.line, chunk...)
			}
		}
		if i < 0 {
			return
		}
		if !p.long {
			switch kind, _ := pemLine(string(p.line)); kind {
			case pemBegin:
				p.open = true
			case pemEnd:
				p.open = false
			}
		}
		p.line, p.long = p.line[:0], false
		b = b[i+1:]
	}
}

// RedactPrintable is Printable(s, max) for job-derived text: redacted before it is cut to max runes,
// since a value cut short no longer matches its format, and again after Printable (dropped control
// characters may join the parts of a value).
func RedactPrintable(s string, max int) string {
	return Printable(RedactText(Printable(RedactText(s), 0)), max)
}

func (r redactRule) apply(s string) (string, int) {
	idx := r.re.FindAllStringSubmatchIndex(s, -1)
	if len(idx) == 0 {
		return s, 0
	}
	var b strings.Builder
	last, n := 0, 0
	for _, m := range idx {
		start, end := m[2*r.group], m[2*r.group+1]
		if start < 0 || start < last {
			continue
		}
		b.WriteString(s[last:start])
		b.WriteString("[redacted:" + r.kind + "]")
		last = end
		n++
	}
	b.WriteString(s[last:])
	return b.String(), n
}

// countingReader counts the bytes read through it.
type countingReader struct {
	r io.Reader
	n int64
}

func (c *countingReader) Read(p []byte) (int, error) {
	k, err := c.r.Read(p)
	c.n += int64(k)
	return k, err
}

// redactFile redacts an open report: at most MaxRedactBytes+1 bytes are read; an oversized report's
// scanned prefix is redacted and the rest (never read) becomes the unscanned note. read is the number
// of bytes read from f.
func redactFile(f *os.File) (out string, n int, read int64, err error) {
	fi, err := f.Stat()
	if err != nil {
		return "", 0, 0, err
	}
	cr := &countingReader{r: io.LimitReader(f, MaxRedactBytes+1)}
	b, err := io.ReadAll(cr)
	if err != nil {
		return "", 0, cr.n, err
	}
	s := string(b)
	if len(s) <= MaxRedactBytes {
		out, n = redactScanned(s, 0)
		return out, n, cr.n, nil
	}
	cut := scanCut(s, MaxRedactBytes)
	size := fi.Size()
	if size < int64(len(s)) {
		size = int64(len(s)) // the file grew after the stat
	}
	out, n = redactScanned(s[:cut], size-int64(cut))
	return out, n, cr.n, nil
}

// redactReport redacts report.md of a job that has ended, in place. With keep_raw_reports the
// original is kept next to it as report.raw.md (0600); otherwise it is gone. The job's processes
// are over by now, but the job dir was the job's: the file is opened without following a link and
// only a regular file is touched; the new files are unique temp files (O_EXCL, 0600) renamed into
// place after an fsync. A nil error means report.md holds no unredacted secret (or there is no
// regular report.md); an error leaves the job's redaction pending (retried every tick).
func (m *Manager) redactReport(id string) error {
	dir := m.JobDir(id)
	p := filepath.Join(dir, "report.md")
	f, ok, err := openRegular(p)
	if err != nil || !ok {
		return err
	}
	defer f.Close()
	out, n, _, err := redactFile(f)
	if err != nil || n == 0 {
		return err
	}
	if m.Config().KeepRawReports {
		// The whole original, streamed (not held in memory).
		err := replaceFile(dir, RawReportName, func(w io.Writer) error {
			if _, err := f.Seek(0, io.SeekStart); err != nil {
				return err
			}
			_, err := io.Copy(w, f)
			return err
		})
		if err != nil {
			return fmt.Errorf("keep raw report: %w", err)
		}
	}
	if err := replaceFile(dir, "report.md", func(w io.Writer) error {
		_, err := io.WriteString(w, out)
		return err
	}); err != nil {
		return err
	}
	m.Log.Printf("job %s: report.md: %d secret-looking value(s) redacted", id, n)
	return nil
}

// replaceFile writes dir/name through a new unique temp file in dir (0600, O_EXCL), fsynced, then
// renamed over name; the temp file is removed on failure. Whatever sits at dir/name (a file, a
// link) is replaced, never written through.
func replaceFile(dir, name string, write func(io.Writer) error) (err error) {
	tf, err := os.CreateTemp(dir, "."+name+".*.tmp")
	if err != nil {
		return err
	}
	defer func() {
		if err != nil {
			_ = os.Remove(tf.Name())
		}
	}()
	if err = write(tf); err != nil {
		_ = tf.Close()
		return err
	}
	if err = errors.Join(syncFile(tf), tf.Close()); err != nil {
		return err
	}
	if err = renameFile(tf.Name(), filepath.Join(dir, name)); err != nil {
		return err
	}
	return syncDir(dir)
}

// renameFile is os.Rename; a test swaps it to see a failed rewrite leave the redaction pending.
var renameFile = os.Rename

// settled runs once a job reached a terminal or stopped status: its report is redacted in place and
// its stats row written. A failed redaction stays pending (retryRedactions).
func (m *Manager) settled(id string) {
	m.redactAndClear(id)
	if j, err := m.Store.Get(id); err == nil {
		m.spawnedGroupAlive(j) // a group that ended with the job is forgotten right away
		m.acceptanceGroupAlive(j)
	}
	m.recordDirSize(id)
	m.recordStatsOf(id)
}

// redactAndClear redacts the job's report and clears its pending mark on success.
func (m *Manager) redactAndClear(id string) bool {
	if err := m.redactReport(id); err != nil {
		m.Log.Printf("job %s: redact report: %v (redaction pending, retried every tick; readers redact on the fly)", id, err)
		return false
	}
	if err := m.Store.ClearRedactionPending(id); err != nil {
		m.Log.Printf("job %s: clear redaction pending: %v", id, err)
		return false
	}
	return true
}

// retryRedactions redacts the reports whose redaction is still pending: a rewrite that failed, or a
// daemon that died between the row turning terminal and the rewrite. Runs every tick (and so at
// daemon start).
func (m *Manager) retryRedactions() error {
	ids, err := m.Store.RedactionPending()
	if err != nil {
		return err
	}
	for _, id := range ids {
		if m.redactAndClear(id) {
			m.Log.Printf("job %s: report redaction done on retry", id)
		}
	}
	return nil
}

// redactStats redacts the free-text fields of a stats row (they come from the job's stream).
func redactStats(st *store.JobStats) {
	st.ResultSubtype = RedactText(st.ResultSubtype)
	for _, mp := range []map[string]int{st.ToolCalls, st.Skills, st.GuardRules} {
		for k, v := range mp {
			if r := RedactText(k); r != k {
				delete(mp, k)
				mp[r] += v
			}
		}
	}
}
