// Package guard implements the PreToolUse hook that rejects tool calls whose
// failure would look like success, or that misattribute authorship:
//   - `$?` read right after a pipeline (it is the status of the last stage only);
//   - a wait-loop that uses `pgrep -f` (the loop's own shell matches its pattern, so it never ends);
//   - the owner's name inside a commit;
//   - a foreign git identity (author/committer other than the configured one).
package guard

import (
	"encoding/json"
	"fmt"
	"io"
	"regexp"
	"strings"
)

// Config tells the guard who the owner is and which git identity is allowed.
type Config struct {
	OwnerNames []string // names that must never appear in a commit
	GitName    string   // allowed git user.name ("" = no identity override allowed)
	GitEmail   string   // allowed git user.email ("" = no identity override allowed)
}

// HookInput is the subset of the Claude Code PreToolUse payload the guard reads.
type HookInput struct {
	ToolName  string `json:"tool_name"`
	ToolInput struct {
		Command string `json:"command"`
	} `json:"tool_input"`
}

// Decide parses the hook JSON and returns a non-empty reason when the call must be blocked.
func Decide(r io.Reader, cfg Config) (string, error) {
	var in HookInput
	if err := json.NewDecoder(r).Decode(&in); err != nil {
		return "", fmt.Errorf("guard: parse hook input: %w", err)
	}
	if in.ToolName != "Bash" {
		return "", nil
	}
	return Check(in.ToolInput.Command, cfg), nil
}

// Check returns a non-empty reason when the shell command must be blocked.
func Check(cmd string, cfg Config) string {
	if r := checkStatusAfterPipe(cmd); r != "" {
		return r
	}
	if r := checkSelfMatchingWait(cmd); r != "" {
		return r
	}
	if r := checkOwnerInCommit(cmd, cfg); r != "" {
		return r
	}
	return checkForeignIdentity(cmd, cfg)
}

// neutralizeQuotes drops single-quoted text and blanks shell operators inside double quotes,
// so `grep "a|b"` is not mistaken for a pipeline while `echo "rc=$?"` still shows `$?`.
func neutralizeQuotes(s string) string {
	var b strings.Builder
	inSingle, inDouble := false, false
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case inSingle:
			if c == '\'' {
				inSingle = false
			}
		case inDouble:
			if c == '\\' && i+1 < len(s) {
				i++
				continue
			}
			if c == '"' {
				inDouble = false
				continue
			}
			if c == '|' || c == ';' || c == '&' || c == '\n' {
				b.WriteByte(' ')
				continue
			}
			b.WriteByte(c)
		case c == '\'':
			inSingle = true
		case c == '"':
			inDouble = true
		default:
			b.WriteByte(c)
		}
	}
	return b.String()
}

var (
	reAndOr   = regexp.MustCompile(`\|\||&&`)
	reStmtSep = regexp.MustCompile(`[;\n]`)
)

func checkStatusAfterPipe(cmd string) string {
	if strings.Contains(cmd, "pipefail") || strings.Contains(cmd, "PIPESTATUS") {
		return ""
	}
	s := reAndOr.ReplaceAllString(neutralizeQuotes(cmd), "\n")
	stmts := reStmtSep.Split(s, -1)
	for i := 0; i+1 < len(stmts); i++ {
		if !strings.Contains(stmts[i], "|") {
			continue
		}
		// Next non-empty statement.
		for j := i + 1; j < len(stmts); j++ {
			if strings.TrimSpace(stmts[j]) == "" {
				continue
			}
			if strings.Contains(stmts[j], "$?") {
				return "blocked by guard: `$?` right after a pipe is the status of the LAST stage only, " +
					"so a failure of an earlier stage looks like success. Use `set -o pipefail`, `${PIPESTATUS[@]}`, " +
					"or check the command without a pipe."
			}
			break
		}
	}
	return ""
}

var (
	reLoop      = regexp.MustCompile(`\b(while|until)\b`)
	rePgrepSelf = regexp.MustCompile(`\bpgrep\b[^;|&\n]*\s-[A-Za-z]*f`)
)

func checkSelfMatchingWait(cmd string) string {
	if reLoop.MatchString(cmd) && rePgrepSelf.MatchString(cmd) {
		return "blocked by guard: a wait-loop with `pgrep -f` matches its own shell (the pattern is in its command line), " +
			"so it never ends. Wait on a PID (`wait $pid`, `kill -0 $pid`) or a file instead."
	}
	return ""
}

var reGitCommit = regexp.MustCompile(`\bgit\b[^;&|\n]*\bcommit\b`)

func checkOwnerInCommit(cmd string, cfg Config) string {
	if !reGitCommit.MatchString(cmd) {
		return ""
	}
	low := strings.ToLower(cmd)
	for _, name := range cfg.OwnerNames {
		n := strings.ToLower(strings.TrimSpace(name))
		if n != "" && strings.Contains(low, n) {
			return fmt.Sprintf("blocked by guard: the owner's name (%q) must not appear in a commit.", name)
		}
	}
	return ""
}

var (
	reGitC        = regexp.MustCompile(`-c\s+["']?user\.(name|email)=("[^"]*"|'[^']*'|\S+)`)
	reGitConfig   = regexp.MustCompile(`\bgit\b[^;&|\n]*\bconfig\b(?:\s+--\S+)*\s+user\.(name|email)\s+("[^"]*"|'[^']*'|[^\s;&|]+)`)
	reAuthor      = regexp.MustCompile(`--author(?:=|\s+)("[^"]*"|'[^']*'|\S+)`)
	reIdentityEnv = regexp.MustCompile(`\bGIT_(AUTHOR|COMMITTER)_(NAME|EMAIL)=("[^"]*"|'[^']*'|\S*)`)
)

func unquote(v string) string {
	v = strings.TrimSpace(v)
	if len(v) >= 2 && (v[0] == '"' || v[0] == '\'') && v[len(v)-1] == v[0] {
		return v[1 : len(v)-1]
	}
	return strings.Trim(v, `"'`)
}

func allowedValue(field, v string, cfg Config) bool {
	v = unquote(v)
	switch strings.ToLower(field) {
	case "name":
		return cfg.GitName != "" && v == cfg.GitName
	case "email":
		return cfg.GitEmail != "" && strings.EqualFold(v, cfg.GitEmail)
	}
	return false
}

func foreign(what string) string {
	return "blocked by guard: foreign git identity (" + what + "). Commits must use the repository's configured identity; " +
		"do not override user.name/user.email, --author or GIT_AUTHOR_*/GIT_COMMITTER_*."
}

func checkForeignIdentity(cmd string, cfg Config) string {
	for _, m := range reGitC.FindAllStringSubmatch(cmd, -1) {
		if !allowedValue(m[1], m[2], cfg) {
			return foreign("-c user." + m[1])
		}
	}
	for _, m := range reGitConfig.FindAllStringSubmatch(cmd, -1) {
		if !allowedValue(m[1], m[2], cfg) {
			return foreign("git config user." + m[1])
		}
	}
	for _, m := range reAuthor.FindAllStringSubmatch(cmd, -1) {
		v := unquote(m[1])
		ok := cfg.GitName != "" && cfg.GitEmail != "" && v == fmt.Sprintf("%s <%s>", cfg.GitName, cfg.GitEmail)
		if !ok {
			return foreign("--author")
		}
	}
	for _, m := range reIdentityEnv.FindAllStringSubmatch(cmd, -1) {
		if !allowedValue(m[2], m[3], cfg) {
			return foreign("GIT_" + m[1] + "_" + m[2])
		}
	}
	return ""
}
