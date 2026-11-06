package jobs

import (
	"fmt"
	"net"
	"net/url"
	"regexp"
	"slices"
	"strings"

	"github.com/qwwiwi/agent-jobs/internal/store"
)

// These settings describe the command line without running the native sandbox probe.
type codexPromptSettings struct {
	mode, sandbox string
	network       bool
}

func (c Config) codexPromptSettings(kind string) codexPromptSettings {
	s := codexPromptSettings{mode: c.codexSandboxMode(), sandbox: CodexSandbox(kind)}
	if s.mode == CodexSandboxModeWorktreeFullAccess {
		s.sandbox, s.network = CodexSandboxFullAccess, true
	} else {
		s.network = s.sandbox == CodexSandboxWrite && c.CodexWriteNetworkOn()
	}
	return s
}

// CodexBriefWarnings is advisory only: one fixed, actionable line per conflicting category.
// Never include matches from the brief: they can contain credentials or injected instructions.
func (c Config) CodexBriefWarnings(j *store.Job) []string {
	if j.Runner != store.RunnerCodex || IsGateKind(j.Kind) {
		return nil
	}
	s := c.codexPromptSettings(j.Kind)
	var warnings []string
	for _, rule := range codexBriefRules {
		if rule.blocked(s, j) && briefRequests(j.Brief, rule.pattern, rule.externalHTTP) {
			warning := "The brief asks " + rule.asks + "; you cannot " + rule.reason + " – " + rule.instead + "."
			// The curl-verb and URL rules share their message: one line per conflicting category.
			if !slices.Contains(warnings, warning) {
				warnings = append(warnings, warning)
			}
		}
	}
	return warnings
}

func briefPattern(pattern string) *regexp.Regexp {
	// A period can end a sentence, but browser.go is a filename rather than a browser request.
	return regexp.MustCompile(`(?i)(^|[^\pL\pN_])(` + pattern + `)($|[^\pL\pN_.]|\.(?:$|[^\pL\pN_]))`)
}

// Clause boundaries end a negation scope (#141): sentence end, a comma, a line break,
// or a coordinating conjunction in either language ("and then" is covered by "then").
var briefClauseBoundary = regexp.MustCompile(`(?i)[.;,!?]|\r|\n|(^|[^\pL\pN_])(?:but|however|then|а|но|однако|потом|затем)($|[^\pL\pN_])`)

// briefPreVerbClause cuts the text before a matched verb at the last clause boundary:
// a negation from an earlier clause ("do not push but run curl") never reaches this verb.
func briefPreVerbClause(text string) string {
	found := briefClauseBoundary.FindAllStringIndex(text, -1)
	if len(found) == 0 {
		return text
	}
	return text[found[len(found)-1][1]:]
}

// briefPostVerbClause cuts the text after a matched verb at the first clause boundary,
// so a post-verb negation ("run curl not needed but run wget") applies only to its own clause.
func briefPostVerbClause(text string) string {
	found := briefClauseBoundary.FindStringIndex(text)
	if found == nil {
		return text
	}
	return text[:found[0]]
}

// Negation applies only before the matched verb, within four preceding words inside the
// verb's own clause, and stops at a clause or line boundary. This is a lexical heuristic:
// "Don't forget to commit" gives no warning by design; generic sandbox guidance still applies.
var briefNegation = regexp.MustCompile(`(?i)(^|[^\pL\pN_])(?:not|cannot|\pL*n['’]t|never|no|without|не|нельзя|запрещ\pL*|без|никогда)(?:[^\pL\pN_.;,:!?\r\n]+[\pL\pN_]+){0,3}[^\pL\pN_.;,:!?\r\n]*$`)

// Post-verb negation (#141) mirrors briefNegation after the matched verb, within
// the same clause: the negation word must follow the verb right away or through a
// linking verb, and only the listed negation words count (English "not" only as
// "not needed/necessary/required"; a bare "no" is left out — "run curl no matter
// what" is a positive request). "commit is not allowed" stays a known-limit
// warning by design: the negation words do not cover "not allowed".
var briefPostNegation = regexp.MustCompile(`(?i)^[^\pL\pN_.;,:!?\r\n]*(?:(?:is|are|was|were|be|been|being|был\pL*|быть|буд\pL*)[^\pL\pN_.;,:!?\r\n]+){0,2}(?:not[^\pL\pN_.;,:!?\r\n]+(?:needed|necessary|required)|nothing|never|нельзя|не(?:[^\pL\pN_.;,:!?\r\n]+(?:надо|нужно|нужен|нужна))?)($|[^\pL\pN_])`)

func briefRequests(brief string, pattern *regexp.Regexp, externalHTTP bool) bool {
	for offset := 0; offset < len(brief); {
		match := pattern.FindStringSubmatchIndex(brief[offset:])
		if match == nil {
			return false
		}
		start, end := offset+match[4], offset+match[5]
		if !briefNegation.MatchString(briefPreVerbClause(brief[:start])) && !briefPostNegation.MatchString(briefPostVerbClause(brief[end:])) {
			target := brief
			if briefHTTPURLs.MatchString(brief[start:end]) {
				target = brief[start:end]
			}
			if !externalHTTP || briefExternalHTTP(target) {
				return true
			}
		}
		// Keep the trailing delimiter available for a subsequent positive match.
		offset = end
	}
	return false
}

var codexBriefRules = []struct {
	pattern               *regexp.Regexp
	blocked               func(codexPromptSettings, *store.Job) bool
	asks, reason, instead string
	externalHTTP          bool
}{
	{briefPattern(`git\s+clone|clone|клонир\pL*|склонир\pL*`), codexOffline,
		"to clone a repository", "fetch a repository with network off", "use the existing checkout; report missing source material", false},
	{briefPattern(`git\s+commit|commit|коммит\pL*|закомми\pL*`), func(s codexPromptSettings, j *store.Job) bool {
		return s.sandbox != CodexSandboxFullAccess || CodexSandbox(j.Kind) == CodexSandboxReadOnly || j.RepoPath == ""
	}, "for a git commit", "commit in this job", "write files and COMMIT_MSG for the wrapper when writable; otherwise describe changes in the report", false},
	{briefPattern(`git\s+push|push|пуш\pL*|запуш\pL*|отправ\pL*\s+коммит\pL*`), codexAlways,
		"for git push", "publish from a background job", "leave the result for the orchestrator to ship", false},
	{briefPattern(`screenshot\pL*|playwright|browser|скриншот\pL*|сним\pL*\s+экрана|браузер\pL*`), codexOffline,
		"for a browser or screenshot", "run a socket-dependent browser with network off", "describe the screenshot to take and report the browser check as not run", false},
	{briefPattern(`psql|pg_virtualenv|postgres\pL*|docker|постгрес\pL*|докер\pL*`), codexOffline,
		"for PostgreSQL or Docker", "run socket-bound services/tests with network off", "write the tests but do not run socket-bound ones; report the command for the orchestrator", false},
	{briefPattern("(?:run|running|use|using|start|starting|launch|launching|invoke|execute|exec|fetch|запус\\pL*|запущ\\pL*|вызов\\pL*|вызва\\pL*|выполн\\pL*|используй\\pL*|скача\\pL*)\\s+(?:[\\pL\\pN_]+[^\\pL\\pN_.;,:!?\\r\\n]+){0,2}[`\"']?(?:curl|wget)"), codexOffline,
		"for external HTTP/curl", "reach external hosts with network off", "use local fixtures or describe the request for the orchestrator", false},
	{briefPattern(`https?://[^\s<>"` + "`" + `]+`), codexOffline,
		"for external HTTP/curl", "reach external hosts with network off", "use local fixtures or describe the request for the orchestrator", true},
	{briefPattern("(?:run|running|start|starting|launch|launching|invoke|execute|запус\\pL*|запущ\\pL*|вызов\\pL*|вызва\\pL*)\\s+[`\"']?(?:codex|кодекс)|(?m:^[\\t ]*(?:[$>][\\t ]*)?codex[\\t ]+(?:exec|review|--?\\pL+))"), codexAlways,
		"to start codex", "start a nested agent in a background job", "edit or review the files directly in this job", false},
}

func codexOffline(s codexPromptSettings, _ *store.Job) bool { return !s.network }
func codexAlways(codexPromptSettings, *store.Job) bool      { return true }

var briefHTTPURLs = regexp.MustCompile(`(?i)https?://[^\s<>"` + "`" + `]+`)

// Loopback URLs are not external fetches (their socket limits are covered by other guidance).
func briefExternalHTTP(brief string) bool {
	urls := briefHTTPURLs.FindAllString(brief, -1)
	for _, raw := range urls {
		u, err := url.Parse(raw)
		if err != nil || u.Hostname() == "" {
			continue
		}
		host := strings.ToLower(u.Hostname())
		ip := net.ParseIP(host)
		if host != "localhost" && !strings.HasSuffix(host, ".localhost") && (ip == nil || !ip.IsLoopback()) {
			return true
		}
	}
	return false
}

func (c Config) codexSandboxSection(j *store.Job) string {
	if j.Runner != store.RunnerCodex {
		return ""
	}
	s := c.codexPromptSettings(j.Kind)
	network := "off"
	if s.network {
		network = "on"
	}
	var b strings.Builder
	fmt.Fprintf(&b, "## Your sandbox\n\ncodex_sandbox=%s; filesystem=%s; network=%s.\n\n", s.mode, s.sandbox, network)
	switch {
	case CodexSandbox(j.Kind) == CodexSandboxReadOnly:
		b.WriteString("- Read-only task: no file edits, no commits; put proposed changes and test cases in the report.\n")
	case s.sandbox == CodexSandboxWrite:
		b.WriteString("- Write files only in work/ and the progress folder (temporary directories are also writable). Git metadata is read-only; the wrapper commits repository changes using progress/COMMIT_MSG.\n")
	default:
		b.WriteString("- No Codex filesystem or network confinement; job-directory and no-publishing rules still apply. For repository changes, commit locally yourself; the wrapper does not commit in this mode.\n")
	}
	if !s.network {
		b.WriteString("- Network off: use the existing checkout and local/offline dependencies; do not clone or fetch external material. Sockets (including loopback) are unavailable: write tests but do not run socket-bound tests, PostgreSQL, containers, or browsers/Playwright; describe the screenshot to take and list unrun checks in the report.\n")
	}
	b.WriteString("- Do not start `codex` or another nested agent, or push/publish results. Complete feasible work and report unavailable steps with reasons.\n")
	for _, warning := range c.CodexBriefWarnings(j) {
		fmt.Fprintf(&b, "- %s\n", warning)
	}
	b.WriteByte('\n')
	return b.String()
}
