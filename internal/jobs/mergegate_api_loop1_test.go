package jobs

import (
	"strings"
	"testing"
)

// A merge through the API the hook cannot fully read (#107, fix loop 1) is refused in a gated
// repository, the context resolved first (the endpoint's OWNER/REPO, else GH_REPO and the directory):
// a field given twice, a $variable pin at a GraphQL endpoint other than `graphql`, an endpoint from a
// variable or substitution, a body from --input, a -F field from a file and a query from a variable. The
// literal pinned forms with a release_check pass merge; outside a gated context the unreadable forms
// keep the warning.
func TestMergeGateAPIOpaqueRefused(t *testing.T) {
	m, repo, pass := passedRepo(t)
	other := strings.Repeat("b", 40)
	varMutation := `'mutation($oid: GitObjectID!) { mergePullRequest(input: {pullRequestId: "PR_kw", expectedHeadOid: $oid}) { clientMutationId } }'`
	litMutation := `'mutation { mergePullRequest(input: {pullRequestId: "PR_kw", expectedHeadOid: "` + pass + `"}) { clientMutationId } }'`
	outside := t.TempDir()
	for _, tc := range []struct {
		name, cwd, cmd, env string
		want                string // refused (opaqueRefused), warned (unsureWarn) or allowed ("")
	}{
		// Finding 1: duplicate field keys.
		{"REST -F sha then -f sha", outside, "gh api -X PUT repos/acme/demo/pulls/5/merge -F sha=" + other + " -f sha=" + pass, "", opaqueRefused},
		{"GraphQL pin variable twice", repo, "gh api graphql -F oid=" + other + " -f oid=" + pass + " -f query=" + varMutation, "", opaqueRefused},
		// Fix loop 2: a duplicate query hides a merge behind a read query, in either order and from a file.
		{"GraphQL -F mutation then -f read query", repo, `gh api graphql -F 'query=mutation { mergePullRequest(input:{pullRequestId:"PR_kw"}) ` +
			`{ clientMutationId } }' -f 'query=query { viewer { login } }'`, "", opaqueRefused},
		{"GraphQL -f read query then -F mutation", repo, `gh api graphql -f 'query=query { viewer { login } }' -F 'query=mutation { ` +
			`mergePullRequest(input:{pullRequestId:"PR_kw"}) { clientMutationId } }'`, "", opaqueRefused},
		{"GraphQL -F @file query then -f read query", repo, "gh api graphql -F query=@merge.graphql -f 'query=query { viewer { login } }'",
			"", opaqueRefused},
		{"GraphQL -f read query then -F @file query", repo, "gh api graphql -f 'query=query { viewer { login } }' -F query=@merge.graphql",
			"", opaqueRefused},
		{"variable endpoint, -F @file query then -f read query", repo,
			`gh api "$EP" -F query=@merge.graphql -f 'query=query { viewer { login } }'`, "", opaqueRefused},
		{"GraphQL read query without duplicates", repo, "gh api graphql -f 'query=query { viewer { login } }'", "", ""},
		{"REST POST with an array field", repo, "gh api repos/acme/demo/issues/5/labels -f 'labels[]=a' -f 'labels[]=b'", "", ""},
		// Finding 2: a $variable pin at /graphql or a full URL.
		{"GraphQL variable pin at /graphql", repo, "gh api /graphql -f oid=" + pass + " -f 'variables[oid]=" + other + "' -f query=" + varMutation,
			"", opaqueRefused},
		{"GraphQL variable pin at a full URL", repo, "gh api https://api.github.com/graphql -f oid=" + pass + " -f query=" + varMutation,
			"", opaqueRefused},
		// Finding 3: a dynamic endpoint, by the directory and by GH_REPO.
		{"mutation to a variable endpoint", repo, `EP=graphql; gh api "$EP" -f query=` + litMutation, "", opaqueRefused},
		{"mutation to a variable endpoint, GH_REPO", outside, `EP=graphql; gh api "$EP" -f query=` + litMutation, "acme/demo", opaqueRefused},
		{"PUT to a variable endpoint", repo, `gh api -X PUT "$EP" -f sha=` + pass, "", opaqueRefused},
		// Finding 4: opaque bodies.
		{"GraphQL --input", repo, "gh api graphql --input body.json", "", opaqueRefused},
		{"GraphQL --input, GH_REPO on the call", outside, "GH_REPO=acme/demo gh api graphql --input body.json", "", opaqueRefused},
		{"GraphQL query from a file", repo, "gh api graphql -F query=@merge.graphql", "", opaqueRefused},
		{"GraphQL query from a variable", repo, `gh api graphql -f query="$QUERY"`, "", opaqueRefused},
		{"REST --input", outside, "gh api -X PUT repos/acme/demo/pulls/5/merge --input b.json -f sha=" + pass, "", opaqueRefused},
		// The literal pinned forms with a pass merge.
		{"REST literal pin", outside, "gh api -X PUT repos/acme/demo/pulls/5/merge -f sha=" + pass, "", ""},
		{"GraphQL literal pin", repo, "gh api graphql -f query=" + litMutation, "", ""},
		{"GraphQL literal pin at /graphql", repo, "gh api /graphql -f query=" + litMutation, "", ""},
		{"GraphQL variable pin at graphql", repo, "gh api graphql -F oid=" + pass + " -f query=" + varMutation, "", ""},
		{"gh pr merge pinned", repo, "gh pr merge 5 --match-head-commit " + pass, "", ""},
		// Outside a gated context: the warning as before.
		{"PUT to a variable endpoint outside", outside, `gh api -X PUT "$EP"`, "", unsureWarn},
		{"mutation to a variable endpoint outside", outside, `gh api "$EP" -f query=` + litMutation, "", unsureWarn},
		{"GraphQL --input outside", outside, "gh api graphql --input body.json", "", unsureWarn},
		{"REST field twice in another repository", outside, "gh api -X PUT repos/other/repo/pulls/5/merge -F sha=" + other + " -f sha=" + pass,
			"", ""},
	} {
		a, err := m.MergeGateCheck(tc.cwd, tc.cmd, tc.env)
		if err != nil {
			t.Fatalf("%s: %v", tc.name, err)
		}
		switch tc.want {
		case opaqueRefused:
			if !strings.HasPrefix(a.Refusal, MergeGatePrefix) || !strings.Contains(a.Refusal, opaqueRefused) ||
				!strings.Contains(a.Refusal, "--match-head-commit <sha>") || !strings.Contains(a.Refusal, "-f sha=<full sha>") {
				t.Errorf("%s: %+v, want the unreadable-merge refusal", tc.name, a)
			}
		case unsureWarn:
			if a.Refusal != "" || !strings.HasPrefix(a.Warning, unsureWarn) {
				t.Errorf("%s: %+v, want a warning", tc.name, a)
			}
		default:
			if a.Refusal != "" {
				t.Errorf("%s: %+v, want allowed", tc.name, a)
			}
		}
	}
}

const opaqueRefused = "the hook cannot read this merge request"
