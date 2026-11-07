package jobs

import (
	"strings"
	"testing"
)

func TestResultBranchName(t *testing.T) {
	for _, tc := range []struct {
		attempt int
		want    string
	}{{0, "job/abc"}, {1, "job/abc"}, {2, "job/abc-2"}, {10, "job/abc-10"}} {
		if got := resultBranchName("abc", tc.attempt); got != tc.want {
			t.Errorf("attempt %d: %q, want %q", tc.attempt, got, tc.want)
		}
	}
}

// A resumed attempt publishes under its own name: the earlier attempt's ref stays at its commit and
// both results stay reachable (#205).
func TestPublishResultRefsPerAttempt(t *testing.T) {
	run, _, h1 := branchRepo(t)
	if branch, note, err := publishResultRefs(run, "abc", 1, "", h1); err != nil || branch != "job/abc" || note != "" {
		t.Fatalf("attempt 1: %q %q %v", branch, note, err)
	}
	branchMust(t, run, "commit", "-q", "--allow-empty", "-m", "resumed")
	h2 := branchMust(t, run, "rev-parse", "HEAD")
	branch, note, err := publishResultRefs(run, "abc", 2, "", h2)
	if err != nil || branch != "job/abc-2" || note != "" {
		t.Fatalf("attempt 2: %q %q %v", branch, note, err)
	}
	if got := branchMust(t, run, "rev-parse", "refs/heads/job/abc"); got != h1 {
		t.Fatalf("attempt 1 ref moved to %s, want %s", got, h1)
	}
	if got := branchMust(t, run, "rev-parse", "refs/heads/job/abc-2"); got != h2 {
		t.Fatalf("attempt 2 ref at %s, want %s", got, h2)
	}
	// A crash-retry of attempt 2 finds its own ref at the same head: same name, no second write.
	writes := 0
	counting := branchGit(func(args ...string) (string, error) {
		if len(args) > 0 && args[0] == "update-ref" {
			writes++
		}
		return run(args...)
	})
	if branch, _, err := publishResultRefs(counting, "abc", 2, "", h2); err != nil || branch != "job/abc-2" || writes != 0 {
		t.Fatalf("retry: %q writes=%d %v", branch, writes, err)
	}
}

// A requested name inside the generated namespace is never honoured (an old row may carry one), and
// nothing is created for it.
func TestPublishResultRefsReservedRequested(t *testing.T) {
	run, _, head := branchRepo(t)
	branch, note, err := publishResultRefs(run, "abc", 1, "job", head)
	if err != nil || branch != "job/abc" || !strings.Contains(note, "requested branch job not created: reserved") {
		t.Fatalf("branch=%q note=%q err=%v", branch, note, err)
	}
}

// A branch named job would block every job/<id> (a file where a directory is needed): the name is
// refused at validation, so the default publication stays free.
func TestReservedJobNameThenDefaultPublication(t *testing.T) {
	run, _, head := branchRepo(t)
	if err := validateResultBranch(run, "job"); err == nil {
		t.Fatal("requested branch job accepted")
	}
	if created, err := publishBranchRef(run, "job/abc", head); err != nil || !created {
		t.Fatalf("default publication: created=%v err=%v", created, err)
	}
}
