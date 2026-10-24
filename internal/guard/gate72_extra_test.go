package guard

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestGate72BashNoReadOverflow: a NoRead target or root past the symlink step budget is refused in
// Bash too, for a read and for a write.
func TestGate72BashNoReadOverflow(t *testing.T) {
	root := t.TempDir()
	for _, d := range []string{"d", "real", "work"} {
		if err := os.Mkdir(filepath.Join(root, d), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	lock := filepath.Join(root, "real", "a.lock")
	if err := os.WriteFile(lock, []byte("lock\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	pad := strings.Repeat("d/../", 600)
	for i := 1; i <= 4; i++ {
		target := "real"
		if i < 4 {
			target = fmt.Sprintf("l%d", i+1)
		}
		if err := os.Symlink(pad+target, filepath.Join(root, fmt.Sprintf("l%d", i))); err != nil {
			t.Fatal(err)
		}
	}
	alias := filepath.Join(root, "l1")
	work := filepath.Join(root, "work")
	for _, c := range []struct {
		noRead, cmd string
	}{
		{filepath.Join(root, "real"), "cat " + filepath.Join(alias, "a.lock")},
		{alias, "cat " + lock},
		{alias, "touch " + lock},
	} {
		cfg := Config{Dir: work, NoRead: []string{c.noRead}}
		if reason := gate72Decision(t, cfg, "Bash", c.cmd); reason == "" {
			t.Errorf("NoRead %s: %q allowed", c.noRead, c.cmd)
		}
	}
}

// TestGate72FdReaders: fd without an exec option stays a reader of the parent of a protected path;
// a value option holding an x (`-tx`, `-e x`) is no exec, any other x or X spelling is.
func TestGate72FdReaders(t *testing.T) {
	cases := map[string]bool{
		"-x": true, "-X": true, "-xtouch": true, "-Hxtouch": true, "-HIX": true, "--exec=touch": true,
		"--exec-batch": true, "--exe": true, "-tx": false, "-Htx": false, "-etxt": false, "-H": false,
		"--extension": false, "x": false, "--": false,
	}
	for a, want := range cases {
		if got := fdActs([]string{"victim", a}); got != want {
			t.Errorf("fdActs(%q) = %v, want %v", a, got, want)
		}
	}
	root := t.TempDir()
	skills, work := filepath.Join(root, "skills"), filepath.Join(root, "work")
	for _, d := range []string{skills, work} {
		if err := os.Mkdir(d, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	cfg := Config{Dir: work, Protected: []string{skills}}
	for _, cmd := range []string{"fd victim " + root, "fd -tx victim " + root, "fdfind -H victim " + root} {
		if reason := gate72Decision(t, cfg, "Bash", cmd); reason != "" {
			t.Errorf("%q refused: %s", cmd, reason)
		}
	}
	for _, cmd := range []string{"fdfind victim " + root + " -Xtouch", "fd -HIxtouch victim " + root, "fd --exe touch victim " + root} {
		if reason := gate72Decision(t, cfg, "Bash", cmd); reason == "" {
			t.Errorf("%q allowed", cmd)
		}
	}
}
