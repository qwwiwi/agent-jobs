package guard

import (
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

// REDPROOF 4: resolving the symlinks of a path costs one step per component, with one shared budget:
// 40 dangling links with long `..` targets in a deep directory used to take minutes (each `..`
// resolved its prefix again from the root); now the resolution stops at the budget and refuses.
func TestLinkWalkBudget(t *testing.T) {
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	deep := filepath.Join(root, strings.Repeat("a/", 300))
	if err := os.MkdirAll(deep, 0o700); err != nil {
		t.Fatal(err)
	}
	for i := range 40 {
		mustSymlink(t, strings.Repeat("a/../", 800)+"l"+strconv.Itoa(i+1), filepath.Join(deep, "l"+strconv.Itoa(i)))
	}
	cfg := Config{Dir: deep, Protected: []string{filepath.Join(root, "p")}}
	done := make(chan [2]string, 1)
	start := time.Now()
	go func() {
		done <- [2]string{CheckFileWrite("Write", "l0", cfg), Check("echo x > l0", cfg)}
	}()
	select {
	case r := <-done:
		t.Logf("checked in %v", time.Since(start))
		for i, what := range []string{"Write", "Bash"} {
			if r[i] == "" {
				t.Errorf("%s through 40 links past the resolution budget allowed", what)
			}
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the link walk did not finish within 5s")
	}
}

// The kernel follows 40 symlinks in one lookup and fails the 41st (ELOOP): chains of 39 and 40
// dangling links (each target with a `..`) ending in .git/hooks/pre-commit are refused, and the
// kernel indeed creates the file through them; through 41 it creates nothing.
func TestLinkChainKernelBoundary(t *testing.T) {
	for _, n := range []int{39, 40, 41} {
		repo, err := filepath.EvalSymlinks(t.TempDir())
		if err != nil {
			t.Fatal(err)
		}
		for _, d := range []string{".git/hooks", "d"} {
			if err := os.MkdirAll(filepath.Join(repo, d), 0o700); err != nil {
				t.Fatal(err)
			}
		}
		link := func(i int) string { return "l" + strconv.Itoa(i) }
		for i := 1; i < n; i++ {
			mustSymlink(t, "d/../"+link(i+1), filepath.Join(repo, link(i)))
		}
		mustSymlink(t, "d/../.git/hooks/pre-commit", filepath.Join(repo, link(n)))
		r := CheckFileWrite("Write", link(1), Config{Dir: repo})
		werr := os.WriteFile(filepath.Join(repo, link(1)), []byte("x"), 0o600)
		_, serr := os.Stat(filepath.Join(repo, ".git", "hooks", "pre-commit"))
		switch {
		case n <= 40:
			if werr != nil || serr != nil {
				t.Fatalf("%d links: the kernel did not create the hook: %v %v", n, werr, serr)
			}
			if r == "" {
				t.Errorf("%d links: Write allowed into .git/hooks", n)
			}
		default:
			if !errors.Is(werr, syscall.ELOOP) || serr == nil {
				t.Fatalf("%d links: want ELOOP and no hook, got %v %v", n, werr, serr)
			}
		}
	}
}

// REDPROOF 5: a protected path that does not exist yet is resolved like a write target: through a
// dangling link (config.json -> /outside/new.json) and behind a symlinked parent (a cards dir not
// created yet), both spellings are protected.
func TestDanglingProtectedPaths(t *testing.T) {
	_, _, _, cfg := protectedSetup(t)
	ext, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	conf := filepath.Join(ext, "config.json")
	mustSymlink(t, filepath.Join(ext, "outside", "new.json"), conf)
	if err := os.MkdirAll(filepath.Join(ext, "outside"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(ext, "real"), 0o700); err != nil {
		t.Fatal(err)
	}
	mustSymlink(t, filepath.Join(ext, "real"), filepath.Join(ext, "link"))
	cfg.Protected = append(cfg.Protected, conf, filepath.Join(ext, "link", "cards"))
	for _, p := range []string{conf, filepath.Join(ext, "outside", "new.json"), filepath.Join(ext, "real", "cards", "x.md"),
		filepath.Join(ext, "link", "cards", "x.md")} {
		if r := CheckFileWrite("Write", p, cfg); r == "" {
			t.Errorf("Write %s allowed", p)
		}
		if r := Check("echo x > '"+p+"'", cfg); r == "" {
			t.Errorf("echo x > %s allowed", p)
		}
	}
	if r := CheckFileWrite("Write", filepath.Join(ext, "outside", "other.json"), cfg); r != "" {
		t.Errorf("Write next to the target refused: %s", r)
	}
}
