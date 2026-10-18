package jobs

import (
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

// pemBeginRe and pemEndRe are the header and footer lines of a PEM private key block (PKCS#1/#8,
// EC, OPENSSH, ENCRYPTED, ...) and of an armored PGP private key block.
const (
	pemBeginRe = `-----BEGIN [A-Z0-9 ]{0,40}PRIVATE KEY(?: BLOCK)?-----`
	pemEndRe   = `-----END [A-Z0-9 ]{0,40}PRIVATE KEY(?: BLOCK)?-----`
)

var (
	pemBegin = regexp.MustCompile(pemBeginRe)
	pemEnd   = regexp.MustCompile(pemEndRe)
)

// redactRules run in this order: the specific token formats first, the generic key=value last, so a
// value already replaced by a specific rule ("[redacted:...]") is not matched again (values never
// contain '['). All are RE2 (linear time), and a variable-length value is matched whole (no upper
// bound), so a long credential is never replaced only in part. The distinctive GitHub prefixes match
// inside a word ("mcp__x__ghp_..."); the short "sk-" only at a word start ("task-..." is no key).
// A boundary before a value is a captured class (RE2 has no lookaround) outside the secret group,
// and none is matched after it, so two values one separator apart are both found.
var redactRules = []redactRule{
	{"private-key", regexp.MustCompile(pemBeginRe + `[\s\S]*?` + pemEndRe), 0},
	// A key block cut off before its END line: the header and the base64 lines after it.
	{"private-key", regexp.MustCompile(pemBeginRe + `(?:\r?\n[A-Za-z0-9+/=:\- ]+)*`), 0},
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
	n := 0
	if unscanned > 0 {
		s, n = redactTailCutPEM(s)
	}
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

// A cut can split a PEM private key block, and dropping the partial line at the cut may take its
// header or footer with it: what is left (base64 lines and the other marker) matches no rule. So a
// text cut at its end loses everything from a BEGIN line with no END line after it, and a text cut
// at its start everything up to an END line with no BEGIN line before it; both become
// [redacted:private-key]. A complete block is left to Redact.

// redactTailCutPEM redacts a text cut at its end (more followed): from the first BEGIN line after
// the last END line (a block whose END was cut off) to the end of the text.
func redactTailCutPEM(s string) (string, int) {
	from := 0
	if ends := pemEnd.FindAllStringIndex(s, -1); len(ends) > 0 {
		from = ends[len(ends)-1][1]
	}
	b := pemBegin.FindStringIndex(s[from:])
	if b == nil {
		return s, 0
	}
	return s[:from+b[0]] + "[redacted:private-key]\n", 1
}

// redactHeadCutPEM redacts a text cut at its start (more preceded it): from the start through the
// last END line before the first BEGIN line (a block whose BEGIN was cut off), that whole line.
func redactHeadCutPEM(s string) (string, int) {
	to := len(s)
	if b := pemBegin.FindStringIndex(s); b != nil {
		to = b[0]
	}
	ends := pemEnd.FindAllStringIndex(s[:to], -1)
	if len(ends) == 0 {
		return s, 0
	}
	end := ends[len(ends)-1][1]
	if i := strings.IndexByte(s[end:], '\n'); i >= 0 {
		end += i
	} else {
		end = len(s)
	}
	return "[redacted:private-key]" + s[end:], 1
}

// RedactText is Redact without the count.
func RedactText(s string) string {
	out, _ := Redact(s)
	return out
}

// RedactRead redacts a text read up to a byte cap: when truncated (the file went on), the last
// incomplete line is dropped whole first (all of the text when it has no line break), so a value cut
// by the read cap is not shown in part; a PEM key block cut off by it is redacted from its BEGIN line
// on (redactTailCutPEM).
func RedactRead(s string, truncated bool) string {
	if truncated {
		s, _ = redactTailCutPEM(s[:lineCut(s)])
	}
	return RedactText(s)
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
	if m.Cfg.KeepRawReports {
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
