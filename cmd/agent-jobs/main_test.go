package main

import (
	"os/exec"
	"path/filepath"
	"testing"
)

// The version is "dev" by default and is set at build time via -ldflags "-X main.version=…"
// (scripts/install-linux.sh passes `git describe --tags --always --dirty`).
func TestVersion(t *testing.T) {
	dir := t.TempDir()
	cases := []struct {
		name    string
		ldflags string
		want    string
	}{
		{"default", "", "agent-jobs dev\n"},
		{"ldflags", "-X main.version=v0.1.0-3-gabc1234-dirty", "agent-jobs v0.1.0-3-gabc1234-dirty\n"},
	}
	for _, c := range cases {
		bin := filepath.Join(dir, "agent-jobs-"+c.name)
		args := []string{"build", "-o", bin}
		if c.ldflags != "" {
			args = append(args, "-ldflags", c.ldflags)
		}
		if out, err := exec.Command("go", append(args, ".")...).CombinedOutput(); err != nil {
			t.Fatalf("%s: go build: %v\n%s", c.name, err, out)
		}
		for _, arg := range []string{"version", "--version"} {
			out, err := exec.Command(bin, arg).Output()
			if err != nil {
				t.Fatalf("%s %s: %v", c.name, arg, err)
			}
			if string(out) != c.want {
				t.Errorf("%s %s: got %q, want %q", c.name, arg, out, c.want)
			}
		}
	}
}

// run-wrapper accepts only known options with values before "--"; unknown flags and invalid
// the removed acceptance authorization flag fail before the wrapper touches the job dir.
func TestRunWrapperUsage(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "missing")
	for _, args := range [][]string{
		{dir, "--token", "t", "--bogus", "x", "--", "prog"},
		{dir, "--token", "t", "--acceptance-allowed", "invalid", "--", "prog"},
		{dir, "--token", "t", "--acceptance-allowed", "true", "--", "prog"},
		{dir, "--token", "t", "--prompt", "p.md", "prog", "x"},
		{dir, "--token", "t", "--prompt", "--", "prog"},
		{dir, "--token"},
	} {
		if code := runWrapper(args); code != 2 {
			t.Errorf("%q: exit %d, want 2", args, code)
		}
	}
}

// Names from a job's stream are printed as they are, or quoted when they carry control characters.
func TestShownQuotesControlChars(t *testing.T) {
	cases := [][2]string{
		{"deploy", "deploy"},
		{"сборка", "сборка"},
		{"x\x1b]0;pwned\x07", `"x\x1b]0;pwned\a"`},
		{"a\nb", `"a\nb"`},
	}
	for _, c := range cases {
		in, want := c[0], c[1]
		if got := shown(in); got != want {
			t.Errorf("shown(%q) = %s, want %s", in, got, want)
		}
	}
}
