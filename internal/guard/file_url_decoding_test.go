package guard

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// TestFileURLDecide is an independent closure probe: invoke the hook entry point with actual existing
// paths, checking each read policy separately so another policy cannot mask it.
func TestFileURLDecide(t *testing.T) {
	root := t.TempDir()
	work := filepath.Join(root, "allowed", "work")
	outside := filepath.Join(root, "outside", "secret.txt")
	parent := filepath.Join(root, "allowed", "x")
	for _, p := range []string{outside, parent, filepath.Join(work, "ok.txt")} {
		if err := os.MkdirAll(filepath.Dir(p), 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte("closure fixture\n"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	allow := Config{Dir: work, ReadAllow: []string{work, "/usr", "/bin", "/lib", "/lib64", "/dev/null"}}
	deny := Config{Dir: work, NoRead: []string{filepath.Dir(outside), parent}}
	combined := allow
	combined.NoRead = deny.NoRead
	gate := combined
	gate.ReadOnly = work
	decide := func(t *testing.T, cfg Config, cmd string, bg any) string {
		t.Helper()
		b, err := json.Marshal(map[string]any{"tool_name": "Bash", "cwd": work, "tool_input": map[string]any{"command": cmd, "run_in_background": bg}})
		if err != nil {
			t.Fatal(err)
		}
		reason, err := Decide(strings.NewReader(string(b)), cfg)
		if err != nil {
			t.Fatal(err)
		}
		return reason
	}
	encoded := func(p, sep string) string { return strings.ReplaceAll(p, "/", sep) }
	paths := []string{outside, encodeFirstSep(outside), strings.ReplaceAll(encodeFirstSep(outside), "%2F", "%2f"), encoded(outside, "%2F"), encoded(outside, "%2f"), encoded(outside, "%252F"), encoded(outside, "%252f"), work + "/%2e%2e/%2E%2E/outside/secret.txt", work + "/%2E%2e%2f%2e%2E%2Foutside%2fsecret.txt", work + "/%252e%252e%252f%252E%252E%252Foutside%252fsecret.txt"}
	for name, cfg := range map[string]Config{"allow": allow, "no_read": deny, "both": combined, "gate": gate} {
		t.Run(name, func(t *testing.T) {
			for _, p := range paths {
				for _, prefix := range []string{"file:", "file://", "file://localhost", "FILE://", "FiLe://LOCALHOST"} {
					u := prefix + p
					for _, cmd := range []string{"curl -s '" + u + "'", "curl --url='" + u + "'", "wget -qO- '" + u + "'", `python3 -c "import urllib.request as u; print(u.urlopen('` + u + `').read())"`, `python3 -c "from urllib.request import urlopen; print(urlopen('` + u + `').read())"`, `python -c "import urllib; print(urllib.urlopen('` + u + `').read())"`} {
						if reason := decide(t, cfg, cmd, nil); reason == "" {
							t.Errorf("ALLOWED: %s", cmd)
						}
					}
				}
			}
			for _, cmd := range []string{"grep --file=../../outside/secret.txt ok.txt", "curl -d @../x http://localhost:1", "awk -f../../outside/secret.txt /dev/null"} {
				if reason := decide(t, cfg, cmd, nil); reason == "" {
					t.Errorf("ALLOWED: %s", cmd)
				}
			}
			for _, cmd := range []string{"cat ok.txt", "curl -s file://" + work + "/ok.txt", "curl -s file://localhost" + work + "/ok.txt"} {
				if reason := decide(t, cfg, cmd, nil); reason != "" {
					t.Errorf("REFUSED: %s: %s", cmd, reason)
				}
			}
		})
	}
	if reason := decide(t, gate, "go test ./...", true); !strings.Contains(reason, "run_in_background") {
		t.Errorf("background decision: %q", reason)
	}
}

func TestFileURLIntermediateDecoding(t *testing.T) {
	root := t.TempDir()
	work := filepath.Join(root, "work")
	outside := filepath.Join(root, "outside")
	for _, dir := range []string{work, outside} {
		if err := os.MkdirAll(dir, 0700); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(outside, "secret.txt"), []byte("OUTSIDE_FIXTURE"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(work, "alias%2Fleaf")); err != nil {
		t.Fatal(err)
	}
	cfg := Config{Dir: work, ReadOnly: work, ReadAllow: []string{work, "/usr", "/bin", "/lib", "/lib64", "/dev/null"}, NoRead: []string{outside}}
	u := "file://" + work + "/alias%252Fleaf/secret.txt"
	b, _ := json.Marshal(map[string]any{"tool_name": "Bash", "cwd": work, "tool_input": map[string]any{"command": "curl -s '" + u + "'"}})
	reason, err := Decide(strings.NewReader(string(b)), cfg)
	if err != nil {
		t.Fatal(err)
	}
	data := curlRead(t, u)
	t.Logf("guard=%q curl=%q", reason, data)
	if reason == "" && data == "OUTSIDE_FIXTURE" {
		t.Error("guard allowed a URL whose once-decoded path follows an outside symlink")
	}
}

func TestFileURLDoubleEncodingTraversal(t *testing.T) {
	root := t.TempDir()
	job := filepath.Join(root, "job")
	work := filepath.Join(job, "work")
	outside := filepath.Join(root, "outside")
	for _, dir := range []string{filepath.Join(work, "a%2fb%2fc"), filepath.Join(work, "a", "b", "c"), outside} {
		if err := os.MkdirAll(dir, 0700); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(outside, "secret.txt"), []byte("OUTSIDE_FIXTURE"), 0600); err != nil {
		t.Fatal(err)
	}
	allow := Config{Dir: work, ReadOnly: work, ReadAllow: []string{job, "/usr", "/bin", "/lib", "/lib64", "/dev/null"}}
	deny := Config{Dir: work, ReadOnly: work, NoRead: []string{outside}}
	combined := allow
	combined.NoRead = deny.NoRead
	u := "file://" + work + "/a%252fb%252fc/%2e%2e/%2e%2e/%2e%2e/outside/secret.txt"
	data := curlRead(t, u)
	for name, cfg := range map[string]Config{"allow": allow, "no_read": deny, "both": combined} {
		t.Run(name, func(t *testing.T) {
			b, _ := json.Marshal(map[string]any{"tool_name": "Bash", "cwd": work, "tool_input": map[string]any{"command": "curl -s '" + u + "'"}})
			reason, err := Decide(strings.NewReader(string(b)), cfg)
			if err != nil {
				t.Fatal(err)
			}
			t.Logf("guard=%q curl=%q URL=%s", reason, data, u)
			if reason == "" && data == "OUTSIDE_FIXTURE" {
				t.Error("guard allowed double-encoding traversal without symlinks")
			}
		})
	}
}

// curlRead returns what curl reads at the URL u; the test is skipped without curl.
func curlRead(t *testing.T, u string) string {
	t.Helper()
	if _, err := exec.LookPath("curl"); err != nil {
		t.Skip("curl is not available")
	}
	data, err := exec.Command("curl", "-s", u).Output()
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

// TestFileURLDecision is the guard decision for the URLs of the curl tests, without curl: a file URL
// still percent-encoded after one decode, or with a dot segment as written or decoded once, is refused
// in every read scope (state dir, no-read, read allowlist), while a sqlite URI with a query stays
// allowed in a job without a read allowlist.
func TestFileURLDecision(t *testing.T) {
	root := t.TempDir()
	job := filepath.Join(root, "job")
	work := filepath.Join(job, "work")
	state := filepath.Join(root, "state")
	other := filepath.Join(state, "jobs", "other")
	for _, dir := range []string{filepath.Join(work, "a%2fb%2fc"), filepath.Join(work, "a", "b", "c"), other, filepath.Join(state, "jobs", "own")} {
		if err := os.MkdirAll(dir, 0700); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(other, "secret.txt"), []byte("OUTSIDE_FIXTURE"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(other, filepath.Join(work, "alias%2Fleaf")); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(work, "x.db"), nil, 0600); err != nil {
		t.Fatal(err)
	}
	cfgs := map[string]Config{
		"state":   {Dir: work, StateDir: state, OwnDir: filepath.Join(state, "jobs", "own")},
		"no_read": {Dir: work, NoRead: []string{other}},
		"allow":   {Dir: work, ReadAllow: []string{job, "/usr", "/bin", "/lib", "/lib64", "/dev/null"}},
	}
	decide := func(t *testing.T, cfg Config, cmd string) string {
		t.Helper()
		b, err := json.Marshal(map[string]any{"tool_name": "Bash", "cwd": work, "tool_input": map[string]any{"command": cmd}})
		if err != nil {
			t.Fatal(err)
		}
		reason, err := Decide(strings.NewReader(string(b)), cfg)
		if err != nil {
			t.Fatal(err)
		}
		return reason
	}
	refused := []string{
		"curl -s 'file://" + work + "/alias%252Fleaf/secret.txt'",
		"curl -s 'file://" + work + "/a%252fb%252fc/%2e%2e/%2e%2e/%2e%2e/state/jobs/other/secret.txt'",
		"curl -s 'file://" + work + "/a/%2e%2e/x.db'",
		"curl -s 'file://" + work + "/./x.db'",
		"curl -s 'file:x%2541.db'",
	}
	for name, cfg := range cfgs {
		t.Run(name, func(t *testing.T) {
			for _, cmd := range refused {
				if reason := decide(t, cfg, cmd); reason == "" {
					t.Errorf("ALLOWED: %s", cmd)
				}
			}
			for _, cmd := range []string{"curl -s 'file://" + work + "/x.db'", "curl -s 'file://" + work + "/x%2Edb'"} {
				if reason := decide(t, cfg, cmd); reason != "" {
					t.Errorf("REFUSED: %s: %s", cmd, reason)
				}
			}
		})
	}
	// A sqlite URI keeps its query in a job without a read allowlist; with one it is refused. The read
	// check is called directly: Decide refuses the `?` anyway as a glob in an argument of a writer.
	sqlite := "sqlite3 'file:x.db?mode=ro' 'select 1'"
	for _, name := range []string{"state", "no_read"} {
		if reason := checkStateRead(parseAll(sqlite), sqlite, cfgs[name]); reason != "" {
			t.Errorf("%s: REFUSED: %s: %s", name, sqlite, reason)
		}
	}
	if reason := checkStateRead(parseAll(sqlite), sqlite, cfgs["allow"]); reason == "" {
		t.Errorf("allow: ALLOWED: %s", sqlite)
	}
}
