package guard

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// Fix loop 2, item 2: a protected path whose symlinks take more than the resolution budget to
// resolve is not taken lexically (the physical file would be unprotected): every file tool write and
// every Bash call is refused until the configuration names a path the guard can resolve.
func TestUnresolvableProtectedRootRefuses(t *testing.T) {
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	for _, d := range []string{"real", "d"} {
		if err := os.Mkdir(filepath.Join(root, d), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	rules := filepath.Join(root, "real", "owner-rules.md")
	if err := os.WriteFile(rules, []byte("rules\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	// Four valid links, each target 600 times `d/../`: 4 * 1201 components exceed the 4096 budget.
	pad := strings.Repeat("d/../", 600)
	for i := 1; i < 4; i++ {
		mustSymlink(t, pad+"l"+strconv.Itoa(i+1), filepath.Join(root, "l"+strconv.Itoa(i)))
	}
	mustSymlink(t, pad+"real", filepath.Join(root, "l4"))
	if got, err := filepath.EvalSymlinks(filepath.Join(root, "l1", "owner-rules.md")); err != nil || got != rules {
		t.Fatalf("the chain does not lead to the rules: %q %v", got, err)
	}
	work := filepath.Join(root, "work")
	if err := os.Mkdir(work, 0o700); err != nil {
		t.Fatal(err)
	}
	cfg := Config{Dir: work, Protected: []string{filepath.Join(root, "l1", "owner-rules.md")}}
	for _, p := range []string{rules, filepath.Join(work, "main.go")} {
		if r := CheckFileWrite("Write", p, cfg); r == "" {
			t.Errorf("Write %s allowed with an unresolvable protected path", p)
		}
	}
	refused(t, cfg, "echo x > '"+rules+"'", "ls")
	// A write target past the budget may lead into the cgroup file system as well.
	refused(t, Config{Dir: work}, "echo 1 > '"+filepath.Join(root, "l1", "cgroup.procs2")+"'")
	allowed(t, Config{Dir: work}, "echo 1 > '"+filepath.Join(root, "real", "x")+"'")
}
