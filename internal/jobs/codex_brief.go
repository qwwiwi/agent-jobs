package jobs

import (
	"fmt"
	"net"
	"net/url"
	"regexp"
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
		if rule.blocked(s, j) && rule.pattern.MatchString(j.Brief) {
			if rule.externalHTTP && !briefExternalHTTP(j.Brief) {
				continue
			}
			warnings = append(warnings, "The brief asks "+rule.asks+"; you cannot "+rule.reason+" – "+rule.instead+".")
		}
	}
	return warnings
}

func briefPattern(pattern string) *regexp.Regexp {
	// A period can end a sentence, but browser.go is a filename rather than a browser request.
	return regexp.MustCompile(`(?i)(^|[^\pL\pN_])(?:` + pattern + `)($|[^\pL\pN_.]|\.(?:$|[^\pL\pN_]))`)
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
	{briefPattern(`curl|https?|https?://|http-запрос\pL*|сетев\pL*\s+запрос\pL*`), codexOffline,
		"for external HTTP/curl", "reach external hosts with network off", "use local fixtures or describe the request for the orchestrator", true},
	{briefPattern("(?:run|running|start|starting|launch|launching|invoke|execute|запус\\pL*|запущ\\pL*|вызов\\pL*|вызва\\pL*)\\s+[`\"']?(?:codex|кодекс)|codex\\s+(?:exec|review|--?\\pL+)"), codexAlways,
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
		if err != nil {
			return true
		}
		host := strings.ToLower(u.Hostname())
		ip := net.ParseIP(host)
		if host != "localhost" && !strings.HasSuffix(host, ".localhost") && (ip == nil || !ip.IsLoopback()) {
			return true
		}
	}
	return len(urls) == 0 // curl/HTTP with no literal URL still deserves an advisory warning.
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
