package jobs

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"unicode/utf8"

	"github.com/qwwiwi/agent-jobs/internal/store"
)

// MaxRedactBytes caps the text Redact scans. RE2 is linear, so this bounds the time, not the risk of
// backtracking; what lies past the cap is not shown at all (it could not be checked).
const MaxRedactBytes = 4 << 20

// RawReportName is the unredacted copy of report.md, kept (0600) only with keep_raw_reports.
const RawReportName = "report.raw.md"

// redactRule is one secret format: what a match is called and which submatch is the secret (0 = all).
type redactRule struct {
	kind  string
	re    *regexp.Regexp
	group int
}

// redactRules run in this order: the specific token formats first, the generic key=value last, so a
// value already replaced by a specific rule ("[redacted:...]") is not matched again (values never
// contain '['). All are RE2 (linear time, mostly bounded repeats). The distinctive GitHub prefixes
// match inside a word ("mcp__x__ghp_..."); the short "sk-" only at a word start ("task-..." is no key).
var redactRules = []redactRule{
	{"private-key", regexp.MustCompile(`-----BEGIN [A-Z0-9 ]{0,40}PRIVATE KEY-----[\s\S]*?-----END [A-Z0-9 ]{0,40}PRIVATE KEY-----`), 0},
	// A key block cut off before its END line: the header and the base64 lines after it.
	{"private-key", regexp.MustCompile(`-----BEGIN [A-Z0-9 ]{0,40}PRIVATE KEY-----(?:\r?\n[A-Za-z0-9+/=:\- ]+)*`), 0},
	{"anthropic", regexp.MustCompile(`\bsk-ant-[A-Za-z0-9_-]{20,400}`), 0},
	{"openai", regexp.MustCompile(`\bsk-(?:proj-|svcacct-|admin-)?[A-Za-z0-9_-]{20,400}`), 0},
	{"github", regexp.MustCompile(`gh[pousr]_[A-Za-z0-9]{30,255}`), 0},
	{"github", regexp.MustCompile(`github_pat_[A-Za-z0-9_]{22,255}`), 0},
	{"slack", regexp.MustCompile(`\bxox[abposr]-[A-Za-z0-9-]{10,255}`), 0},
	{"aws-key-id", regexp.MustCompile(`\b(?:AKIA|ASIA)[0-9A-Z]{16}\b`), 0},
	{"aws-secret", regexp.MustCompile(`(?i)aws_secret_access_key["']?\s{0,4}[:=]\s{0,4}["']?([A-Za-z0-9/+=]{40})`), 1},
	{"google", regexp.MustCompile(`\bAIza[0-9A-Za-z_-]{35}`), 0},
	{"telegram", regexp.MustCompile(`(?:^|[^0-9A-Za-z_-])([0-9]{8,10}:[A-Za-z0-9_-]{35})(?:[^A-Za-z0-9_-]|$)`), 1},
	{"jwt", regexp.MustCompile(`\beyJ[A-Za-z0-9_-]{8,1000}\.[A-Za-z0-9_-]{8,1000}\.[A-Za-z0-9_-]{8,1000}`), 0},
	{"bearer", regexp.MustCompile(`(?i)\bbearer[ \t]{1,8}([A-Za-z0-9._~+/=-]{16,1000})`), 1},
	{"secret", regexp.MustCompile(`(?i)[A-Za-z0-9_.-]{0,40}(?:token|secret|password|passwd|api[_-]?key)[A-Za-z0-9_.-]{0,40}["']?[ \t]{0,4}[:=][ \t]{0,4}["']?([^\s"'` + "`" + `,;\[\]{}()<>]{16,1000})`), 1},
}

// Redact replaces secret-looking values in s with [redacted:<kind>] and returns the text and the
// number of values replaced. Text past MaxRedactBytes is dropped with a note.
func Redact(s string) (string, int) {
	tail := ""
	if len(s) > MaxRedactBytes {
		cut := MaxRedactBytes
		for cut > 0 && !utf8.RuneStart(s[cut]) {
			cut--
		}
		tail = fmt.Sprintf("\n[redacted:unscanned %d bytes past the %d-byte scan cap]\n", len(s)-cut, MaxRedactBytes)
		s = s[:cut]
	}
	n := 0
	for _, r := range redactRules {
		var k int
		s, k = r.apply(s)
		n += k
	}
	if tail != "" {
		n++
	}
	return s + tail, n
}

// RedactText is Redact without the count.
func RedactText(s string) string {
	out, _ := Redact(s)
	return out
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

// redactReport redacts report.md of a job that has ended, in place. With keep_raw_reports the
// original is kept next to it as report.raw.md (0600); otherwise it is gone. The job's processes
// are over by now, but the job dir was the job's: the file is opened without following a link and
// only a regular file is touched.
func (m *Manager) redactReport(id string) {
	dir := m.JobDir(id)
	p := filepath.Join(dir, "report.md")
	f, ok, err := openRegular(p)
	if err != nil || !ok {
		if err != nil {
			m.Log.Printf("job %s: redact report: %v", id, err)
		}
		return
	}
	var buf bytes.Buffer
	_, err = buf.ReadFrom(f)
	_ = f.Close()
	if err != nil {
		m.Log.Printf("job %s: redact report: %v", id, err)
		return
	}
	out, n := Redact(buf.String())
	if n == 0 {
		return
	}
	if m.Cfg.KeepRawReports {
		raw := filepath.Join(dir, RawReportName)
		if err := os.Remove(raw); err != nil && !os.IsNotExist(err) {
			m.Log.Printf("job %s: redact report: %v", id, err)
			return
		}
		if err := createSynced(raw, buf.Bytes()); err != nil {
			m.Log.Printf("job %s: keep raw report: %v (report.md left unredacted)", id, err)
			return
		}
	}
	tmp := p + ".tmp"
	if err := os.Remove(tmp); err != nil && !os.IsNotExist(err) {
		m.Log.Printf("job %s: redact report: %v", id, err)
		return
	}
	if err := createSynced(tmp, []byte(out)); err != nil {
		m.Log.Printf("job %s: redact report: %v", id, err)
		return
	}
	if err := os.Rename(tmp, p); err != nil {
		m.Log.Printf("job %s: redact report: %v", id, err)
		return
	}
	m.Log.Printf("job %s: report.md: %d secret-looking value(s) redacted", id, n)
}

// settled runs once a job reached a terminal or stopped status: its report is redacted in place and
// its stats row written.
func (m *Manager) settled(id string) {
	m.redactReport(id)
	m.recordStatsOf(id)
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
