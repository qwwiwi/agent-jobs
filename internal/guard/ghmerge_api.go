package guard

import (
	"regexp"
	"slices"
	"strconv"
	"strings"
)

// The merges gh makes through the API (#107), read for the merge hook like `gh pr merge` (ghMergeCall):
// `gh api` with the method PUT and the REST merge endpoint repos/OWNER/REPO/pulls/N/merge, and `gh api
// graphql` whose query runs mergePullRequest or enqueuePullRequest (or enablePullRequestAutoMerge,
// which the hook refuses like --auto). It is a text-level reading of the call's words, as the rest of the reader.

// ghAPIValueLong / ghAPIValueShort are the `gh api` options that take a value (gh 2.x).
var (
	ghAPIValueLong = map[string]bool{"--cache": true, "--field": true, "--header": true, "--hostname": true, "--input": true,
		"--jq": true, "--method": true, "--preview": true, "--raw-field": true, "--template": true}
	ghAPIValueShort = "FHXfpqt"
)

var (
	// reAPIMergePath is the REST merge endpoint as gh api takes it: repos/OWNER/REPO/pulls/N/merge (or
	// repositories/ID/pulls/N/merge), also with a leading slash or as a full URL (with GitHub Enterprise's
	// api/v3 prefix), a trailing slash or a query.
	reAPIMergePath = regexp.MustCompile(`^(?:[A-Za-z]+://[^/]+/(?:api/v3/)?|/)?(?:repos/([^/]+)/([^/]+)|repositories/([^/]+))` +
		`/pulls/([^/?#]+)/merge/?(?:[?#].*)?$`)
	// reGraphQLPath is the GraphQL endpoint: gh's own `graphql`, and the path gh posts the fields to as a
	// JSON body ({"query": …}) otherwise (a leading slash, a full URL, GitHub Enterprise's api/graphql).
	reGraphQLPath = regexp.MustCompile(`^(?:[A-Za-z]+://[^/]+/(?:api/)?|/)?graphql/?(?:[?#].*)?$`)
	// reGraphQLMerge is a GraphQL mutation field that merges a pull request (enqueuePullRequest: into the
	// merge queue, at expectedHeadOid too) or turns auto-merge on.
	reGraphQLMerge = regexp.MustCompile(`\b(mergePullRequest|enqueuePullRequest|enablePullRequestAutoMerge)\b`)
	// reExpectedHeadOid is expectedHeadOid in the code of a GraphQL query (graphQLCode): a string literal,
	// masked as "N", or a $variable.
	reExpectedHeadOid = regexp.MustCompile(`\bexpectedHeadOid\s*:\s*(?:"([0-9]+)"|\$([A-Za-z_][A-Za-z0-9_]*))`)
	// reEndpointPlaceholder is a placeholder gh api fills itself (from GH_REPO or the directory's
	// repository): its braces are no brace expansion.
	reEndpointPlaceholder = regexp.MustCompile(`\{(?:owner|repo|branch)\}`)
)

// apiMergeText reports a word that names a merge through the API: the REST merge endpoint or a GraphQL
// merge mutation.
func apiMergeText(w string) bool {
	return reAPIMergePath.MatchString(w) || reGraphQLMerge.MatchString(w)
}

// ghAPIMergeCall reads a gh command as a merge through the API (ok false for any other gh call). The pin
// is the REST call's sha field (none when the body comes from --input: gh then sends the fields as query
// parameters, and the body's sha is not read) or the GraphQL expectedHeadOid of the one merge mutation
// of the query, among that field's arguments (a literal, or the field its $variable names; graphQLPin).
// The repository is the REST endpoint's OWNER/REPO; with the {owner}/{repo}
// placeholders, and for GraphQL (a pull request node id names no repository the reader can read), it is
// the one of GH_REPO or the directory, as for gh pr merge without -R.
func ghAPIMergeCall(sc *script, c *simpleCmd, v cmdView) (GhMerge, bool) {
	argv, dyn, glob := c.words[v.start:], c.dyn[v.start:], c.glob[v.start:]
	var call GhMerge
	if len(argv) == 0 || dyn[0] || !isProg(argv[0], "gh") {
		return call, false
	}
	pos, _ := ghPositional(argv)
	if len(pos) == 0 || dyn[pos[0]] || argv[pos[0]] != "api" {
		return call, false
	}
	endpoint, method, methodAt, input := -1, "", -1, false
	fieldVal, fieldAt := map[string]string{}, map[string]int{} // the last value of each field and its word
	set := func(name, value string, at int) {
		switch name {
		case "-X", "--method":
			method, methodAt = value, at
		case "-f", "--raw-field", "-F", "--field":
			if k, val, ok := strings.Cut(value, "="); ok {
				fieldVal[k], fieldAt[k] = val, at
			}
		case "--input":
			input = true
		}
	}
	for i := pos[0] + 1; i < len(argv); i++ {
		a := argv[i]
		switch {
		case a == "--":
			if endpoint < 0 && i+1 < len(argv) {
				endpoint = i + 1
			}
			i = len(argv)
		case strings.HasPrefix(a, "--"):
			name, value, eq := strings.Cut(a, "=")
			at := i
			if ghAPIValueLong[name] && !eq && i+1 < len(argv) {
				i++
				value, at = argv[i], i
			}
			set(name, value, at)
		case strings.HasPrefix(a, "-") && len(a) > 1:
			for k := 1; k < len(a); k++ {
				if strings.IndexByte(ghAPIValueShort, a[k]) < 0 {
					continue // a boolean shorthand: -i
				}
				value, at := strings.TrimPrefix(a[k+1:], "="), i
				if value == "" && i+1 < len(argv) {
					i++
					value, at = argv[i], i
				}
				set("-"+a[k:k+1], value, at)
				break
			}
		default:
			if endpoint < 0 {
				endpoint = i
			}
		}
	}
	if endpoint < 0 {
		return call, false
	}
	methodPut := strings.EqualFold(method, "PUT") || methodAt >= 0 && dyn[methodAt]
	ep := argv[endpoint]
	switch m := reAPIMergePath.FindStringSubmatch(ep); {
	case reGraphQLPath.MatchString(ep) && !dyn[endpoint]:
		call.API = APIMergeGraphQL
		q, hasQ := fieldVal["query"]
		if input || hasQ && strings.HasPrefix(q, "@") {
			call.Unsure = "gh api graphql with a query the reader cannot see (a file or stdin), which may run mergePullRequest"
			return call, true
		}
		code, strs := graphQLCode(q)
		ops := reGraphQLMerge.FindAllStringSubmatchIndex(code, -1)
		switch {
		case len(ops) == 0 && hasQ && dyn[fieldAt["query"]]:
			call.Unsure = "gh api graphql with a query from a variable or substitution, which may run mergePullRequest"
			return call, true
		case len(ops) == 0:
			return GhMerge{}, false
		}
		for _, op := range ops {
			if code[op[2]:op[3]] == "enablePullRequestAutoMerge" {
				call.Auto = true
			}
		}
		// Only one merge mutation is read for its pin: with aliases (`a: mergePullRequest(…) b:
		// mergePullRequest(…)`) one pin would let the other merge any head.
		if len(ops) == 1 && !call.Auto {
			call.MatchHead, call.HasMatchHead = graphQLPin(code[ops[0][1]:], strs, fieldVal)
		}
	case m == nil && dyn[endpoint] && methodPut:
		call.API = APIMergeREST
		call.Unsure = "gh api PUT to an endpoint from a variable or substitution, which may be a pull request's merge"
		return call, true
	case m == nil || !methodPut:
		// Not the merge endpoint, or a GET (the default) or POST, which merge nothing.
		return GhMerge{}, false
	default:
		call.API = APIMergeREST
		owner, repo := m[1] == "{owner}", m[2] == "{repo}"
		switch {
		case m[3] != "":
			call.Unsure = "a repository id in the endpoint of gh api (repositories/" + m[3] + "), which the reader cannot tie " +
				"to a repository"
		case owner && repo:
			// gh fills them from GH_REPO or the directory: the call names no repository itself.
		case owner || repo:
			call.Unsure = "a {owner} or {repo} placeholder next to a literal part in the endpoint of gh api"
		default:
			call.Repo = m[1] + "/" + m[2]
		}
		if _, ok := fieldAt["sha"]; ok && !input {
			call.MatchHead, call.HasMatchHead = fieldVal["sha"], true
		}
	}
	switch {
	case call.Unsure != "":
	case slices.Contains(dyn, true):
		call.Unsure = DynamicWordUnsure
	case apiGlob(argv, glob):
		call.Unsure = "a glob or brace in the words of gh api (the shell may expand it into other words)"
	case sc.depth > 0:
		call.Unsure = "gh api merge inside a substitution, eval or sh -c"
	case c.stdin || slices.Contains(v.wrappers, "xargs"):
		call.Unsure = "gh api merge with arguments or input the reader cannot see"
	}
	for _, w := range c.words[v.lead:v.start] {
		if strings.HasPrefix(w, "-") && call.Unsure == "" {
			call.Unsure = "a wrapper option before gh (" + w + "), which may change its directory"
		}
	}
	call.GHRepos = envAssigned(c.words, c.dyn, 0, v.start, func(why string) {
		if call.Unsure == "" {
			call.Unsure = why
		}
	})
	return call, true
}

// apiGlob reports a word of a gh api call with an unquoted glob or brace the shell may expand (glob[i]),
// beyond the placeholders gh fills itself ({owner}, {repo}, {branch}).
func apiGlob(argv []string, glob []bool) bool {
	for i, w := range argv {
		if i < len(glob) && glob[i] && hasGlob(reEndpointPlaceholder.ReplaceAllString(w, "")) {
			return true
		}
	}
	return false
}

// graphQLCode is GraphQL document q with its comments (# to the end of the line) removed and each string
// literal ("…" or """…""") replaced by "N", N its index in strs (the literal's text, escapes read as the
// character after the backslash): what is left is code, so a field or an argument name inside a comment
// or a string is not read as one.
func graphQLCode(q string) (code string, strs []string) {
	var b strings.Builder
	for i := 0; i < len(q); {
		switch {
		case q[i] == '#':
			for i < len(q) && q[i] != '\n' && q[i] != '\r' {
				i++
			}
		case strings.HasPrefix(q[i:], `"""`):
			var lit strings.Builder
			i += 3
			for i < len(q) && !strings.HasPrefix(q[i:], `"""`) {
				if strings.HasPrefix(q[i:], `\"""`) {
					lit.WriteString(`"""`)
					i += 4
					continue
				}
				lit.WriteByte(q[i])
				i++
			}
			i += 3
			b.WriteString(`"` + strconv.Itoa(len(strs)) + `"`)
			strs = append(strs, lit.String())
		case q[i] == '"':
			var lit strings.Builder
			for i++; i < len(q) && q[i] != '"' && q[i] != '\n'; i++ {
				if q[i] == '\\' && i+1 < len(q) {
					i++
				}
				lit.WriteByte(q[i])
			}
			i++
			b.WriteString(`"` + strconv.Itoa(len(strs)) + `"`)
			strs = append(strs, lit.String())
		default:
			b.WriteByte(q[i])
			i++
		}
	}
	return b.String(), strs
}

// graphQLPin is the expectedHeadOid among the arguments of the merge field whose code (graphQLCode)
// starts right after its name: the one expectedHeadOid between the parentheses that follow it, a string
// literal (from strs) or a $variable (the value of the gh api field of that name). ok is false without
// exactly one there.
func graphQLPin(after string, strs []string, fields map[string]string) (pin string, ok bool) {
	rest := strings.TrimLeft(after, " \t\r\n,")
	if !strings.HasPrefix(rest, "(") {
		return "", false
	}
	depth, end := 0, -1
	for i := 0; i < len(rest) && end < 0; i++ {
		switch rest[i] {
		case '(':
			depth++
		case ')':
			if depth--; depth == 0 {
				end = i
			}
		}
	}
	if end < 0 {
		return "", false
	}
	m := reExpectedHeadOid.FindAllStringSubmatch(rest[1:end], -1)
	if len(m) != 1 {
		return "", false
	}
	if m[0][2] != "" {
		v, ok := fields[m[0][2]]
		return v, ok
	}
	n, err := strconv.Atoi(m[0][1])
	if err != nil || n >= len(strs) {
		return "", false
	}
	return strs[n], true
}
