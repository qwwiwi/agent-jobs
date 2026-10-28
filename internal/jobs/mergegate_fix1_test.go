package jobs

import (
	"os"
	"strings"
	"testing"
)

// A gated repository whose names are unknown never drops the gate of another gated repository the
// merge is known to target: the identified target is checked, the unknown one only adds a warning.
func TestMergeGateUnknownOtherKeepsKnownTarget(t *testing.T) {
	m, repo := mergeGateRepo(t)
	other, _ := gateRepo(t)
	approveBrief(t, m, other)
	if err := os.RemoveAll(other); err != nil {
		t.Fatal(err)
	}
	clone := newRepo(t)
	gitT(t, clone, "remote", "add", "origin", "https://github.com/acme/demo.git")
	for _, tc := range []struct{ name, cwd, cmd string }{
		{"by -R", t.TempDir(), "gh pr merge 5 -R acme/demo"},
		{"by URL", t.TempDir(), "gh pr merge https://github.com/acme/demo/pull/5"},
		{"in another clone", clone, "gh pr merge 5"},
		{"in the gated repo", repo, "gh pr merge 5"},
	} {
		if r := gateAnswer(t, m, tc.cwd, tc.cmd, ""); !strings.HasPrefix(r, MergeGatePrefix) {
			t.Errorf("%s: %q, want a refusal", tc.name, r)
		}
	}
	// A merge that targets neither stays a warning: the unknown repository may be it.
	if r := gateAnswer(t, m, t.TempDir(), "gh pr merge 5 -R other/repo", ""); !strings.HasPrefix(r, unsureWarn) {
		t.Errorf("unrelated -R: %q", r)
	}
}

// A parsed merge into a gated repository whose only unreadable part is a variable sha or PR word is
// refused (write it literally); a merge whose target itself is unreadable only warns.
func TestMergeGateDynamicWordInGatedTarget(t *testing.T) {
	m, repo := mergeGateRepo(t)
	plain := newRepo(t)
	gitT(t, plain, "remote", "add", "origin", "https://github.com/other/repo.git")
	for _, tc := range []struct {
		name, cwd, cmd string
		refused        bool
	}{
		{"sha variable in gated cwd", repo, `gh pr merge 5 --match-head-commit "$SHA"`, true},
		{"PR variable in gated cwd", repo, `gh pr merge $PR`, true},
		{"substitution with -R", t.TempDir(), `gh pr merge 5 -R acme/demo --match-head-commit $(git rev-parse HEAD)`, true},
		{"variable -R", repo, `gh pr merge 5 -R "$REPO" --match-head-commit "$SHA"`, false},
		{"variable URL", repo, `gh pr merge "https://github.com/$O/demo/pull/5"`, false},
		{"PR variable elsewhere", plain, `gh pr merge $PR`, false},
	} {
		a, err := m.MergeGateCheck(tc.cwd, tc.cmd, "")
		if err != nil {
			t.Fatalf("%s: %v", tc.name, err)
		}
		switch {
		case tc.refused && (!strings.HasPrefix(a.Refusal, MergeGatePrefix) || !strings.Contains(a.Refusal, "literally")):
			t.Errorf("%s: %+v, want a refusal asking for literal words", tc.name, a)
		case !tc.refused && (a.Refusal != "" || !strings.HasPrefix(a.Warning, unsureWarn)):
			t.Errorf("%s: %+v, want a warning", tc.name, a)
		}
	}
}
