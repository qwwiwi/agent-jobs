package jobs

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"sync"
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

func TestPublishBranchRefCreates(t *testing.T) {
	run, _, head := branchRepo(t)
	for _, name := range []string{"job/123", "feature"} {
		if created, err := publishBranchRef(run, name, head); err != nil || !created {
			t.Fatalf("create %s: created=%v err=%v", name, created, err)
		}
		if got := branchMust(t, run, "rev-parse", "refs/heads/"+name); got != head {
			t.Fatalf("%s: %s != %s", name, got, head)
		}
	}
}

// An existing branch is never moved: not when it is an ancestor of the head (a fast-forward would
// have been possible), not when it diverged, and not when it is a symbolic ref.
func TestPublishBranchRefNeverMovesExisting(t *testing.T) {
	run, base, head := branchRepo(t)
	branchMust(t, run, "branch", "ancestor", base)
	branchMust(t, run, "checkout", "-q", "--detach", base)
	branchMust(t, run, "commit", "-q", "--allow-empty", "-m", "other")
	other := branchMust(t, run, "rev-parse", "HEAD")
	branchMust(t, run, "branch", "diverged", other)
	branchMust(t, run, "branch", "trunk", base)
	branchMust(t, run, "symbolic-ref", "refs/heads/alias", "refs/heads/trunk")
	for name, want := range map[string]string{"ancestor": base, "diverged": other, "trunk": base, "alias": base} {
		if _, err := publishBranchRef(run, name, head); !errors.Is(err, errBranchExists) {
			t.Fatalf("%s: expected errBranchExists, got %v", name, err)
		}
		if got := branchMust(t, run, "rev-parse", "refs/heads/"+name); got != want {
			t.Fatalf("%s moved to %s, want %s", name, got, want)
		}
	}
	if got := branchMust(t, run, "symbolic-ref", "refs/heads/alias"); got != "refs/heads/trunk" {
		t.Fatalf("symbolic ref rewritten: %s", got)
	}
}

// A retry after a crash finds its own earlier publication: accepted without a write.
func TestPublishBranchRefRetry(t *testing.T) {
	run, _, head := branchRepo(t)
	if created, err := publishBranchRef(run, "feature", head); err != nil || !created {
		t.Fatalf("first: created=%v err=%v", created, err)
	}
	if created, err := publishBranchRef(run, "feature", head); err != nil || created {
		t.Fatalf("retry: created=%v err=%v", created, err)
	}
}

func TestPublishBranchRefRejectsBadHeadAndName(t *testing.T) {
	run, base, head := branchRepo(t)
	for _, tc := range []struct{ name, head string }{{"job/empty", ""}, {"job/bad", "HEAD"}, {"job/short", head[:10]}, {"bad name", head}, {"main", head}} {
		if _, err := publishBranchRef(run, tc.name, tc.head); err == nil {
			t.Fatalf("accepted %+v", tc)
		}
	}
	if got := branchMust(t, run, "rev-parse", "main"); got != base {
		t.Fatal("main changed")
	}
}

// Many writers creating the same name: git's expected-absent write lets exactly one through.
func TestPublishBranchRefConcurrentCreateOneWinner(t *testing.T) {
	run, base, _ := branchRepo(t)
	const writers = 8
	heads := make([]string, writers)
	for i := range heads {
		branchMust(t, run, "checkout", "-q", "--detach", base)
		branchMust(t, run, "commit", "-q", "--allow-empty", "-m", fmt.Sprintf("job %d", i))
		heads[i] = branchMust(t, run, "rev-parse", "HEAD")
	}
	var (
		wg      sync.WaitGroup
		start   = make(chan struct{})
		results = make([]error, writers)
	)
	for i := range heads {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			_, results[i] = publishBranchRef(run, "feature", heads[i])
		}()
	}
	close(start)
	wg.Wait()
	winner := ""
	for i, err := range results {
		switch {
		case err == nil:
			if winner != "" {
				t.Fatalf("two winners: %s and %s", winner, heads[i])
			}
			winner = heads[i]
		case !errors.Is(err, errBranchExists):
			t.Fatalf("writer %d: %v", i, err)
		}
	}
	if winner == "" {
		t.Fatal("no writer won")
	}
	if got := branchMust(t, run, "rev-parse", "refs/heads/feature"); got != winner {
		t.Fatalf("feature at %s, winner %s", got, winner)
	}
}

// Another job creates the requested name between the caller's decision and the write: the write is
// refused, the other job's branch stays, and the result goes to job/<id> with an explanation.
func TestPublishResultRefsLosesRaceFallsBack(t *testing.T) {
	run, base, head := branchRepo(t)
	rival := branchMust(t, run, "commit-tree", "-p", base, "-m", "rival", base+"^{tree}")
	raced := false
	racing := branchGit(func(args ...string) (string, error) {
		if !raced && len(args) > 1 && args[0] == "update-ref" && strings.HasSuffix(args[2], "/feature") {
			raced = true
			branchMust(t, run, "update-ref", "refs/heads/feature", rival, "")
		}
		return run(args...)
	})
	branch, note, err := publishResultRefs(racing, "abc", 1, "feature", head)
	if err != nil || branch != "job/abc" || !strings.Contains(note, "requested branch feature exists; not moved") {
		t.Fatalf("branch=%q note=%q err=%v", branch, note, err)
	}
	if got := branchMust(t, run, "rev-parse", "feature"); got != rival {
		t.Fatalf("the rival's branch moved to %s", got)
	}
	if got := branchMust(t, run, "rev-parse", "job/abc"); got != head {
		t.Fatalf("job/abc at %s, want %s", got, head)
	}
}

func TestPublishResultRefsOutcomes(t *testing.T) {
	run, base, head := branchRepo(t)
	branchMust(t, run, "branch", "taken", base)
	branchMust(t, run, "branch", "dir/x", base)
	for _, tc := range []struct{ id, requested, branch, note string }{
		{"a", "", "job/a", ""},
		// The job/ namespace is generated: a requested job/b is not honoured (an old row may carry one).
		{"b", "job/b", "job/b", "requested branch job/b not created: reserved"},
		{"f", "job", "job/f", "requested branch job not created: reserved"},
		{"c", "fresh", "fresh", ""},
		{"d", "taken", "job/d", "requested branch taken exists; not moved - published job/d; fast-forward it yourself with git merge --ff-only"},
		// A directory/file conflict is not "exists", but the result still lands on job/<id>.
		{"e", "dir", "job/e", "requested branch dir not created"},
	} {
		branch, note, err := publishResultRefs(run, tc.id, 1, tc.requested, head)
		if err != nil || branch != tc.branch || !strings.HasPrefix(note, tc.note) {
			t.Fatalf("%+v: branch=%q note=%q err=%v", tc, branch, note, err)
		}
	}
	if got := branchMust(t, run, "rev-parse", "taken"); got != base {
		t.Fatal("existing branch moved")
	}
	if got := branchMust(t, run, "rev-parse", "dir/x"); got != base {
		t.Fatal("existing branch moved")
	}
	// Re-running every outcome (crash recovery) is idempotent.
	if branch, _, err := publishResultRefs(run, "c", 1, "fresh", head); err != nil || branch != "fresh" {
		t.Fatalf("retry: %q %v", branch, err)
	}
}

// A branch an unborn worktree has checked out (git checkout --orphan) is not created under it.
func TestPublishBranchRefCheckedOutUnborn(t *testing.T) {
	run, _, head := branchRepo(t)
	branchMust(t, run, "checkout", "-q", "--orphan", "unborn")
	if _, err := publishBranchRef(run, "unborn", head); err == nil || !strings.Contains(err.Error(), "checked out") {
		t.Fatalf("expected checked-out refusal, got %v", err)
	}
	if _, err := run("rev-parse", "--verify", "-q", "refs/heads/unborn"); err == nil {
		t.Fatal("unborn branch was created")
	}
}

func TestValidateResultBranch(t *testing.T) {
	run, base, _ := branchRepo(t)
	branchMust(t, run, "update-ref", "refs/remotes/origin/trunk", base)
	branchMust(t, run, "symbolic-ref", "refs/remotes/origin/HEAD", "refs/remotes/origin/trunk")
	for _, name := range []string{"job", "Job", "job/manual", "JOB/1", "job/123-2", "main", "Main", "master", "release", "release/1", "Release/1", "release-next", "bad name", "-flag", "a..b", "a@{b", "@{-1}", "refs/heads/main"} {
		t.Run(name, func(t *testing.T) {
			if err := validateResultBranch(run, name); err == nil {
				t.Fatalf("accepted %q", name)
			}
		})
	}
	// Existing branches cannot be moved, so the default branch needs no special knowledge.
	for _, name := range []string{"feature/result", "trunk", "develop", "jobs", "job-1", "myjob/x"} {
		if err := validateResultBranch(run, name); err != nil {
			t.Fatalf("refused %q: %v", name, err)
		}
	}
}
