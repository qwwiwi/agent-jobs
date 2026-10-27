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
// which the hook refuses like --auto). It is a text-level reading of the call's words, as the rest of the
// reader; a merge request it cannot fully read is marked GhMerge.Opaque (apiMergeRequest).

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

// apiField is one -f / -F field of a gh api call.
type apiField struct {
	key, val string
	at       int  // its word
	typed    bool // -F / --field: gh reads a value @file from the file (@- from stdin) and converts others
}

// ghAPIMergeCall reads a gh command as a merge through the API (ok false for any other gh call). The pin
// is the REST call's sha field or the GraphQL expectedHeadOid of the one merge mutation of the query,
// among that field's arguments (a literal, or the field its $variable names; graphQLPin). The
// repository is the REST endpoint's OWNER/REPO; with the {owner}/{repo} placeholders, and for GraphQL (a
// pull request node id names no repository the reader can read), it is the one of GH_REPO or the
// directory, as for gh pr merge without -R. A merge whose request the reader cannot fully read has
// GhMerge.Opaque (apiMergeRequest).
func ghAPIMergeCall(sc *script, c *simpleCmd, v cmdView) (GhMerge, bool) {
	argv, dyn, glob := c.words[v.start:], c.dyn[v.start:], c.glob[v.start:]
	if len(argv) == 0 || dyn[0] || !isProg(argv[0], "gh") {
		return GhMerge{}, false
	}
	pos, _ := ghPositional(argv)
	if len(pos) == 0 || dyn[pos[0]] || argv[pos[0]] != "api" {
		return GhMerge{}, false
	}
	endpoint, method, methodAt, input := -1, "", -1, false
	var fields []apiField
	set := func(name, value string, at int) {
		switch name {
		case "-X", "--method":
			method, methodAt = value, at
		case "-f", "--raw-field", "-F", "--field":
			if k, val, ok := strings.Cut(value, "="); ok {
				fields = append(fields, apiField{key: k, val: val, at: at, typed: name == "-F" || name == "--field"})
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
		return GhMerge{}, false
	}
	call, ok := apiMergeRequest(argv, dyn, endpoint, method, methodAt, input, fields)
	if !ok {
		return GhMerge{}, false
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
	// Also for an unreadable request: the hook resolves its gated context from GH_REPO and the directory.
	call.GHRepos = envAssigned(c.words, c.dyn, 0, v.start, func(why string) {
		if call.Unsure == "" {
			call.Unsure = why
		}
	})
	return call, true
}

// apiMergeRequest reads the request of a gh api call (endpoint: the index of its endpoint in argv) as a
// merge (ok false when it merges nothing). Opaque is set when the reader cannot fully read what gh sends
// (the hook refuses such a merge in a gated repository, #107):
//   - an endpoint from a variable or substitution (beyond the PR number of a literal OWNER/REPO merge
//     endpoint) with the method PUT or one from a variable, or with POST (also gh's implicit one, with
//     fields or --input) and --input, a merge_method field, a query from a variable or a file, or a field
//     naming a merge mutation;
//   - a body from --input (gh then sends the fields as query parameters), a -F field from a file or stdin
//     (@…), a GraphQL query from a variable or substitution;
//   - a field given more than once, also as key and key[…]: gh sends every -f field before the -F ones,
//     whatever their order, so the last one written is not always the one sent;
//   - a $variable expectedHeadOid at a GraphQL endpoint other than the exact `graphql`: only there does gh
//     group the fields into the variables object; at /graphql or a URL it sends them as written (a
//     variables[oid] field may then carry another sha).
func apiMergeRequest(argv []string, dyn []bool, endpoint int, method string, methodAt int, input bool,
	fields []apiField) (call GhMerge, ok bool) {
	fieldVal, fieldAt, fieldTyped := map[string]string{}, map[string]int{}, map[string]bool{}
	seen, dup, file := map[string]bool{}, "", ""
	for _, f := range fields {
		base, _, _ := strings.Cut(f.key, "[")
		if seen[base] && dup == "" {
			dup = base
		}
		seen[base] = true
		if f.typed && strings.HasPrefix(f.val, "@") && file == "" {
			file = f.key
		}
		fieldVal[f.key], fieldAt[f.key], fieldTyped[f.key] = f.val, f.at, f.typed
	}
	methodPut := strings.EqualFold(method, "PUT") || methodAt >= 0 && dyn[methodAt]
	// gh posts when it has fields or --input and no method is given.
	methodPost := strings.EqualFold(method, "POST") || method == "" && (len(fields) > 0 || input)
	q, hasQ := fieldVal["query"]
	qDyn, qFile := hasQ && dyn[fieldAt["query"]], hasQ && fieldTyped["query"] && strings.HasPrefix(q, "@")
	ep := argv[endpoint]
	m := reAPIMergePath.FindStringSubmatch(ep)
	switch {
	case dyn[endpoint] && (m == nil || dynamicText(m[1]+m[2]+m[3])):
		// gh may send it anywhere, graphql included.
		code, _ := graphQLCode(q)
		names := slices.ContainsFunc(fields, func(f apiField) bool { return reGraphQLMerge.MatchString(f.val) })
		if !methodPut && !(methodPost && (input || seen["merge_method"] || names || qDyn || qFile ||
			reGraphQLMerge.MatchString(code))) {
			return GhMerge{}, false
		}
		call.API = APIMergeGraphQL
		if methodPut {
			call.API = APIMergeREST
		}
		call.Unsure = "gh api to an endpoint from a variable or substitution, which may be a pull request's merge"
		call.Opaque = "the endpoint is a variable or substitution"
		return call, true
	case reGraphQLPath.MatchString(ep):
		call.API = APIMergeGraphQL
		if input || qFile {
			call.Unsure = "gh api graphql with a query the reader cannot see (a file or stdin), which may run mergePullRequest"
			call.Opaque = "the query comes from a file or stdin (-F query=@…)"
			if input {
				call.Opaque = "the body comes from --input"
			}
			return call, true
		}
		code, strs := graphQLCode(q)
		ops := reGraphQLMerge.FindAllStringSubmatchIndex(code, -1)
		switch {
		case len(ops) == 0 && qDyn:
			call.Unsure = "gh api graphql with a query from a variable or substitution, which may run mergePullRequest"
			call.Opaque = "the query is a variable or substitution"
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
			pin, variable, ok := graphQLPin(code[ops[0][1]:], strs, fieldVal)
			switch {
			case variable != "" && ep != "graphql":
				call.Opaque = "expectedHeadOid is the $variable " + variable + " at the endpoint " + ep +
					" (gh groups the fields into variables only for the endpoint graphql)"
			case variable == "query" || variable == "operationName":
				// gh sends these fields at the top level, never as variables: no pin.
			default:
				call.MatchHead, call.HasMatchHead = pin, ok
			}
		}
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
		if input {
			call.Opaque = "the body comes from --input (gh then sends the fields as query parameters)"
		} else if _, ok := fieldAt["sha"]; ok {
			call.MatchHead, call.HasMatchHead = fieldVal["sha"], true
		}
	}
	switch {
	case call.Opaque != "":
	case dup != "":
		call.Opaque = "the field " + dup + " is given more than once (gh sends -f fields before -F ones, whatever their order)"
	case file != "":
		call.Opaque = "the field " + file + " comes from a file or stdin (-F " + file + "=@…)"
	}
	return call, true
}

// dynamicText reports text holding a shell expansion ($VAR, $(…), `…`).
func dynamicText(s string) bool {
	return strings.ContainsAny(s, "$`")
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
// literal (from strs) or a $variable (variable is its name, pin the value of the gh api field of that
// name). ok is false without exactly one there.
func graphQLPin(after string, strs []string, fields map[string]string) (pin, variable string, ok bool) {
	rest := strings.TrimLeft(after, " \t\r\n,")
	if !strings.HasPrefix(rest, "(") {
		return "", "", false
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
		return "", "", false
	}
	m := reExpectedHeadOid.FindAllStringSubmatch(rest[1:end], -1)
	if len(m) != 1 {
		return "", "", false
	}
	if m[0][2] != "" {
		v, ok := fields[m[0][2]]
		return v, m[0][2], ok
	}
	n, err := strconv.Atoi(m[0][1])
	if err != nil || n >= len(strs) {
		return "", "", false
	}
	return strs[n], "", true
}
