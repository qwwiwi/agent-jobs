package jobs

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// restackRepo is a real repository in t.TempDir() with the stack
//
//	main:  base
//	P:     base - p1 - p2
//	C:     base - p1 - p2 - c1 - c2
//
// and a git helper that runs in it. The identity is set in the repository's config only.
func restackRepo(t *testing.T) (repo string, g func(args ...string) string) {
	t.Helper()
	repo = t.TempDir()
	if resolved, err := filepath.EvalSymlinks(repo); err == nil {
		repo = resolved
	}
	g = func(args ...string) string {
		t.Helper()
		cmd := exec.Command("git", append([]string{"-C", repo}, args...)...)
		for _, kv := range os.Environ() {
			if k, _, _ := strings.Cut(kv, "="); !strings.HasPrefix(k, "GIT_") && !strings.HasPrefix(k, "TELEGRAM_") {
				cmd.Env = append(cmd.Env, kv)
			}
		}
		cmd.Env = append(cmd.Env, "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL="+os.DevNull)
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %v: %s", args, err, out)
		}
		return strings.TrimSpace(string(out))
	}
	commit := func(msg string, files map[string]string) {
		t.Helper()
		for name, body := range files {
			if err := os.WriteFile(filepath.Join(repo, name), []byte(body), 0o644); err != nil {
				t.Fatal(err)
			}
		}
		g("add", "-A")
		g("commit", "-q", "-m", msg)
	}
	g("init", "-q", "-b", "main")
	g("config", "user.name", "Test")
	g("config", "user.email", "test@example.com")
	commit("base", map[string]string{"a.txt": "one\ntwo\nthree\n"})
	g("checkout", "-q", "-b", "P")
	commit("p1", map[string]string{"p.txt": "p1\np2\np3\n"})
	commit("p2", map[string]string{"q.txt": "q\n"})
	g("checkout", "-q", "-b", "C")
	commit("c1", map[string]string{"c.txt": "c\n"})
	commit("c2", map[string]string{"d.txt": "d\n"})
	g("checkout", "-q", "main")
	return repo, g
}

// squashP squash-merges P into main as one commit S, the way a forge does.
func squashP(t *testing.T, g func(args ...string) string) string {
	t.Helper()
	g("merge", "--squash", "P")
	g("commit", "-q", "-m", "S: squash of P")
	return g("rev-parse", "HEAD")
}

func restackSubjects(g func(args ...string) string, rng string) []string {
	out := g("log", "--format=%s", "--reverse", rng)
	if out == "" {
		return nil
	}
	return strings.Split(out, "\n")
}

// leftovers fails when a restack left a temporary worktree behind: no extra entry in the repository's
// worktree list and nothing in the private TMPDIR.
func restackNoLeftovers(t *testing.T, g func(args ...string) string, tmp string) {
	t.Helper()
	if n := strings.Count(g("worktree", "list", "--porcelain"), "worktree "); n != 1 {
		t.Fatalf("temporary worktree left registered: %s", g("worktree", "list"))
	}
	if entries, err := os.ReadDir(tmp); err != nil || len(entries) != 0 {
		t.Fatalf("temporary files left in %s: %v %v", tmp, entries, err)
	}
}

func TestRestackAfterSquash(t *testing.T) {
	repo, g := restackRepo(t)
	s := squashP(t, g)
	// Main moved on after the squash; the restacked branch starts from the current tip.
	if err := os.WriteFile(filepath.Join(repo, "a.txt"), []byte("one\ntwo\nthree\nfour\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	g("commit", "-q", "-am", "m1")
	tip := g("rev-parse", "main")
	childHead, parentHead := g("rev-parse", "C"), g("rev-parse", "P")
	tmp := t.TempDir()
	t.Setenv("TMPDIR", tmp)

	res, err := Restack(repo, "C", "P", "main", "")
	if err != nil {
		t.Fatal(err)
	}
	if res.Branch != "C-restacked" || res.Commits != 2 || res.Head != g("rev-parse", "C-restacked") {
		t.Fatalf("result: %+v", res)
	}
	if got := restackSubjects(g, "main..C-restacked"); !slices.Equal(got, []string{"c1", "c2"}) {
		t.Fatalf("commits on top of main: %v", got)
	}
	if g("merge-base", "main", "C-restacked") != tip {
		t.Fatal("restacked branch does not start at the tip of main")
	}
	if log := g("log", "--format=%s", "C-restacked"); strings.Contains(log, "p1") || strings.Contains(log, "p2") || !strings.Contains(log, "S: squash of P") {
		t.Fatalf("P's commits must be gone, the squash kept:\n%s", log)
	}
	for _, f := range []string{"a.txt", "p.txt", "q.txt", "c.txt", "d.txt"} {
		g("cat-file", "-e", "C-restacked:"+f)
	}
	if g("rev-parse", "C") != childHead || g("rev-parse", "P") != parentHead || g("rev-parse", "main") != tip || g("rev-parse", s) != s {
		t.Fatal("child, parent or onto moved")
	}
	if g("rev-parse", "--abbrev-ref", "HEAD") != "main" || g("status", "--porcelain") != "" {
		t.Fatal("the repository's own checkout was touched")
	}
	restackNoLeftovers(t, g, tmp)
}

// Main edited the squashed lines after the squash, so merging P into main conflicts and containment cannot
// be proven against the current main. A patch-id match with the squash commit is NOT accepted instead
// (it ignores whitespace); the refusal is a deliberate false negative and creates nothing.
func TestRestackRefusesWhenSquashedLinesWereEditedAfterwards(t *testing.T) {
	repo, g := restackRepo(t)
	squashP(t, g)
	if err := os.WriteFile(filepath.Join(repo, "p.txt"), []byte("p1\nEDITED\np3\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	g("commit", "-q", "-am", "main edits p.txt")
	// Precondition: the merge of main and P conflicts, so the tree-equality proof cannot hold.
	if err := exec.Command("git", "-C", repo, "merge-tree", "--write-tree", "main", "P").Run(); err == nil {
		t.Fatal("test setup: merge of main and P is clean")
	}
	before := g("for-each-ref")
	if _, err := Restack(repo, "C", "P", "main", ""); !errors.Is(err, ErrRestackNotMerged) {
		t.Fatalf("want ErrRestackNotMerged, got %v", err)
	}
	if g("for-each-ref") != before {
		t.Fatal("a ref appeared")
	}
}

// A child commit whose change main already has (the same file added after the squash) becomes empty:
// it is skipped and reported, the other commits are carried.
func TestRestackSkipsCommitsAlreadyInOnto(t *testing.T) {
	repo, g := restackRepo(t)
	squashP(t, g)
	if err := os.WriteFile(filepath.Join(repo, "c.txt"), []byte("c\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	g("add", "-A")
	g("commit", "-q", "-m", "main adds c.txt too")
	res, err := Restack(repo, "C", "P", "main", "")
	if err != nil {
		t.Fatal(err)
	}
	if got := restackSubjects(g, "main..C-restacked"); res.Commits != 1 || len(res.Skipped) != 1 || !slices.Equal(got, []string{"c2"}) {
		t.Fatalf("%+v %v", res, got)
	}
}

func TestRestackRefusesWhenParentNotMerged(t *testing.T) {
	repo, g := restackRepo(t)
	g("commit", "-q", "--allow-empty", "-m", "unrelated work on main")
	before := g("for-each-ref")
	tmp := t.TempDir()
	t.Setenv("TMPDIR", tmp)

	_, err := Restack(repo, "C", "P", "main", "")
	if !errors.Is(err, ErrRestackNotMerged) {
		t.Fatalf("want ErrRestackNotMerged, got %v", err)
	}
	if !strings.Contains(err.Error(), "P") {
		t.Fatalf("the reason does not name the parent: %v", err)
	}
	if g("for-each-ref") != before {
		t.Fatalf("a ref appeared or moved:\n%s\n--\n%s", before, g("for-each-ref"))
	}
	restackNoLeftovers(t, g, tmp)
}

func TestRestackConflictNamesFileAndLeavesNothing(t *testing.T) {
	repo, g := restackRepo(t)
	// c0 on C rewrites a.txt line one; main rewrites it differently after the squash.
	g("checkout", "-q", "C")
	if err := os.WriteFile(filepath.Join(repo, "a.txt"), []byte("from-c\ntwo\nthree\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	g("commit", "-q", "-am", "c3 edits a.txt")
	g("checkout", "-q", "main")
	squashP(t, g)
	if err := os.WriteFile(filepath.Join(repo, "a.txt"), []byte("from-main\ntwo\nthree\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	g("commit", "-q", "-am", "main edits a.txt")
	before := g("for-each-ref")
	tmp := t.TempDir()
	t.Setenv("TMPDIR", tmp)

	_, err := Restack(repo, "C", "P", "main", "")
	var conflict *RestackConflictError
	if !errors.As(err, &conflict) {
		t.Fatalf("want RestackConflictError, got %v", err)
	}
	if !slices.Equal(conflict.Files, []string{"a.txt"}) || !strings.Contains(err.Error(), "a.txt") || !strings.Contains(err.Error(), "c3 edits a.txt") {
		t.Fatalf("conflict does not name the file and commit: %+v / %v", conflict, err)
	}
	if g("for-each-ref") != before {
		t.Fatalf("a ref was left behind:\n%s", g("for-each-ref"))
	}
	restackNoLeftovers(t, g, tmp)
}

func TestRestackRefusesExistingName(t *testing.T) {
	repo, g := restackRepo(t)
	squashP(t, g)
	g("branch", "C-restacked", "main~1")
	g("branch", "taken", "main~1")
	taken := g("rev-parse", "C-restacked")
	for _, name := range []string{"", "taken"} {
		_, err := Restack(repo, "C", "P", "main", name)
		if !errors.Is(err, errBranchExists) {
			t.Fatalf("name %q: want errBranchExists, got %v", name, err)
		}
	}
	if g("rev-parse", "C-restacked") != taken || g("rev-parse", "taken") != taken {
		t.Fatal("an existing branch moved")
	}
	// A free explicit name works, a protected or generated one does not.
	if res, err := Restack(repo, "C", "P", "main", "feature/c-on-main"); err != nil || res.Branch != "feature/c-on-main" {
		t.Fatalf("%+v %v", res, err)
	}
	for _, bad := range []string{"main", "job/x", "bad name"} {
		if _, err := Restack(repo, "C", "P", "main", bad); err == nil {
			t.Fatalf("name %q accepted", bad)
		}
	}
}

// Without P in C's history there are no "own" commits to tell apart: refuse instead of guessing.
func TestRestackRefusesUnrelatedParentAndMerges(t *testing.T) {
	repo, g := restackRepo(t)
	squashP(t, g)
	g("branch", "other", "main~1")
	g("checkout", "-q", "other")
	g("commit", "-q", "--allow-empty", "-m", "other work")
	g("checkout", "-q", "main")
	if _, err := Restack(repo, "C", "other", "main", ""); err == nil || !strings.Contains(err.Error(), "not an ancestor") {
		t.Fatalf("parent that is not an ancestor of the child: %v", err)
	}
	g("checkout", "-q", "C")
	g("merge", "-q", "--no-ff", "-m", "merge other", "other")
	g("checkout", "-q", "main")
	if _, err := Restack(repo, "C", "P", "main", ""); err == nil || !strings.Contains(err.Error(), "merge commit") {
		t.Fatalf("a merge commit among the child's commits: %v", err)
	}
	if _, err := Restack(repo, "P", "P", "main", ""); err == nil {
		t.Fatal("child == parent accepted")
	}
	if out := g("branch", "--list", "*restacked*"); out != "" {
		t.Fatalf("branch created: %s", out)
	}
}
