package jobs

import (
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/qwwiwi/agent-jobs/internal/store"
)

// A claude review job is read-only by deny rules (#39): the edit tools and the git and file writes are
// denied for review, while fix and general keep them; reads, search and test runners stay allowed.
func TestReviewSettingsDenyWrites(t *testing.T) {
	m := newManager(t)
	deny := func(s map[string]any) []string {
		d, _ := s["permissions"].(map[string]any)["deny"].([]string)
		return d
	}
	want := []string{"Edit", "Write", "MultiEdit", "NotebookEdit", "Bash(git commit:*)", "Bash(git merge:*)",
		"Bash(git rebase:*)", "Bash(git reset:*)", "Bash(git checkout -b:*)", "Bash(git stash:*)",
		"Bash(rm:*)", "Bash(mv:*)"}
	review := deny(m.Settings("review"))
	jobReview := deny(m.JobSettings(&store.Job{ID: "261019-000000-rvw0", Kind: "review"}))
	for _, w := range want {
		if !slices.Contains(review, w) {
			t.Errorf("review deny lacks %s", w)
		}
		if !slices.Contains(jobReview, w) {
			t.Errorf("review job settings lack %s", w)
		}
		for _, kind := range []string{"fix", "general", "research"} {
			if slices.Contains(deny(m.Settings(kind)), w) {
				t.Errorf("%s deny must not hold %s", kind, w)
			}
		}
	}
	for _, allowed := range []string{"Read", "Grep", "Glob", "Bash(go test:*)", "Bash(go vet:*)", "Bash(go build:*)", "Bash(git diff:*)"} {
		if slices.Contains(review, allowed) {
			t.Errorf("review deny must not hold %s", allowed)
		}
	}
}

// The second layer: a review job whose worktree differs from its base at the end (a file written, a
// commit) gets the warning on its job-done line; a review that changed nothing and a fix job do not.
func TestReviewChangedWorktreeWarning(t *testing.T) {
	for _, c := range []struct {
		kind, brief string
		warn        bool
	}{
		{"review", "FAKE_ONLY_UNTRACKED", true},
		{"review", "FAKE_ONLY_UNSTAGED", true},
		{"review", "FAKE_ONLY_COMMIT", true},
		{"review", "FAKE_RM_WORKTREE", true},
		{"review", "nothing", false},
		{"fix", "FAKE_ONLY_UNTRACKED", false},
	} {
		t.Run(c.kind+"/"+c.brief, func(t *testing.T) {
			m := newManager(t)
			repo := newRepo(t)
			j := runToEnd(t, m, enqueue(t, m, c.brief, func(r *StartRequest) { r.RepoPath = repo; r.Kind = c.kind }).ID)
			if j.Status != store.StatusSucceeded {
				t.Fatalf("status %s %q", j.Status, j.Error)
			}
			if got := strings.Contains(j.Error, ReviewChangedWarning); got != c.warn { // job_result's error
				t.Fatalf("warning in the error = %v, want %v: %q", got, c.warn, j.Error)
			}
			line := DoneLine(j, 0)
			if got := strings.Contains(line, ReviewChangedWarning); got != c.warn {
				t.Fatalf("warning on the done line = %v, want %v: %s", got, c.warn, line)
			}
		})
	}
}

// The guard hook of a review job is told the job's worktree (--read-only); other kinds are not.
func TestReviewGuardReadOnlyFlag(t *testing.T) {
	m := newManager(t)
	hook := func(kind string) string {
		s := m.JobSettings(&store.Job{ID: "261019-000000-rvw1", Kind: kind})
		pre := s["hooks"].(map[string]any)["PreToolUse"].([]any)[0].(map[string]any)
		return pre["hooks"].([]any)[0].(map[string]any)["command"].(string)
	}
	want := "--read-only " + shellQuote(filepath.Join(m.JobDir("261019-000000-rvw1"), "work"))
	if h := hook("review"); !strings.Contains(h, want) {
		t.Fatalf("review hook lacks %s: %s", want, h)
	}
	for _, kind := range []string{"fix", "general", "research"} {
		if h := hook(kind); strings.Contains(h, "--read-only") {
			t.Fatalf("%s hook has --read-only: %s", kind, h)
		}
	}
}
