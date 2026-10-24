package guard

import (
	"path/filepath"
	"strings"
	"testing"
)

// fileURLCmds are the spellings that open a file URL: curl, wget, python's urllib inline and from a
// here-document, and xargs fed the URL by an earlier stage.
func fileURLCmds(u string) []string {
	return []string{
		"curl -s '" + u + "'",
		"wget -qO- '" + u + "'",
		`python3 -c "import urllib.request as u; u.urlopen('` + u + `')"`,
		"python3 - <<'EOF'\nimport urllib.request\nurllib.request.urlopen('" + u + "')\nEOF",
		"echo '" + u + "' | xargs curl -s",
	}
}

// Under a read allowlist a file URL with a query, a fragment or a `.`/`..` segment, as written or
// percent-decoded, is refused: a URL reader cuts the path at `?` or `#`, so the path the guard would
// check is not the one read. A plain file URL inside the allowlist stays readable, and kinds without
// a read allowlist keep their behaviour.
func TestReadAllowFileURLFailClosed(t *testing.T) {
	l := newReadAllowLayout(t)
	home := filepath.Dir(filepath.Dir(filepath.Dir(filepath.Dir(l.own))))
	// From the other agent's report (as a component `report.md#`) back into the job's own folder.
	back := "/../../../../thrall/jobs/j1/prompt.md"
	noRead := l.cfg
	noRead.ReadAllow = append(append([]string{}, l.cfg.ReadAllow...), home)
	noRead.NoRead = []string{filepath.Dir(l.checkout)}
	for _, tc := range []struct {
		name string
		cfg  Config
		url  string
	}{
		{"dotdot", l.cfg, "file:///a/b/../../x"},
		{"dotdot_nonexistent", l.cfg, "file://" + l.work + "/nonexistent/../../prompt.md"},
		{"query", l.cfg, "file:///x?y"},
		{"fragment", l.cfg, "file:///x#y"},
		{"fragment_escape", l.cfg, "file://" + l.otherReport + "#" + back},
		{"query_escape", l.cfg, "file://" + l.otherReport + "?" + back},
		{"pct_lower", l.cfg, "file://" + l.otherReport + "%23" + strings.ReplaceAll(back, "..", "%2e%2e")},
		{"pct_upper", l.cfg, "file://" + l.otherReport + "%23" + strings.ReplaceAll(back, "..", "%2E%2E")},
		{"pct_nonexistent", l.cfg, "file://" + l.work + "/nonexistent/%2e%2e/%2E%2E/prompt.md"},
		{"pct_twice", l.cfg, "file://" + l.otherReport + "%2523" + strings.ReplaceAll(back, "..", "%252e%252E")},
		{"localhost", l.cfg, "file://localhost" + l.otherReport + "#" + back},
		{"one_slash", l.cfg, "file:" + l.otherReport + "#" + back},
		{"dot_upper_scheme", l.cfg, "FILE://" + l.work + "/./main.go"},
		{"no_read", noRead, "file://" + l.checkout + "#/../../.agent-jobs/thrall/jobs/j1/prompt.md"},
	} {
		for _, cmd := range fileURLCmds(tc.url) {
			if r := Check(cmd, tc.cfg); r == "" {
				t.Errorf("%s: allowed: %s", tc.name, cmd)
			}
		}
	}
	for _, u := range []string{
		"file://" + l.work + "/main.go", "file://localhost" + l.work + "/main.go", "file:" + l.work + "/main.go",
		"FILE://" + strings.Replace(l.work, "/work", "/work%2Fmain.go", 1),
	} {
		for _, cmd := range fileURLCmds(u) {
			if r := Check(cmd, l.cfg); r != "" {
				t.Errorf("refused %s: %s", cmd, r)
			}
		}
	}
	// Kinds without a read allowlist: a query or a fragment stays allowed, a dot segment is refused by
	// the state and no-read checks (file_url_decoding_test.go); a job without read scopes keeps both.
	state := l.cfg
	state.ReadAllow = nil
	state.NoRead = []string{filepath.Dir(l.checkout)}
	dot := "file://" + l.work + "/./main.go"
	for _, cfg := range []Config{state, {Dir: l.work}} {
		for _, cmd := range fileURLCmds("file:///x#y") {
			if r := Check(cmd, cfg); r != "" {
				t.Errorf("no read allowlist: refused %s: %s", cmd, r)
			}
		}
	}
	for _, cmd := range fileURLCmds(dot) {
		if r := Check(cmd, state); r == "" {
			t.Errorf("no read allowlist: allowed %s", cmd)
		}
		if r := Check(cmd, Config{Dir: l.work}); r != "" {
			t.Errorf("no read scopes: refused %s: %s", cmd, r)
		}
	}
}
