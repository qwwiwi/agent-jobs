package jobs

import (
	"strings"
	"testing"

	"github.com/qwwiwi/agent-jobs/internal/store"
)

// passedRepo is mergeGateRepo after a judge run on its head with the given suspect tests and a
// release_check that passed on it; it returns the head sha.
func passedRepo(t *testing.T, suspect ...string) (*Manager, string, string) {
	t.Helper()
	m, repo := mergeGateRepo(t)
	sha := gitT(t, repo, "rev-parse", "HEAD")
	j, err := m.StartGate(GateRequest{Kind: KindJudge, Repo: repo, SHA: sha, Model: "sonnet"})
	if err != nil {
		t.Fatal(err)
	}
	block := judgeBlock(j.Gate)
	if suspect == nil {
		suspect = []string{}
	}
	block["suspect_tests"] = suspect
	gateReport(t, block)
	if j := runToEnd(t, m, j.ID); j.Status != store.StatusSucceeded {
		t.Fatalf("judge: %s %s", j.Status, j.Error)
	}
	if r, err := m.ReleaseCheck(repo, sha); err != nil || !r.Pass {
		t.Fatalf("release_check: %+v %v", r, err)
	}
	return m, repo, sha
}

// A merge through the API (#107) gets the gate decision of gh pr merge: a REST PUT to the merge endpoint
// with the sha field as its pin, a GraphQL mergePullRequest with expectedHeadOid, and
// enablePullRequestAutoMerge refused like --auto.
func TestMergeGateAPIMerges(t *testing.T) {
	m, repo := mergeGateRepo(t)
	sha := gitT(t, repo, "rev-parse", "HEAD")
	rest := "gh api -X PUT repos/acme/demo/pulls/5/merge -f merge_method=squash -f sha="
	if r := gateAnswer(t, m, t.TempDir(), rest+sha, ""); !strings.HasPrefix(r, MergeGatePrefix) ||
		!strings.Contains(r, "release_check has not passed on "+sha) {
		t.Fatalf("REST before release_check: %q", r)
	}

	m, repo, sha = passedRepo(t)
	other := strings.Repeat("b", 40)
	gql := func(input string) string {
		return `gh api graphql -f query='mutation { mergePullRequest(input: {pullRequestId: "PR_kw"` + input + `}) { clientMutationId } }'`
	}
	for _, tc := range []struct {
		name, cwd, cmd string
		want           string // part of the answer ("" = allowed silently)
	}{
		{"REST with the pin", t.TempDir(), rest + sha, ""},
		{"REST, --method after the path", t.TempDir(), "gh api repos/Acme/Demo/pulls/5/merge --method PUT -F sha=" + sha, ""},
		{"REST placeholders in the gated repo", repo, "gh api -X PUT repos/{owner}/{repo}/pulls/5/merge -f sha=" + sha, ""},
		{"REST without the pin", t.TempDir(), "gh api -X PUT repos/acme/demo/pulls/5/merge", "without -f sha=<full sha>"},
		{"REST, another sha", t.TempDir(), rest + other, "release_check has not passed on " + other},
		{"REST, a short sha", t.TempDir(), rest + sha[:12], "is not a full 40-hex sha"},
		{"REST, body from --input", t.TempDir(), "gh api -X PUT repos/acme/demo/pulls/5/merge --input b.json -f sha=" + sha,
			"the hook cannot read this merge request"},
		{"REST, a variable sha", t.TempDir(), `gh api -X PUT repos/acme/demo/pulls/5/merge -f sha="$SHA"`, "literally"},
		{"REST in another repository", t.TempDir(), "gh api -X PUT repos/other/repo/pulls/5/merge", ""},
		{"REST read of the merge endpoint", t.TempDir(), "gh api repos/acme/demo/pulls/5/merge", ""},
		{"GraphQL with the pin", repo, gql(`, expectedHeadOid: "` + sha + `"`), ""},
		{"GraphQL without the pin", repo, gql(""), "without expectedHeadOid"},
		{"GraphQL, another sha", repo, gql(`, expectedHeadOid: "` + other + `"`), "release_check has not passed on " + other},
		{"GraphQL outside a repository", t.TempDir(), gql(`, expectedHeadOid: "` + sha + `"`), unsureWarn},
		{"GraphQL auto-merge", repo, `gh api graphql -f query='mutation { enablePullRequestAutoMerge(input: {pullRequestId: "PR_kw"}) { clientMutationId } }'`,
			"auto-merge merges a later head"},
	} {
		r := gateAnswer(t, m, tc.cwd, tc.cmd, "")
		if tc.want == "" && r != "" || tc.want != "" && !strings.Contains(r, tc.want) {
			t.Errorf("%s: %q, want %q", tc.name, r, tc.want)
		}
	}
	if !MergeGateNeeded(rest+sha) || !MergeGateNeeded(gql("")) {
		t.Fatal("the hook does not open the store for an API merge")
	}
}

// A release_check pass whose verdict lists suspect tests (#106) lets the merge through, by gh pr merge and
// by the API, with a needs-live-check warning; a pass without them merges silently.
func TestMergeGateLiveCheckWarning(t *testing.T) {
	m, repo, sha := passedRepo(t, "TestE2E passes with a mock model", "TestServer passes with a scripted fake model")
	for _, cmd := range []string{"gh pr merge 5 --squash --match-head-commit " + sha,
		"gh api -X PUT repos/acme/demo/pulls/5/merge -f sha=" + sha} {
		a, err := m.MergeGateCheck(repo, cmd, "")
		if err != nil || a.Refusal != "" || !strings.HasPrefix(a.Warning, MergeGateWarnPrefix) ||
			!strings.Contains(a.Warning, "live check needed: 2 test(s)") || !strings.Contains(a.Warning, "TestE2E passes with a mock model") ||
			strings.Contains(a.Warning, "\n") {
			t.Fatalf("%s: %+v %v", cmd, a, err)
		}
	}
	m, repo, sha = passedRepo(t)
	if a, err := m.MergeGateCheck(repo, "gh pr merge 5 --match-head-commit "+sha, ""); err != nil || a.Refusal != "" || a.Warning != "" {
		t.Fatalf("no suspect tests: %+v %v", a, err)
	}
}
