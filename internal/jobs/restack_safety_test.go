package jobs

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// Stable patch-ids ignore whitespace, so "a b" and "ab" look like the same change. The parent's value
// is NOT in onto then: restack must refuse instead of dropping the parent's change.
func TestRestackRefusesWhitespaceOnlyPatchIDMatch(t *testing.T) {
	repo := t.TempDir()
	g := func(args ...string) string {
		t.Helper()
		cmd := exec.Command("git", append([]string{"-C", repo, "-c", "user.name=T", "-c", "user.email=t@example.com"}, args...)...)
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
	write := func(name, body string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(repo, name), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	g("init", "-q", "-b", "main")
	write("conf.go", "package x\n\nvar label = \"old\"\n")
	g("add", "-A")
	g("commit", "-q", "-m", "base")
	g("checkout", "-q", "-b", "P")
	write("conf.go", "package x\n\nvar label = \"a b\"\n")
	g("commit", "-q", "-am", "p: label a b")
	g("checkout", "-q", "-b", "C")
	write("c.txt", "c\n")
	g("add", "-A")
	g("commit", "-q", "-m", "c1")
	g("checkout", "-q", "main")
	write("conf.go", "package x\n\nvar label = \"ab\"\n")
	g("commit", "-q", "-am", "main: label ab")
	// Precondition: the two changes are indistinguishable for the stable patch-id.
	pid := func(rev string) string {
		diff := exec.Command("git", "-C", repo, "diff", "main~1", rev, "--", "conf.go")
		out, err := diff.Output()
		if err != nil {
			t.Fatal(err)
		}
		cmd := exec.Command("git", "patch-id", "--stable")
		cmd.Stdin = strings.NewReader(string(out))
		res, err := cmd.Output()
		if err != nil {
			t.Fatal(err)
		}
		return strings.Fields(string(res))[0]
	}
	if pid("P") != pid("main") {
		t.Fatal("test setup: patch-ids differ, the whitespace case is not exercised")
	}
	before := g("for-each-ref")

	_, err := Restack(repo, "C", "P", "main", "")
	if !errors.Is(err, ErrRestackNotMerged) {
		t.Fatalf("want ErrRestackNotMerged, got %v", err)
	}
	if g("for-each-ref") != before {
		t.Fatal("a ref appeared")
	}
}

// A merge driver or filter configured in the repository must not run during the replay.
func TestRestackRunsNoRepositoryConfiguredCommands(t *testing.T) {
	repo, g := restackRepo(t)
	// c3 edits a.txt on C; main edits it differently after the squash: both sides changed the file.
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
	if err := os.WriteFile(filepath.Join(repo, ".gitattributes"), []byte("a.txt merge=evil filter=evil\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	g("add", "-A")
	g("commit", "-q", "-m", "main edits a.txt and adds attributes")
	// The commands are configured only now, so that the setup itself does not run them.
	marks := t.TempDir()
	mergeMark, smudgeMark, cleanMark := filepath.Join(marks, "merge"), filepath.Join(marks, "smudge"), filepath.Join(marks, "clean")
	g("config", "merge.evil.driver", "touch "+mergeMark)
	g("config", "filter.evil.smudge", "touch "+smudgeMark+"; cat")
	g("config", "filter.evil.clean", "touch "+cleanMark+"; cat")
	before := g("for-each-ref")

	_, err := Restack(repo, "C", "P", "main", "")
	for _, m := range []string{mergeMark, smudgeMark, cleanMark} {
		if _, statErr := os.Stat(m); statErr == nil {
			t.Fatalf("a repository-configured command ran during restack: %s", filepath.Base(m))
		}
	}
	// With the driver ignored the built-in merge conflicts, as it must.
	var conflict *RestackConflictError
	if !errors.As(err, &conflict) {
		t.Fatalf("want RestackConflictError from the built-in merge, got %v", err)
	}
	if g("for-each-ref") != before {
		t.Fatal("a ref appeared")
	}
}

// A pick whose commit step fails after the changes were staged is not an empty pick: restack must
// refuse, not skip the commit and publish a branch without it.
func TestRestackRefusesPickThatFailsToCommit(t *testing.T) {
	repo, g := restackRepo(t)
	squashP(t, g)
	first := g("rev-parse", "C~1")
	restackPickConfig = func(c string) []string {
		if c == first {
			return []string{"commit.gpgsign=true", "gpg.program=/nonexistent/agent-jobs-gpg"}
		}
		return nil
	}
	t.Cleanup(func() { restackPickConfig = nil })
	before := g("for-each-ref")
	tmp := t.TempDir()
	t.Setenv("TMPDIR", tmp)

	res, err := Restack(repo, "C", "P", "main", "")
	if err == nil {
		t.Fatalf("a commit that failed to commit was skipped: %+v", res)
	}
	var conflict *RestackConflictError
	if errors.As(err, &conflict) {
		t.Fatalf("not a conflict: %v", err)
	}
	if g("for-each-ref") != before {
		t.Fatalf("a ref was left behind:\n%s", g("for-each-ref"))
	}
	restackNoLeftovers(t, g, tmp)
}

func TestRemoveAllWithinReportsFailure(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root ignores directory permissions")
	}
	ok := filepath.Join(t.TempDir(), "ok")
	if err := os.MkdirAll(filepath.Join(ok, "sub"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := removeAllWithin(ok, time.Minute); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(ok); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("directory still there: %v", err)
	}
	// A child of a read-only directory cannot be removed: the error must come back, not vanish.
	locked := filepath.Join(t.TempDir(), "locked")
	if err := os.MkdirAll(filepath.Join(locked, "sub"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(locked, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(locked, 0o700) })
	if err := removeAllWithin(locked, time.Minute); err == nil || !strings.Contains(err.Error(), "remove temporary directory") {
		t.Fatalf("want a cleanup error, got %v", err)
	}
}

func TestUnmergedPaths(t *testing.T) {
	out := "100644 aaa 1\ta.txt\x00100644 bbb 2\ta.txt\x00100644 ccc 3\ta.txt\x00100644 ddd 2\tdir/b c.txt\x00"
	got := unmergedPaths(out)
	if len(got) != 2 || got[0] != "a.txt" || got[1] != "dir/b c.txt" {
		t.Fatalf("%q", got)
	}
	if len(unmergedPaths("")) != 0 {
		t.Fatal("empty output must give no paths")
	}
}
