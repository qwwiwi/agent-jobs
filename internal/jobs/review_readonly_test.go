package jobs

import (
	"slices"
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
