package guard

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// A URL reader (curl, urllib) cuts the path of a file URL at the first raw `?` or `#`, so outside a
// read allowlist the guard checks the path before it: a symlink to a protected file is refused with a
// query or a fragment after it. An encoded `%23`/`%3F` is a filename character after the decode and
// cuts nothing.
func TestFileURLCutAtQueryAndFragment(t *testing.T) {
	root := t.TempDir()
	work := filepath.Join(root, "work")
	state := filepath.Join(root, "state")
	other := filepath.Join(state, "jobs", "other")
	outside := filepath.Join(root, "outside")
	for _, dir := range []string{work, other, filepath.Join(state, "jobs", "own"), outside} {
		if err := os.MkdirAll(dir, 0700); err != nil {
			t.Fatal(err)
		}
	}
	for _, p := range []string{filepath.Join(other, "secret.txt"), filepath.Join(outside, "secret.txt"), filepath.Join(work, "ok.txt"), filepath.Join(work, "ok#x")} {
		if err := os.WriteFile(p, []byte("FIXTURE"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	cfgs := map[string]struct {
		cfg    Config
		target string
	}{
		"state":   {Config{Dir: work, StateDir: state, OwnDir: filepath.Join(state, "jobs", "own")}, filepath.Join(other, "secret.txt")},
		"no_read": {Config{Dir: work, NoRead: []string{outside}}, filepath.Join(outside, "secret.txt")},
	}
	for name, tc := range cfgs {
		t.Run(name, func(t *testing.T) {
			dir := filepath.Join(work, name)
			if err := os.MkdirAll(dir, 0700); err != nil {
				t.Fatal(err)
			}
			// `alias` reaches the target; `link#x` and `py?q` do too, while no `link` or `py` exists
			// (urllib opens `py?q` as is: it cuts only at `#`).
			for _, l := range []string{"alias", "link#x", "py?q"} {
				if err := os.Symlink(tc.target, filepath.Join(dir, l)); err != nil {
					t.Fatal(err)
				}
			}
			decide := func(cmd string) string {
				t.Helper()
				b, err := json.Marshal(map[string]any{"tool_name": "Bash", "cwd": work, "tool_input": map[string]any{"command": cmd}})
				if err != nil {
					t.Fatal(err)
				}
				reason, err := Decide(strings.NewReader(string(b)), tc.cfg)
				if err != nil {
					t.Fatal(err)
				}
				return reason
			}
			for _, u := range []string{
				"file://" + dir + "/alias#fragment", "file://" + dir + "/alias?q", "file://" + dir + "/alias?q#f",
				"file://localhost" + dir + "/alias#fragment", "file:" + dir + "/alias?q", "FILE://" + dir + "/alias#",
				"file://" + dir + "/link%23x", "file://" + dir + "/link%23x#fragment", "file://" + dir + "/py?q#f",
			} {
				for _, cmd := range fileURLCmds(u) {
					if r := Check(cmd, tc.cfg); r == "" {
						t.Errorf("Check allowed: %s", cmd)
					}
				}
				if r := decide("curl -s '" + u + "'"); r == "" {
					t.Errorf("Decide allowed: curl -s '%s'", u)
				}
			}
			for _, u := range []string{"file://" + work + "/ok.txt#fragment", "file://" + work + "/ok%23x"} {
				cmd := "curl -s '" + u + "'"
				if r := Check(cmd, tc.cfg); r != "" {
					t.Errorf("Check refused %s: %s", cmd, r)
				}
				if r := decide(cmd); r != "" {
					t.Errorf("Decide refused %s: %s", cmd, r)
				}
			}
			// A query: the read check alone (Check refuses the `?` as a glob in an argument of a writer).
			q := "curl -s 'file://" + work + "/ok.txt?q'"
			if r := checkStateRead(parseAll(q), q, tc.cfg); r != "" {
				t.Errorf("REFUSED: %s: %s", q, r)
			}
		})
	}
}

func TestFileURLPaths(t *testing.T) {
	for _, tc := range []struct {
		u    string
		want []string
	}{
		{"///a/b", []string{"/a/b"}},
		{"///a/b#f", []string{"/a/b"}},
		{"///a/b?q", []string{"/a/b", "/a/b?q"}},
		{"///a/b?q#f", []string{"/a/b", "/a/b?q"}},
		{"///a/b#f?q", []string{"/a/b"}},
		{"//localhost/a%23b?q", []string{"/a#b", "/a#b?q"}},
		{"/a%3Fb#f", []string{"/a?b"}},
		{"//host", nil},
		{"//host?x/a", []string{"/a"}},
	} {
		if got := fileURLPaths(tc.u); strings.Join(got, "|") != strings.Join(tc.want, "|") {
			t.Errorf("fileURLPaths(%q) = %q, want %q", tc.u, got, tc.want)
		}
	}
}
