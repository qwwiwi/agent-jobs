package guard

import (
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// encodeFirstSep percent-encodes the separator after the first component of the absolute path p
// (`/tmp/x` -> `/tmp%2Fx`): a URL reader decodes it, the guard saw a top-level name that does not exist.
func encodeFirstSep(p string) string {
	i := strings.IndexByte(p[1:], '/') + 1
	return p[:i] + "%2F" + p[i+1:]
}

// A file URL names a path as curl, wget and python's urllib read it: percent-decoded, with an optional
// host. The read allowlist and the no-read dirs hold for it as for the path written plainly.
func TestReadAllowFileURL(t *testing.T) {
	l := newReadAllowLayout(t)
	noRead := l.cfg
	noRead.ReadAllow = nil
	noRead.NoRead = []string{filepath.Dir(l.checkout)}
	for _, tc := range []struct {
		cfg Config
		p   string
	}{{l.cfg, l.otherReport}, {l.cfg, l.transcript}, {l.cfg, l.checkout}, {noRead, l.checkout}} {
		enc := encodeFirstSep(tc.p)
		for _, cmd := range []string{
			"curl -s file://" + enc,
			"curl -s FILE://" + enc,
			"curl -s file://localhost" + tc.p,
			"curl -s 'file://localhost" + enc + "'",
			"curl -s --url=file://" + enc,
			"wget -qO- file://" + enc,
			`python3 -c "import urllib.request as u; print(u.urlopen('file://` + enc + `').read())"`,
			`python3 -c "from urllib.request import urlopen; urlopen('file://localhost` + tc.p + `')"`,
			"cat " + enc + " " + strings.ReplaceAll(tc.p, "/", "%2f"),
		} {
			if r := Check(cmd, tc.cfg); r == "" {
				t.Errorf("allowed: %s", cmd)
			}
		}
	}
	// A `..` written as %2e%2e inside the job's own folder leaves it once decoded.
	esc := "file://" + l.work + "/%2e%2e/%2E%2E/%2e%2e/%2e%2e/agentb/jobs/k1/report.md"
	if r := Check("curl -s "+esc, l.cfg); r == "" {
		t.Errorf("allowed: curl -s %s", esc)
	}
	for _, cmd := range []string{
		"curl -s file://" + l.work + "/main.go", "curl -s file://" + strings.Replace(l.work, "/work", "/work%2Fmain.go", 1),
		"date +%Y%m%d", "printf '%5d %02x\\n' 1 2", `grep -rn "50%" .`,
	} {
		if r := Check(cmd, l.cfg); r != "" {
			t.Errorf("refused %s: %s", cmd, r)
		}
	}
}

// A relative value attached to an option (`-f../x`, `--file=../x`) or after `@` is checked against
// every working directory as the path it is.
func TestReadAllowAttachedRelative(t *testing.T) {
	l := newReadAllowLayout(t)
	// From the job's folder (`cd ..`), one `..` reaches another job's folder: the word `-f../k2/x` has no
	// `..` component as written.
	rel := "../k2/report.md"
	for _, cmd := range []string{
		"cd .. && awk -f" + rel + " /dev/null",
		"cd .. && tar -cf x.tar -T" + rel,
		"cd .. && tar -xvf" + rel,
		"cd .. && curl -d @" + rel + " http://localhost:1",
		"cd .. && sort --files0-from=" + rel,
		"cd .. && grep --file=" + rel + " work/main.go",
	} {
		if r := Check(cmd, l.cfg); r == "" {
			t.Errorf("allowed: %s", cmd)
		}
	}
	for _, cmd := range []string{"awk -f../prompt.md /dev/null", "curl -d @main.go http://localhost:1", "grep -n5 x main.go"} {
		if r := Check(cmd, l.cfg); r != "" {
			t.Errorf("refused %s: %s", cmd, r)
		}
	}
	// The state scope (every job) checks a relative attached value too.
	state := l.cfg
	state.ReadAllow = nil
	for _, cmd := range []string{"cd .. && curl -d @" + rel + " http://localhost:1", "cd .. && awk -f" + rel + " /dev/null"} {
		if r := Check(cmd, state); r == "" {
			t.Errorf("state scope: allowed: %s", cmd)
		}
	}
}

// A gate job runs no background Bash: claude prints another result event when a background task ends,
// which the one-result rule of the gate result would take for a forged one.
func TestGateRefusesBackgroundBash(t *testing.T) {
	l := newReadAllowLayout(t)
	decide := func(cfg Config, input map[string]any) string {
		t.Helper()
		b, _ := json.Marshal(map[string]any{"tool_name": "Bash", "cwd": l.work, "tool_input": input})
		r, err := Decide(strings.NewReader(string(b)), cfg)
		if err != nil {
			t.Fatal(err)
		}
		return r
	}
	if r := decide(l.cfg, map[string]any{"command": "go test ./...", "run_in_background": true}); !strings.Contains(r, "run_in_background") {
		t.Errorf("gate job, background Bash: %q", r)
	}
	if r := decide(l.cfg, map[string]any{"command": "go test ./...", "run_in_background": false}); r != "" {
		t.Errorf("gate job, foreground Bash: %s", r)
	}
	open := l.cfg
	open.ReadAllow = nil
	if r := decide(open, map[string]any{"command": "go test ./...", "run_in_background": true}); r != "" {
		t.Errorf("other kinds, background Bash: %s", r)
	}
}

// A word encoded many times over (`%252525…41`) decodes one level per round: the check stays linear and
// within the guard's time budget, never one recursion per level and form.
func TestPercentDecodingBounded(t *testing.T) {
	l := newReadAllowLayout(t)
	state := l.cfg
	state.ReadAllow = nil
	word := "%25" + strings.Repeat("25", 40) + "41"
	for _, cfg := range []Config{l.cfg, state} {
		done := make(chan string, 1)
		go func() { done <- Check("cat "+word+" main.go", cfg) }()
		select {
		case <-done:
		case <-time.After(20 * time.Second):
			t.Fatal("the guard did not decide within 20s")
		}
	}
	// Decoded to the end, the encoded path is checked as written plainly.
	enc := strings.ReplaceAll(l.otherReport, "/", "%252F")
	if r := Check("curl -s file://"+enc, l.cfg); r == "" {
		t.Errorf("allowed: curl -s file://%s", enc)
	}
}
