package guard

import (
	"strings"
	"testing"
)

// GhPrMerges also finds the merges gh makes through the API (#107): a PUT to the REST merge endpoint
// (any flag order, -X / --method, with the sha field as the head pin) and a GraphQL mergePullRequest
// (expectedHeadOid as the pin) or enablePullRequestAutoMerge (read as --auto); a gh api call that
// merges nothing is no call.
func TestGhPrMergesAPI(t *testing.T) {
	const sha = "0123456789abcdef0123456789abcdef01234567"
	const mutation = `mutation { mergePullRequest(input: {pullRequestId: "PR_kw", expectedHeadOid: "` + sha + `"}) { clientMutationId } }`
	for _, tc := range []struct {
		name string
		cmd  string
		want *GhMerge // one call (nil = none)
	}{
		{"REST -X PUT", "gh api -X PUT repos/o/r/pulls/5/merge -f sha=" + sha,
			&GhMerge{API: APIMergeREST, Repo: "o/r", MatchHead: sha, HasMatchHead: true}},
		{"REST --method after the path", "gh api repos/o/r/pulls/5/merge -f merge_method=squash --method PUT -F sha=" + sha,
			&GhMerge{API: APIMergeREST, Repo: "o/r", MatchHead: sha, HasMatchHead: true}},
		{"REST --method=put, leading slash", "gh api --method=put /repos/O/R/pulls/5/merge --raw-field=sha=" + sha,
			&GhMerge{API: APIMergeREST, Repo: "O/R", MatchHead: sha, HasMatchHead: true}},
		{"REST -XPUT attached", "gh api -XPUT repos/o/r/pulls/5/merge -fsha=" + sha,
			&GhMerge{API: APIMergeREST, Repo: "o/r", MatchHead: sha, HasMatchHead: true}},
		{"REST full URL", "gh api -X PUT https://api.github.com/repos/o/r/pulls/5/merge",
			&GhMerge{API: APIMergeREST, Repo: "o/r"}},
		{"REST placeholders", "gh api -X PUT repos/{owner}/{repo}/pulls/5/merge -f sha=" + sha,
			&GhMerge{API: APIMergeREST, MatchHead: sha, HasMatchHead: true}},
		{"REST without a sha", "gh api -X PUT repos/o/r/pulls/5/merge", &GhMerge{API: APIMergeREST, Repo: "o/r"}},
		{"REST sha with --input is no pin", "gh api -X PUT repos/o/r/pulls/5/merge --input body.json -f sha=" + sha,
			&GhMerge{API: APIMergeREST, Repo: "o/r"}},
		{"REST behind cd and env", "cd /srv && GH_TOKEN=x gh api -X PUT repos/o/r/pulls/5/merge",
			&GhMerge{API: APIMergeREST, Repo: "o/r", Dirs: []string{"/srv"}}},
		{"REST variable sha", `gh api -X PUT repos/o/r/pulls/5/merge -f sha="$SHA"`,
			&GhMerge{API: APIMergeREST, Repo: "o/r", HasMatchHead: true, Unsure: DynamicWordUnsure}},
		{"REST method from a variable", `gh api -X "$M" repos/o/r/pulls/5/merge`,
			&GhMerge{API: APIMergeREST, Repo: "o/r", Unsure: DynamicWordUnsure}},
		{"REST inside sh -c", `sh -c 'gh api -X PUT repos/o/r/pulls/5/merge'`,
			&GhMerge{API: APIMergeREST, Repo: "o/r", Unsure: "inside a substitution, eval or sh -c"}},
		{"REST GET is no merge", "gh api repos/o/r/pulls/5/merge", nil},
		{"REST POST is no merge", "gh api -X POST repos/o/r/pulls/5/merge", nil},
		{"PUT elsewhere is no merge", "gh api -X PUT repos/o/r/pulls/5/update-branch", nil},
		{"pulls list is no merge", "gh api repos/{owner}/{repo}/pulls", nil},
		{"GraphQL literal pin", "gh api graphql -f query='" + mutation + "'",
			&GhMerge{API: APIMergeGraphQL, MatchHead: sha, HasMatchHead: true}},
		{"GraphQL pin from a variable", `gh api graphql -F oid=` + sha + ` -f query='mutation($oid: GitObjectID!) { ` +
			`mergePullRequest(input: {pullRequestId: "PR_kw", expectedHeadOid: $oid}) { clientMutationId } }'`,
			&GhMerge{API: APIMergeGraphQL, MatchHead: sha, HasMatchHead: true}},
		{"GraphQL without a pin", `gh api graphql -f query='mutation { mergePullRequest(input: {pullRequestId: "PR_kw"}) { clientMutationId } }'`,
			&GhMerge{API: APIMergeGraphQL}},
		{"GraphQL auto-merge", `gh api graphql -f query='mutation { enablePullRequestAutoMerge(input: {pullRequestId: "PR_kw"}) { clientMutationId } }'`,
			&GhMerge{API: APIMergeGraphQL, Auto: true}},
		{"GraphQL query from a file", "gh api graphql -F query=@merge.graphql",
			&GhMerge{API: APIMergeGraphQL, Unsure: "a query the reader cannot see"}},
		{"GraphQL at /graphql", "gh api /graphql -f query='" + mutation + "'",
			&GhMerge{API: APIMergeGraphQL, MatchHead: sha, HasMatchHead: true}},
		{"GraphQL at a full URL without a pin",
			`gh api https://api.github.com/graphql -f query='mutation { mergePullRequest(input: {pullRequestId: "PR_kw"}) { clientMutationId } }'`,
			&GhMerge{API: APIMergeGraphQL}},
		{"GraphQL pin in a comment is no pin", `gh api graphql -f query='mutation { mergePullRequest(input: {pullRequestId: "PR_kw"}) ` +
			`{ clientMutationId } } # expectedHeadOid: "` + sha + `"'`, &GhMerge{API: APIMergeGraphQL}},
		{"GraphQL pin in a string is no pin", `gh api graphql -f query='mutation { mergePullRequest(input: {pullRequestId: "PR_kw", ` +
			`commitBody: """expectedHeadOid: "` + sha + `" """}) { clientMutationId } }'`, &GhMerge{API: APIMergeGraphQL}},
		{"GraphQL pin of another field is no pin", `gh api graphql -f query='mutation { updatePullRequestBranch(input: ` +
			`{pullRequestId: "PR_kw", expectedHeadOid: "` + sha + `"}) { clientMutationId } mergePullRequest(input: ` +
			`{pullRequestId: "PR_kw"}) { clientMutationId } }'`, &GhMerge{API: APIMergeGraphQL}},
		{"GraphQL two aliased merges are unpinned", `gh api graphql -f query='mutation { a: mergePullRequest(input: ` +
			`{pullRequestId: "PR_a", expectedHeadOid: "` + sha + `"}) { clientMutationId } b: mergePullRequest(input: ` +
			`{pullRequestId: "PR_b"}) { clientMutationId } }'`, &GhMerge{API: APIMergeGraphQL}},
		{"GraphQL merge queue", `gh api graphql -f query='mutation { enqueuePullRequest(input: {pullRequestId: "PR_kw", ` +
			`expectedHeadOid: "` + sha + `"}) { clientMutationId } }'`, &GhMerge{API: APIMergeGraphQL, MatchHead: sha, HasMatchHead: true}},
		{"GraphQL merge only in a comment is no merge", `gh api graphql -f query='query { viewer { login } } # mergePullRequest'`, nil},
		{"REST by repository id", "gh api -X PUT repositories/123/pulls/1/merge -f sha=" + sha,
			&GhMerge{API: APIMergeREST, MatchHead: sha, HasMatchHead: true, Unsure: "a repository id"}},
		{"GraphQL read is no merge", `gh api graphql -f query='query { viewer { login } }'`, nil},
		{"GraphQL disable auto-merge is no merge",
			`gh api graphql -f query='mutation { disablePullRequestAutoMerge(input: {pullRequestId: "PR_kw"}) { clientMutationId } }'`, nil},
	} {
		calls, _ := GhPrMerges(tc.cmd, "/w")
		if tc.want == nil {
			if len(calls) != 0 {
				t.Errorf("%s: calls %+v, want none", tc.name, calls)
			}
			continue
		}
		if len(calls) != 1 {
			t.Errorf("%s: calls %+v, want one", tc.name, calls)
			continue
		}
		got, want := calls[0], *tc.want
		if !strings.Contains(got.Unsure, want.Unsure) || (want.Unsure == "") != (got.Unsure == "") {
			t.Errorf("%s: call unsure %q, want %q", tc.name, got.Unsure, want.Unsure)
		}
		if want.Dirs == nil {
			want.Dirs = []string{"/w"}
		}
		if got.API != want.API || got.Repo != want.Repo || got.HasMatchHead != want.HasMatchHead ||
			(want.MatchHead != "" && got.MatchHead != want.MatchHead) || got.Auto != want.Auto ||
			(want.Unsure == "" && strings.Join(got.Dirs, ",") != strings.Join(want.Dirs, ",")) {
			t.Errorf("%s: %+v, want %+v", tc.name, got, want)
		}
	}
}

// A flag the reader cannot place before the group of a gh call that names a merge endpoint leaves the
// call unreadable (a warning), not silently passed.
func TestGhPrMergesAPIUnknownFlag(t *testing.T) {
	calls, _ := GhPrMerges("gh --foo api -X PUT repos/o/r/pulls/5/merge", "/w")
	if len(calls) != 1 || !strings.Contains(calls[0].Unsure, "gh flag --foo") {
		t.Fatalf("%+v", calls)
	}
}
