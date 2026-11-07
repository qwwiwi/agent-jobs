package jobs

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// These tests use only git and the standard library, so they can also run offline:
// go test branch_refs.go branch_refs_test.go
func branchRepo(t *testing.T) (branchGit, string, string) {
	t.Helper()
	repo := t.TempDir()
	run := branchGit(func(args ...string) (string, error) {
		t.Helper()
		cmd := exec.Command("git", append([]string{"-C", repo}, args...)...)
		for _, kv := range os.Environ() {
			if !strings.HasPrefix(kv, "GIT_") {
				cmd.Env = append(cmd.Env, kv)
			}
		}
		cmd.Env = append(cmd.Env, "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL="+os.DevNull)
		out, err := cmd.CombinedOutput()
		if err != nil {
			return "", fmt.Errorf("git %v: %w: %s", args, err, out)
		}
		return strings.TrimSpace(string(out)), nil
	})
	branchMust(t, run, "init", "-q", "-b", "main")
	branchMust(t, run, "config", "user.name", "Test")
	branchMust(t, run, "config", "user.email", "test@example.com")
	branchMust(t, run, "commit", "-q", "--allow-empty", "-m", "base")
	base := branchMust(t, run, "rev-parse", "HEAD")
	branchMust(t, run, "checkout", "-q", "--detach")
	branchMust(t, run, "commit", "-q", "--allow-empty", "-m", "job")
	return run, base, branchMust(t, run, "rev-parse", "HEAD")
}

func branchMust(t *testing.T, run branchGit, args ...string) string {
	t.Helper()
	out, err := run(args...)
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func TestPublishBranchRefCreateAndFastForward(t *testing.T) {
	run, base, head := branchRepo(t)
	if ff, err := publishBranchRef(run, "job/123", head); err != nil || ff {
		t.Fatalf("create: ff=%v err=%v", ff, err)
	}
	branchMust(t, run, "branch", "feature", base)
	if ff, err := publishBranchRef(run, "feature", head); err != nil || !ff {
		t.Fatalf("fast-forward: ff=%v err=%v", ff, err)
	}
	for _, name := range []string{"job/123", "feature"} {
		if got := branchMust(t, run, "rev-parse", name); got != head {
			t.Fatalf("%s: %s != %s", name, got, head)
		}
	}
}

func TestPublishBranchRefRejectsNonFastForward(t *testing.T) {
	run, base, head := branchRepo(t)
	branchMust(t, run, "checkout", "-q", "--detach", base)
	branchMust(t, run, "commit", "-q", "--allow-empty", "-m", "other")
	other := branchMust(t, run, "rev-parse", "HEAD")
	branchMust(t, run, "branch", "feature", other)
	if _, err := publishBranchRef(run, "feature", head); err == nil || !strings.Contains(err.Error(), "not a fast-forward") {
		t.Fatalf("expected non-ff refusal, got %v", err)
	}
	if got := branchMust(t, run, "rev-parse", "feature"); got != other {
		t.Fatalf("divergent branch moved: %s", got)
	}
}

func TestValidateResultBranch(t *testing.T) {
	run, base, _ := branchRepo(t)
	branchMust(t, run, "update-ref", "refs/remotes/origin/trunk", base)
	branchMust(t, run, "symbolic-ref", "refs/remotes/origin/HEAD", "refs/remotes/origin/trunk")
	for _, name := range []string{"main", "Main", "master", "trunk", "TrUnK", "release", "release/1", "Release/1", "release-next", "bad name", "-flag", "a..b", "@{-1}", "refs/heads/main"} {
		t.Run(name, func(t *testing.T) {
			if err := validateResultBranch(run, name); err == nil {
				t.Fatalf("accepted %q", name)
			}
		})
	}
	if err := validateResultBranch(run, "feature/result"); err != nil {
		t.Fatal(err)
	}
	branchMust(t, run, "config", "init.defaultBranch", "integration")
	if err := validateResultBranch(run, "integration"); err == nil {
		t.Fatal("accepted configured default branch")
	}
}

func TestPublishBranchRefRejectsEmptyHeadAndSymbolicRef(t *testing.T) {
	run, base, head := branchRepo(t)
	branchMust(t, run, "symbolic-ref", "refs/heads/alias", "refs/heads/main")
	for _, tc := range []struct{ name, head string }{{"job/empty", ""}, {"alias", head}, {"job/bad", "HEAD"}} {
		if _, err := publishBranchRef(run, tc.name, tc.head); err == nil {
			t.Fatalf("accepted %+v", tc)
		}
	}
	if got := branchMust(t, run, "rev-parse", "main"); got != base {
		t.Fatal("symbolic ref changed main")
	}
}

func TestPublishBranchRefCAS(t *testing.T) {
	run, base, head := branchRepo(t)
	branchMust(t, run, "branch", "feature", base)
	branchMust(t, run, "commit", "-q", "--allow-empty", "-m", "concurrent")
	other := branchMust(t, run, "rev-parse", "HEAD")
	racing := branchGit(func(args ...string) (string, error) {
		if len(args) > 1 && args[0] == "update-ref" {
			branchMust(t, run, "update-ref", "refs/heads/feature", other, base)
		}
		return run(args...)
	})
	if _, err := publishBranchRef(racing, "feature", head); err == nil {
		t.Fatal("concurrent ref move was overwritten")
	}
	if got := branchMust(t, run, "rev-parse", "feature"); got != other {
		t.Fatal("lost concurrent ref update")
	}
}

func TestPublishBranchRefCheckedOut(t *testing.T) {
	run, base, head := branchRepo(t)
	branchMust(t, run, "branch", "feature", base)
	work := filepath.Join(t.TempDir(), "linked")
	branchMust(t, run, "worktree", "add", "-q", work, "feature")
	if _, err := publishBranchRef(run, "feature", head); err == nil || !strings.Contains(err.Error(), "checked out") {
		t.Fatalf("expected checked-out refusal, got %v", err)
	}
}

func TestValidateResultBranchLocalDefault(t *testing.T) {
	run, _, _ := branchRepo(t)
	if err := validateResultBranch(run, "feature"); err == nil {
		t.Fatal("accepted explicit target without enough metadata to protect the default")
	}
	branchMust(t, run, "checkout", "-q", "-b", "trunk")
	if err := validateResultBranch(run, "trunk"); err == nil {
		t.Fatal("accepted possible local default without remote metadata")
	}
	if err := validateResultBranch(run, "feature"); err != nil {
		t.Fatalf("refused feature with a known current branch: %v", err)
	}
}

func TestPublishBranchRefRetry(t *testing.T) {
	run, base, head := branchRepo(t)
	branchMust(t, run, "branch", "feature", base)
	for range 2 {
		if ff, err := publishBranchRef(run, "feature", head); err != nil || !ff {
			t.Fatalf("idempotent fast-forward: ff=%v err=%v", ff, err)
		}
	}
}
