package jobs

import (
	"context"
	"encoding/base64"
	"io"
	"math/rand/v2"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

// testPEM is a private key block of the given label ("RSA PRIVATE KEY", "PGP PRIVATE KEY BLOCK", ...)
// with optional header lines; built from pieces, so no file of the repository holds a key block.
type testPEM struct {
	text string   // the whole block, no trailing line break
	body []string // its base64 lines
}

func newTestPEM(label string, headers ...string) testPEM {
	// A pseudo-random body (fixed seed per label): 8 lines of 64 base64 characters and a short last one.
	r := rand.New(rand.NewPCG(uint64(len(label)), uint64(len(headers))))
	raw := make([]byte, 8*48+10)
	for i := range raw {
		raw[i] = byte(r.Uint32())
	}
	enc := base64.StdEncoding.EncodeToString(raw)
	var body []string
	for len(enc) > 64 {
		body, enc = append(body, enc[:64]), enc[64:]
	}
	body = append(body, enc)
	lines := append([]string{"-----" + "BEGIN " + label + "-----"}, headers...)
	lines = append(lines, body...)
	lines = append(lines, "-----"+"END "+label+"-----")
	return testPEM{text: strings.Join(lines, "\n"), body: body}
}

// leaks reports whether out holds any 8-byte piece of a body line of the key.
func (p testPEM) leaks(out string) bool {
	for _, l := range p.body {
		for i := 0; i+8 <= len(l); i++ {
			if strings.Contains(out, l[i:i+8]) {
				return true
			}
		}
	}
	return false
}

// cutPoints are offsets into p.text: inside the BEGIN line, inside the body, inside the END line.
func (p testPEM) cutPoints() map[string]int {
	first := strings.IndexByte(p.text, '\n')
	last := strings.LastIndexByte(p.text, '\n')
	return map[string]int{
		"in BEGIN line": first / 2,
		"in body":       first + 1 + (last-first)/2 + 7,
		"in END line":   last + 1 + (len(p.text)-last)/2,
	}
}

func testPEMs() map[string]testPEM {
	return map[string]testPEM{
		"rsa":       newTestPEM("RSA PRIVATE KEY"),
		"openssh":   newTestPEM("OPENSSH PRIVATE KEY"),
		"ec":        newTestPEM("EC PRIVATE KEY"),
		"pkcs8 enc": newTestPEM("ENCRYPTED PRIVATE KEY"),
		"rsa enc":   newTestPEM("RSA PRIVATE KEY", "Proc-Type: 4,ENCRYPTED", "DEK-Info: AES-128-CBC,0011AABB", ""),
		"pgp":       newTestPEM("PGP PRIVATE KEY BLOCK", "Version: GnuPG v2", ""),
	}
}

// A complete key block is redacted, and text around it (and text with no key) is left as is.
func TestRedactPEMWhole(t *testing.T) {
	for name, p := range testPEMs() {
		in := "before\n" + p.text + "\nafter\n"
		out := RedactText(in)
		if p.leaks(out) || out != "before\n[redacted:private-key]\nafter\n" {
			t.Errorf("%s: %q", name, out)
		}
	}
	plain := "## Summary\nok -----END of section\nBEGIN here\n"
	if out := RedactText(plain); out != plain {
		t.Errorf("plain text changed: %q", out)
	}
	if out := RedactRead(plain, true); out != plain {
		t.Errorf("RedactRead changed plain text: %q", out)
	}
}

// A report read cut at the read cap (RedactRead) inside a key block shows no part of its body.
func TestRedactReadPEMCut(t *testing.T) {
	for name, p := range testPEMs() {
		in := "## Summary\nok\n" + p.text + "\nafter\n"
		for where, at := range p.cutPoints() {
			out := RedactRead(in[:len("## Summary\nok\n")+at], true)
			if p.leaks(out) || !strings.HasPrefix(out, "## Summary\nok\n") {
				t.Errorf("%s, cut %s: %q", name, where, out)
			}
		}
	}
}

// An oversized report cut at the scan cap inside a key block, by Redact and in place (redactFile):
// no part of the key body is left.
func TestRedactOversizedPEMCut(t *testing.T) {
	m := newManager(t)
	head := "## Summary\nok\n"
	// One label only (the one with header lines no base64 rule matches): each case scans 4 MiB twice.
	for _, name := range []string{"rsa enc"} {
		p := testPEMs()[name]
		for where, at := range p.cutPoints() {
			pad := MaxRedactBytes - len(head) - 1 - at // the cap lands at offset at of the block
			big := head + rep(" ", pad) + "\n" + p.text + "\nafter\n"
			if out, _ := Redact(big); p.leaks(out) || !strings.HasPrefix(out, head) {
				t.Errorf("Redact %s, cut %s: tail %q", name, where, out[max(0, len(out)-200):])
			}
			id := "big"
			dir := m.JobDir(id)
			if err := os.MkdirAll(dir, 0o700); err != nil {
				t.Fatal(err)
			}
			rp := filepath.Join(dir, "report.md")
			if err := os.WriteFile(rp, []byte(big), 0o600); err != nil {
				t.Fatal(err)
			}
			if err := m.redactReport(id); err != nil {
				t.Fatal(err)
			}
			if b := readFile(t, rp); p.leaks(b) || !strings.HasPrefix(b, head) {
				t.Errorf("report.md %s, cut %s: tail %q", name, where, b[max(0, len(b)-200):])
			}
		}
	}
}

// The acceptance log keeps the last AcceptanceLogBytes; a cut inside a key block leaves no part of
// its body in the log on disk, nor in log_tail.
func TestAcceptanceLogWriteCapPEMCut(t *testing.T) {
	for name, p := range testPEMs() {
		for where, at := range p.cutPoints() {
			state := t.TempDir()
			dir := filepath.Join(state, "jobs", "j1")
			if err := os.MkdirAll(filepath.Join(dir, "work"), 0o700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(dir, "key.txt"), []byte(p.text+"\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			// Output: block, "\n", k bytes, "\nEND\n"; the kept tail starts at offset at of the block.
			k := AcceptanceLogBytes - (len(p.text) - at) - 1 - 5
			cmd := "cat ../key.txt; head -c " + strconv.Itoa(k) + " /dev/zero | tr '\\0' a; printf '\\nEND\\n'"
			if exit, _ := runAcceptance(context.Background(), dir, cmd, time.Minute, io.Discard, nil); exit != 0 {
				t.Fatalf("exit %d", exit)
			}
			log := readFile(t, filepath.Join(dir, AcceptanceLog))
			if p.leaks(log) || !strings.HasPrefix(log, "[... ") || !strings.HasSuffix(log, "END\n") {
				t.Errorf("%s, cut %s: log starts %q", name, where, log[:min(len(log), 200)])
			}
			tail, err := ReadAcceptanceLogTail(state, "j1", int64(acceptanceReadCap))
			if err != nil || p.leaks(tail) {
				t.Errorf("%s, cut %s: tail starts %q, err %v", name, where, tail[:min(len(tail), 200)], err)
			}
		}
	}
}

// A raw acceptance log larger than the read window, the window starting inside a key block: no
// part of its body in log_tail.
func TestAcceptanceLogReadWindowPEMCut(t *testing.T) {
	for name, p := range testPEMs() {
		for where, at := range p.cutPoints() {
			after := "\n" + rep("z", acceptanceReadCap-(len(p.text)-at)-1-5) + "\nEND\n"
			body := "start\n" + p.text + after
			if off := len(body) - acceptanceReadCap; off != len("start\n")+at {
				t.Fatalf("the read window starts at %d, not at %d", off, len("start\n")+at)
			}
			state := writeAcceptanceLog(t, body)
			tail, err := ReadAcceptanceLogTail(state, "j1", int64(acceptanceReadCap))
			if err != nil || p.leaks(tail) || !strings.HasSuffix(tail, "END\n") {
				t.Errorf("%s, cut %s: tail starts %q, err %v", name, where, tail[:min(len(tail), 200)], err)
			}
		}
	}
}

// log_tail cut to max inside a key block, the log read whole (the block redacted before the cut)
// and a log holding a headerless block (a cut made by an earlier build): no part of the body.
func TestAcceptanceLogTailPEMCut(t *testing.T) {
	for name, p := range testPEMs() {
		suffix := "\nafter\nEND\n"
		for where, at := range p.cutPoints() {
			state := writeAcceptanceLog(t, "start\n"+p.text+suffix)
			tail, err := ReadAcceptanceLogTail(state, "j1", int64(len(p.text)-at+len(suffix)))
			if err != nil || p.leaks(tail) || !strings.HasSuffix(tail, "END\n") {
				t.Errorf("%s, cut %s: tail %q, err %v", name, where, tail, err)
			}
			raw := p.text[strings.IndexByte(p.text[at:], '\n')+at+1:] + suffix
			state = writeAcceptanceLog(t, "start\n"+raw)
			tail, err = ReadAcceptanceLogTail(state, "j1", int64(len(raw)-3))
			if err != nil || p.leaks(tail) || !strings.HasSuffix(tail, "END\n") {
				t.Errorf("%s headerless, cut %s: tail %q, err %v", name, where, tail, err)
			}
		}
	}
}

// Key blocks as they appear in files and logs: CRLF line ends, indented (YAML, a tab), blank lines,
// PGP metadata and an encrypted PKCS#1 header with commas; whole and cut off before their END line.
func TestRedactPEMLayouts(t *testing.T) {
	p := newTestPEM("RSA PRIVATE KEY", "Proc-Type: 4,ENCRYPTED", "DEK-Info: DES-EDE3-CBC,A1B2C3D4E5F60718", "")
	g := newTestPEM("PGP PRIVATE KEY BLOCK", "Version: GnuPG v2", "Comment: a key, of mine: test", "", "")
	layouts := map[string]func(string) string{
		"lf":       func(s string) string { return s },
		"crlf":     func(s string) string { return strings.ReplaceAll(s, "\n", "\r\n") },
		"spaces":   func(s string) string { return "    " + strings.ReplaceAll(s, "\n", "\n    ") },
		"tab":      func(s string) string { return "\t" + strings.ReplaceAll(s, "\n", "\n\t") },
		"blank":    func(s string) string { return strings.Replace(s, "\n", "\n\n", 3) },
		"crlf tab": func(s string) string { return "\t" + strings.ReplaceAll(s, "\n", "\r\n\t") },
	}
	for kname, k := range map[string]testPEM{"encrypted rsa": p, "pgp": g} {
		for lname, f := range layouts {
			block := f(k.text)
			whole := "key:\n" + block + "\nafter\n"
			if out := RedactText(whole); k.leaks(out) || !strings.HasPrefix(out, "key:\n") ||
				!strings.HasSuffix(out, "after\n") || !strings.Contains(out, "[redacted:private-key]") {
				t.Errorf("%s %s whole: %q", kname, lname, out)
			}
			// No END line: everything from the BEGIN line on goes.
			open := "key:\n" + block[:strings.LastIndexByte(block, '\n')] + "\n"
			if out := RedactText(open); k.leaks(out) || !strings.HasPrefix(out, "key:\n") {
				t.Errorf("%s %s open: %q", kname, lname, out)
			}
			// No BEGIN line: the body run above the END line goes, the prose above it stays.
			orphan := "prose line\n" + block[strings.IndexByte(block, '\n')+1:] + "\nafter\n"
			if out := RedactText(orphan); k.leaks(out) || !strings.HasPrefix(out, "prose line\n") ||
				!strings.HasSuffix(out, "after\n") {
				t.Errorf("%s %s orphan: %q", kname, lname, out)
			}
		}
	}
}

// A marker quoted inside prose is no marker: a report that talks about key blocks keeps its text.
func TestRedactPEMInlineMentionKept(t *testing.T) {
	b, e := "-----"+"BEGIN RSA PRIVATE KEY-----", "-----"+"END OPENSSH PRIVATE KEY-----"
	for _, s := range []string{
		"## Summary\nThe redactor drops a block from `" + b + "` on.\nNext line stays.\nAnd this one.\n",
		"## Findings\nA file ending in " + e + " was seen.\n" + rep("A", 64) + "\nmore prose\n",
		"## Notes\nkeys start with " + b + "\nand end with " + e + "\n- done\n",
		"## Summary\nok\n" + rep("a", 64) + "\n" + "b\n",
	} {
		if out := RedactText(s); out != s {
			t.Errorf("changed:\n%q\n%q", s, out)
		}
		if out := RedactRead(s, true); out != s {
			t.Errorf("RedactRead changed:\n%q\n%q", s, out)
		}
	}
}

// A log capped by an earlier build: the cut note, then the body and END line of a block whose BEGIN
// line was cut off. Read whole (max covers the file, no cut on this read), log_tail shows no body.
func TestAcceptanceLogPreviouslyCappedOrphan(t *testing.T) {
	for name, p := range testPEMs() {
		raw := p.text[strings.IndexByte(p.text, '\n')+1:]
		raw = raw[strings.IndexByte(raw, '\n')+1:] // a later body line first: the note's neighbour
		body := "[... 70000 earlier bytes cut, last 900 kept ...]\n" + raw + "\nafter\nEND\n"
		state := writeAcceptanceLog(t, body)
		for _, max := range []int64{int64(len(body)), int64(len(body)) + 100, int64(acceptanceReadCap)} {
			tail, err := ReadAcceptanceLogTail(state, "j1", max)
			if err != nil || p.leaks(tail) || !strings.HasSuffix(tail, "after\nEND\n") {
				t.Errorf("%s, max %d: %q, err %v", name, max, tail, err)
			}
		}
	}
}

// A key larger than the write cap leaves the log on disk holding only its middle: the BEGIN line
// was evicted, the command was killed before the END line (none in the output at all).
func TestAcceptanceLogEvictedOpenKey(t *testing.T) {
	var body []string
	r := rand.New(rand.NewPCG(7, 7))
	for range AcceptanceLogBytes/64 + 200 {
		raw := make([]byte, 48)
		for i := range raw {
			raw[i] = byte(r.Uint32())
		}
		body = append(body, base64.StdEncoding.EncodeToString(raw))
	}
	k := testPEM{body: body}
	state := t.TempDir()
	dir := filepath.Join(state, "jobs", "j1")
	if err := os.MkdirAll(filepath.Join(dir, "work"), 0o700); err != nil {
		t.Fatal(err)
	}
	text := "start\n-----" + "BEGIN OPENSSH PRIVATE KEY-----\n" + strings.Join(body, "\n") + "\n"
	if err := os.WriteFile(filepath.Join(dir, "key.txt"), []byte(text), 0o600); err != nil {
		t.Fatal(err)
	}
	if exit, _ := runAcceptance(context.Background(), dir, "cat ../key.txt", time.Minute, io.Discard, nil); exit != 0 {
		t.Fatalf("exit %d", exit)
	}
	log := readFile(t, filepath.Join(dir, AcceptanceLog))
	if k.leaks(log) || !strings.HasPrefix(log, "[... ") {
		t.Errorf("log on disk: %q", log[:min(len(log), 300)])
	}
	tail, err := ReadAcceptanceLogTail(state, "j1", 4096)
	if err != nil || k.leaks(tail) {
		t.Errorf("tail %q, err %v", tail, err)
	}
	// The same eviction with the key closed in the evicted part: what follows is kept.
	text = "-----" + "BEGIN OPENSSH PRIVATE KEY-----\nAAAA\n-----" + "END OPENSSH PRIVATE KEY-----\n" +
		rep(rep("y", 99)+"\n", AcceptanceLogBytes/100+50) + "tail line\n"
	if err := os.WriteFile(filepath.Join(dir, "key.txt"), []byte(text), 0o600); err != nil {
		t.Fatal(err)
	}
	if exit, _ := runAcceptance(context.Background(), dir, "cat ../key.txt", time.Minute, io.Discard, nil); exit != 0 {
		t.Fatalf("exit %d", exit)
	}
	if log := readFile(t, filepath.Join(dir, AcceptanceLog)); strings.Contains(log, "private-key") ||
		!strings.HasSuffix(log, rep("y", 99)+"\ntail line\n") {
		t.Errorf("closed key: log %q", log[max(0, len(log)-300):])
	}
}

// A window with only the middle of a key and no marker, next to a cut note or at a text's end.
func TestRedactPEMMarkerless(t *testing.T) {
	p := newTestPEM("RSA PRIVATE KEY")
	mid := strings.Join(p.body[2:6], "\n")
	for _, s := range []string{
		"[... 99 earlier bytes cut, last 9 kept ...]\n" + mid + "\nafter\n",
		mid + "\nafter\n",
		"before\n" + mid + "\n",
	} {
		out := RedactText(s)
		if strings.Contains(out, p.body[3][8:24]) || !strings.Contains(out, "[redacted:private-key]") {
			t.Errorf("%q", out)
		}
	}
	// A big report cut at the scan cap in the middle of a markerless run.
	big := "## Summary\n" + rep(" ", MaxRedactBytes-len("## Summary\n")-len(mid)-1) + "\n" + mid + "\n" + rep("x", 100)
	if out, _ := Redact(big); strings.Contains(out, p.body[3][8:24]) {
		t.Errorf("scan cap: tail %q", out[max(0, len(out)-400):])
	}
}

// The archive copy and the in-place rewrite of a report that holds an orphan body and END line (a
// report cut by an earlier tool) and an open block at its end: neither file keeps any body.
func TestRedactReportArchiveAndRewritePEM(t *testing.T) {
	m := newManager(t)
	a, b := newTestPEM("EC PRIVATE KEY"), newTestPEM("PGP PRIVATE KEY BLOCK", "Version: x", "")
	report := "## Summary\nok\n" + a.text[strings.IndexByte(a.text, '\n')+1:] + "\nmiddle\n" +
		b.text[:strings.LastIndexByte(b.text, '\n')] + "\n"
	dir := m.JobDir("r1")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	rp := filepath.Join(dir, "report.md")
	if err := os.WriteFile(rp, []byte(report), 0o600); err != nil {
		t.Fatal(err)
	}
	dst := filepath.Join(t.TempDir(), "report.md")
	if err := archiveReport(rp, dst); err != nil {
		t.Fatal(err)
	}
	if got := readFile(t, dst); a.leaks(got) || b.leaks(got) || !strings.Contains(got, "\nmiddle\n") {
		t.Errorf("archive: %q", got)
	}
	if err := m.redactReport("r1"); err != nil {
		t.Fatal(err)
	}
	if got := readFile(t, rp); a.leaks(got) || b.leaks(got) || !strings.HasPrefix(got, "## Summary\nok\n") {
		t.Errorf("rewrite: %q", got)
	}
}

// Markers as files and listings decorate them: a quoted assignment (.env, YAML), a diff, a quote;
// and a BEGIN marker followed by escaped line breaks on one line, cut off before its END.
func TestRedactPEMDecoratedMarkers(t *testing.T) {
	p := newTestPEM("RSA PRIVATE KEY")
	lines := strings.Split(p.text, "\n")
	deco := func(first, each, last string) string {
		out := []string{first + lines[0]}
		for _, l := range lines[1 : len(lines)-1] {
			out = append(out, each+l)
		}
		return strings.Join(append(out, last+lines[len(lines)-1]), "\n")
	}
	for name, block := range map[string]string{
		"env":       deco(`KEY="`, "", "") + `"`,
		"yaml":      deco("key: |\n  ", "  ", "  "),
		"diff add":  deco("+", "+", "+"),
		"diff del":  deco("-", "-", "-"),
		"quote":     deco("> ", "> ", "> "),
		"json list": deco(`"`, `"`, `"`) + `",`,
	} {
		in := "loading env\n" + block + "\ndone\n"
		if out := RedactText(in); p.leaks(out) || !strings.HasPrefix(out, "loading env\n") || !strings.HasSuffix(out, "\ndone\n") {
			t.Errorf("%s: %q", name, out)
		}
	}
	esc := `got: "` + strings.Join(lines[:4], `\n`) + `...` + "\nnext line\n"
	if out := RedactText(esc); p.leaks(out) || !strings.HasSuffix(out, "\nnext line\n") {
		t.Errorf("escaped: %q", out)
	}
}

// An armored PGP key whose BEGIN line a cut took: armor headers, a blank line, full body lines, a
// short last one and the checksum line ("=Ab12") right above the END line. The short last line does
// not end the body run when the checksum line (or a blank line) follows it.
func TestRedactPEMOrphanPGPChecksum(t *testing.T) {
	g := newTestPEM("PGP PRIVATE KEY BLOCK", "Version: GnuPG v2", "Comment: test key", "")
	lines := strings.Split(g.text, "\n")
	end := lines[len(lines)-1]
	body := strings.Join(lines[1:len(lines)-1], "\n")
	for name, orphan := range map[string]string{
		"crc":             body + "\n=Ab12\n" + end,
		"blank crc":       body + "\n\n=Ab12\n" + end,
		"crc blank":       body + "\n=Ab12\n\n" + end,
		"crc crlf":        strings.ReplaceAll(body+"\n=Ab12\n"+end, "\n", "\r\n"),
		"crc indented":    "  " + strings.ReplaceAll(body+"\n=Ab12\n"+end, "\n", "\n  "),
		"no headers, crc": strings.Join(g.body, "\n") + "\n=Ab12\n" + end,
	} {
		for bname, before := range map[string]string{
			"prose":    "prose line\n",
			"cut note": "[... 4096 earlier bytes cut]\n",
		} {
			in := before + orphan + "\nafter\n"
			if out := RedactText(in); g.leaks(out) || !strings.HasPrefix(out, before) ||
				!strings.HasSuffix(out, "\nafter\n") || !strings.Contains(out, "[redacted:private-key]") {
				t.Errorf("%s after %s: %q", name, bname, out)
			}
		}
	}
}

// A decorated key block whose BEGIN line is gone: every body line (and header and blank line) carries
// the same diff or quote prefix as its END line, with or without whitespace after it.
func TestRedactPEMDecoratedOrphan(t *testing.T) {
	for kname, p := range map[string]testPEM{
		"rsa":     newTestPEM("RSA PRIVATE KEY"),
		"rsa enc": newTestPEM("RSA PRIVATE KEY", "Proc-Type: 4,ENCRYPTED", "DEK-Info: AES-128-CBC,0011AABB", ""),
		"pgp":     newTestPEM("PGP PRIVATE KEY BLOCK", "Version: GnuPG v2", ""),
	} {
		lines := strings.Split(p.text, "\n")[1:]
		for _, prefix := range []string{"> ", ">", ">> ", "# ", "#", "| ", "|", "* ", "*", "+ ", "+", "- ", "-", "+> ", "\t> ", ">\t"} {
			deco := make([]string, len(lines))
			for i, l := range lines {
				deco[i] = prefix + l
			}
			orphan := strings.Join(deco, "\n")
			for bname, before := range map[string]string{
				"prose":    "prose line\n",
				"cut note": "[... 4096 earlier bytes cut]\n",
				"none":     "",
			} {
				in := before + orphan + "\nafter\n"
				if out := RedactText(in); p.leaks(out) || !strings.HasPrefix(out, before) ||
					!strings.HasSuffix(out, "\nafter\n") || !strings.Contains(out, "[redacted:private-key]") {
					t.Errorf("%s prefix %q after %s: %q", kname, prefix, bname, out)
				}
			}
		}
	}
}
