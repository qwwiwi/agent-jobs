package guard

import (
	"os"
	"path/filepath"
	"testing"
)

func mustSymlink(t *testing.T, target, link string) {
	t.Helper()
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
}

// REDPROOF 3: a dangling symlink whose target has a `..` segment is resolved the way the kernel
// does, `..` after the symlinks before it, not lexically: `cfg -> jump/../hooks/pre-commit` with
// `jump -> .git/objects` creates .git/hooks/pre-commit.
func TestDanglingLinkDotDotResolvedLikeKernel(t *testing.T) {
	repo := t.TempDir()
	for _, d := range []string{".git/objects", ".git/hooks", "sub", "real/inner"} {
		if err := os.MkdirAll(filepath.Join(repo, d), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	mustSymlink(t, filepath.Join(repo, ".git", "objects"), filepath.Join(repo, "jump"))
	mustSymlink(t, "jump/../hooks/pre-commit", filepath.Join(repo, "cfg"))
	// Relative `..` from a link whose own directory is reached through a symlink.
	mustSymlink(t, filepath.Join(repo, ".git", "objects"), filepath.Join(repo, "objs"))
	mustSymlink(t, "../hooks/post-checkout", filepath.Join(repo, ".git", "objects", "up"))
	// A chain: the second link's `..` is taken after the first.
	mustSymlink(t, "jump/../hooks", filepath.Join(repo, "hk"))
	mustSymlink(t, "hk/../hooks/../objects/../hooks/pre-push", filepath.Join(repo, "chain"))
	// Harmless: `..` over real directories.
	mustSymlink(t, "sub/../new.txt", filepath.Join(repo, "ok"))
	mustSymlink(t, "real/inner/../../fresh.txt", filepath.Join(repo, "ok2"))

	cfg := Config{Dir: repo}
	for _, p := range []string{"cfg", filepath.Join(repo, "cfg"), "objs/up", "chain"} {
		if r := CheckFileWrite("Write", p, cfg); r == "" {
			t.Errorf("Write %s allowed (reaches .git/hooks)", p)
		}
	}
	for _, p := range []string{"ok", "ok2"} {
		if r := CheckFileWrite("Write", p, cfg); r != "" {
			t.Errorf("Write %s refused: %s", p, r)
		}
	}

	// The same through a protected path: `logs/..` is not dropped lexically.
	state := t.TempDir()
	if err := os.MkdirAll(filepath.Join(state, "subagents", "x"), 0o700); err != nil {
		t.Fatal(err)
	}
	mustSymlink(t, filepath.Join(state, "subagents", "x"), filepath.Join(repo, "deep"))
	mustSymlink(t, "deep/../j.log", filepath.Join(repo, "plant"))
	pcfg := Config{Dir: repo, Protected: []string{filepath.Join(state, "subagents")}}
	if r := CheckFileWrite("Write", "plant", pcfg); r == "" {
		t.Error("Write plant allowed (reaches the subagent logs)")
	}
	for _, c := range []string{"echo x > plant", "tee plant", "cp /etc/hostname plant", "truncate -s0 plant"} {
		if r := Check(c, pcfg); r == "" {
			t.Errorf("%q allowed (reaches the subagent logs)", c)
		}
	}

	// The kernel agrees with the guard: writing through cfg creates .git/hooks/pre-commit.
	if err := os.WriteFile(filepath.Join(repo, "cfg"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(repo, ".git", "hooks", "pre-commit")); err != nil {
		t.Fatalf("kernel did not create .git/hooks/pre-commit: %v", err)
	}
}

// A loop of dangling links with `..` ends (the kernel's limit of 40 links per lookup).
func TestDanglingLinkDotDotLoopEnds(t *testing.T) {
	d := t.TempDir()
	mustSymlink(t, "b/../a/x", filepath.Join(d, "a"))
	mustSymlink(t, "a/../b/x", filepath.Join(d, "b"))
	_ = CheckFileWrite("Write", "a", Config{Dir: d})
}
