package guard

import (
	"strings"
	"testing"
)

// An API merge the reader cannot fully read (#107, fix loop 1) is marked Opaque, so the hook refuses it in
// a gated repository: a field given twice (gh sends -f fields before -F ones, whatever the order), a
// $variable pin at a GraphQL endpoint other than the exact `graphql` (gh groups fields into variables
// only there), an endpoint from a variable or substitution, a body from --input, a -F field from a file
// and a query from a variable. The literal pinned forms stay readable.
func TestGhPrMergesAPIOpaque(t *testing.T) {
	const pass = "0123456789abcdef0123456789abcdef01234567"
	const other = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	const varMutation = `'mutation($oid: GitObjectID!) { mergePullRequest(input: {pullRequestId: "PR_kw", expectedHeadOid: $oid}) { clientMutationId } }'`
	const litMutation = `'mutation { mergePullRequest(input: {pullRequestId: "PR_kw", expectedHeadOid: "` + pass + `"}) { clientMutationId } }'`
	const bareMutation = `'mutation { mergePullRequest(input: {pullRequestId: "PR_kw"}) { clientMutationId } }'`
	for _, tc := range []struct {
		name   string
		cmd    string
		merge  bool   // the command holds one merge call
		opaque string // part of call.Opaque ("" = readable)
		pin    string // the pin read when readable ("" = not checked)
	}{
		// Finding 1: duplicate field keys.
		{"REST -F sha then -f sha", "gh api -X PUT repos/o/r/pulls/5/merge -F sha=" + other + " -f sha=" + pass, true, "more than once", ""},
		{"REST -f sha then -F sha", "gh api -X PUT repos/o/r/pulls/5/merge -f sha=" + pass + " -F sha=" + other, true, "more than once", ""},
		{"REST --raw-field and --field", "gh api -X PUT repos/o/r/pulls/5/merge --raw-field sha=" + pass + " --field=sha=" + other, true, "more than once", ""},
		{"REST sha and sha[]", "gh api -X PUT repos/o/r/pulls/5/merge -f sha=" + pass + " -f 'sha[]=" + other + "'", true, "more than once", ""},
		{"GraphQL pin variable twice", "gh api graphql -F oid=" + other + " -f oid=" + pass + " -f query=" + varMutation, true, "more than once", ""},
		{"GraphQL query twice", "gh api graphql -f query=" + litMutation + " -F query=" + bareMutation, true, "more than once", ""},
		// Finding 2: a $variable pin at a GraphQL endpoint other than the exact graphql.
		{"GraphQL variable pin at /graphql", "gh api /graphql -f oid=" + pass + " -f 'variables[oid]=" + other + "' -f query=" + varMutation,
			true, "$variable", ""},
		{"GraphQL variable pin at a full URL", "gh api https://api.github.com/graphql -f oid=" + pass + " -f query=" + varMutation,
			true, "$variable", ""},
		{"GraphQL variable pin at graphql/", "gh api graphql/ -f oid=" + pass + " -f query=" + varMutation, true, "$variable", ""},
		{"GraphQL variable pin at graphql", "gh api graphql -F oid=" + pass + " -f query=" + varMutation, true, "", pass},
		{"GraphQL variables[] field at graphql is no pin", "gh api graphql -f oid=" + pass + " -f 'variables[oid]=" + other + "' -f query=" + varMutation,
			true, "", pass},
		{"GraphQL literal pin at /graphql", "gh api /graphql -f query=" + litMutation, true, "", pass},
		{"GraphQL pin variable named query is no pin", `gh api graphql -f query='mutation($query: GitObjectID!) { mergePullRequest(input: ` +
			`{pullRequestId: "PR_kw", expectedHeadOid: $query}) { clientMutationId } }'`, true, "", ""},
		// Finding 3: an endpoint from a variable or substitution.
		{"GraphQL mutation to a variable endpoint", `EP=graphql; gh api "$EP" -f query=` + litMutation, true, "endpoint", ""},
		{"GraphQL file query to a variable endpoint", `gh api "$EP" -F query=@merge.graphql`, true, "endpoint", ""},
		{"variable query to a variable endpoint", `gh api "$EP" -f query="$Q"`, true, "endpoint", ""},
		{"--input to a variable endpoint", `gh api "$EP" --input body.json`, true, "endpoint", ""},
		{"merge_method to a variable endpoint", `gh api "$EP" -f merge_method=squash`, true, "endpoint", ""},
		{"PUT to a variable endpoint", `gh api -X PUT "$EP" -f sha=` + pass, true, "endpoint", ""},
		{"PUT to a variable owner", `gh api -X PUT "repos/$O/demo/pulls/5/merge" -f sha=` + pass, true, "endpoint", ""},
		{"mutation to a substituted host", `gh api "https://$(cat host)/graphql" -f query=` + litMutation, true, "endpoint", ""},
		{"PUT with a variable PR number keeps its repository", `gh api -X PUT "repos/o/r/pulls/$N/merge" -f sha=` + pass, true, "", ""},
		{"read of a variable endpoint", `gh api "$EP"`, false, "", ""},
		{"read query to a variable endpoint", `gh api "$EP" -f query='query { viewer { login } }'`, false, "", ""},
		{"POST of other fields to a variable endpoint", `gh api "repos/o/r/issues/$N/comments" -f body=hi`, false, "", ""},
		// Finding 4: bodies the reader cannot see.
		{"GraphQL --input", "gh api graphql --input body.json", true, "--input", ""},
		{"GraphQL query from a file", "gh api graphql -F query=@merge.graphql", true, "file", ""},
		{"GraphQL query from a variable", `gh api graphql -f query="$QUERY"`, true, "variable", ""},
		{"REST --input", "gh api -X PUT repos/o/r/pulls/5/merge --input body.json -f sha=" + pass, true, "--input", ""},
		{"REST field from a file", "gh api -X PUT repos/o/r/pulls/5/merge -f sha=" + pass + " -F commit_message=@msg.txt", true, "file", ""},
		{"raw @ query is text, no file", "gh api graphql -f query=@merge.graphql", false, "", ""},
		// Literal pinned forms stay readable.
		{"REST literal pin", "gh api -X PUT repos/o/r/pulls/5/merge -f merge_method=squash -f sha=" + pass, true, "", pass},
		{"GraphQL literal pin", "gh api graphql -f query=" + litMutation, true, "", pass},
	} {
		calls, _ := GhPrMerges(tc.cmd, "/w")
		if !tc.merge {
			if len(calls) != 0 {
				t.Errorf("%s: calls %+v, want none", tc.name, calls)
			}
			continue
		}
		if len(calls) != 1 {
			t.Errorf("%s: calls %+v, want one", tc.name, calls)
			continue
		}
		got := calls[0]
		switch {
		case tc.opaque == "" && got.Opaque != "":
			t.Errorf("%s: opaque %q, want readable", tc.name, got.Opaque)
		case tc.opaque != "" && !strings.Contains(got.Opaque, tc.opaque):
			t.Errorf("%s: opaque %q, want %q", tc.name, got.Opaque, tc.opaque)
		case tc.pin != "" && (!got.HasMatchHead || got.MatchHead != tc.pin):
			t.Errorf("%s: pin %q %v, want %q", tc.name, got.MatchHead, got.HasMatchHead, tc.pin)
		}
	}
}

// An unreadable API merge still carries the GH_REPO set on the call: the hook resolves its gated context
// from it before refusing.
func TestGhPrMergesAPIOpaqueGHRepo(t *testing.T) {
	calls, _ := GhPrMerges("GH_REPO=acme/demo gh api graphql --input body.json", "/w")
	if len(calls) != 1 || calls[0].Opaque == "" || strings.Join(calls[0].GHRepos, ",") != "acme/demo" {
		t.Fatalf("%+v", calls)
	}
}
