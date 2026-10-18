package jobs

import (
	"context"
	"fmt"
	"io"
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
	var body []string
	for i := range 8 {
		// 64 base64 characters, distinct per line.
		body = append(body, fmt.Sprintf("K%02dPEMbody", i)+rep("Zq8+", 13)+"x/")
	}
	lines := append([]string{"-----" + "BEGIN " + label + "-----"}, headers...)
	lines = append(lines, body...)
	lines = append(lines, "-----"+"END "+label+"-----")
	return testPEM{text: strings.Join(lines, "\n"), body: body}
}

// leaks reports whether out holds any 16-byte piece of a body line of the key.
func (p testPEM) leaks(out string) bool {
	for _, l := range p.body {
		for i := 0; i+16 <= len(l); i++ {
			if strings.Contains(out, l[i:i+16]) {
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
		for _, cut := range []func(string) (string, int){redactHeadCutPEM, redactTailCutPEM} {
			if out, n := cut(in); out != in || n != 0 {
				t.Errorf("%s: a whole block is changed by a cut rule: %q", name, out)
			}
		}
	}
	plain := "## Summary\nok -----END of section\nBEGIN here\n"
	for _, f := range []func(string) (string, int){redactHeadCutPEM, redactTailCutPEM} {
		if out, n := f(plain); out != plain || n != 0 {
			t.Errorf("plain text changed: %q", out)
		}
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
