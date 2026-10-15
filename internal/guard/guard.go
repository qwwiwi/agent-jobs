// Package guard implements the PreToolUse hook that rejects tool calls whose
// failure would look like success, or that misattribute authorship:
//   - `$?` read right after a pipeline (it is the status of the last stage only);
//   - a wait-loop that uses `pgrep -f` (the loop's own shell matches its pattern, so it never ends);
//   - the owner's name inside a commit;
//   - a foreign git identity (author/committer other than the configured one);
//   - anything that goes outside: git push/tag, gh release/api, PR/issue create/comment/edit/merge/review.
//
// Commands are parsed (quotes, separators, env prefixes, wrappers, substitutions, `sh -c`, `eval`),
// not matched as raw text, so spelling variants such as `git -C x 'push'` are still seen.
package guard

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

// Config tells the guard who the owner is and which git identity is allowed.
type Config struct {
	OwnerNames []string // names that must never appear in a commit
	GitName    string   // allowed git user.name ("" = no identity override allowed)
	GitEmail   string   // allowed git user.email ("" = no identity override allowed)
	Dir        string   // working directory of the call (resolves `git commit -F <file>`)
}

// HookInput is the subset of the Claude Code PreToolUse payload the guard reads.
type HookInput struct {
	ToolName  string `json:"tool_name"`
	Cwd       string `json:"cwd"`
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
	if cfg.Dir == "" {
		cfg.Dir = in.Cwd
	}
	return Check(in.ToolInput.Command, cfg), nil
}

// Check returns a non-empty reason when the shell command must be blocked.
func Check(cmd string, cfg Config) string {
	scripts := parseAll(cmd)
	for _, check := range []func([]*script, string, Config) string{
		checkNothingOut, checkStatusAfterPipe, checkSelfMatchingWait, checkOwnerInCommit, checkForeignIdentity,
	} {
		if r := check(scripts, cmd, cfg); r != "" {
			return r
		}
	}
	return ""
}

// eachCmd calls f with the real argv (keywords, assignments, wrappers stripped) of every command.
func eachCmd(scripts []*script, f func(c *simpleCmd, argv []string) string) string {
	for _, sc := range scripts {
		for _, pl := range sc.pipelines {
			for _, c := range pl.cmds {
				if r := f(c, argvOf(c.words)); r != "" {
					return r
				}
			}
		}
	}
	return ""
}

// gitCall is a parsed `git [global options] <sub> <args>`.
type gitCall struct {
	configs []string // -c key=value
	sub     string
	args    []string
}

// gitValueOpts are git global options that take a separate value.
var gitValueOpts = map[string]bool{
	"-C": true, "-c": true, "--git-dir": true, "--work-tree": true, "--namespace": true,
	"--super-prefix": true, "--config-env": true, "--exec-path": true, "--list-cmds": true, "--attr-source": true,
}

func parseGit(argv []string) (gitCall, bool) {
	var g gitCall
	if len(argv) == 0 || base(argv[0]) != "git" {
		return g, false
	}
	i := 1
	for i < len(argv) {
		a := argv[i]
		if !strings.HasPrefix(a, "-") {
			break
		}
		if gitValueOpts[a] && i+1 < len(argv) {
			if a == "-c" {
				g.configs = append(g.configs, argv[i+1])
			}
			i += 2
			continue
		}
		i++
	}
	if i < len(argv) {
		g.sub, g.args = argv[i], argv[i+1:]
	}
	return g, true
}

// parseGh returns the first two positional words of a `gh` call (group, subcommand).
func parseGh(argv []string) (string, string, bool) {
	if len(argv) == 0 || base(argv[0]) != "gh" {
		return "", "", false
	}
	var pos []string
	for i := 1; i < len(argv) && len(pos) < 2; i++ {
		a := argv[i]
		switch {
		case a == "-R" || a == "--repo" || a == "--hostname":
			i++
		case strings.HasPrefix(a, "-"):
		default:
			pos = append(pos, a)
		}
	}
	for len(pos) < 2 {
		pos = append(pos, "")
	}
	return pos[0], pos[1], true
}

var ghOut = map[string]map[string]bool{
	"pr":    {"create": true, "new": true, "comment": true, "edit": true, "merge": true, "review": true},
	"issue": {"create": true, "new": true, "comment": true, "edit": true},
	"alias": {"set": true, "import": true}, // an alias can disguise any of the above
}

func outside(what string) string {
	return "blocked by guard: `" + what + "` reaches outside the job. A job commits locally only; " +
		"the orchestrator publishes after reading the report and the diff."
}

func checkNothingOut(scripts []*script, _ string, _ Config) string {
	return eachCmd(scripts, func(_ *simpleCmd, argv []string) string {
		if g, ok := parseGit(argv); ok {
			for _, kv := range g.configs {
				if strings.HasPrefix(strings.ToLower(kv), "alias.") {
					return outside("git -c alias.*")
				}
			}
			switch g.sub {
			case "push", "tag":
				return outside("git " + g.sub)
			case "config":
				if k, _, set := gitConfigWrite(g.args); set && strings.HasPrefix(k, "alias.") {
					return outside("git config alias.*")
				}
			}
			return ""
		}
		if group, sub, ok := parseGh(argv); ok {
			if group == "release" || group == "api" || ghOut[group][sub] {
				return outside(strings.TrimSpace("gh " + group + " " + sub))
			}
		}
		return ""
	})
}

func statusRead(raw string) bool { return strings.Contains(raw, "$?") || strings.Contains(raw, "${?}") }

// applySet updates the pipefail state from a `set` command.
func applySet(argv []string, pipefail bool) bool {
	if len(argv) == 0 || argv[0] != "set" {
		return pipefail
	}
	for i := 1; i < len(argv)-1; i++ {
		a := argv[i]
		if len(a) > 1 && (a[0] == '-' || a[0] == '+') && !strings.HasPrefix(a, "--") &&
			strings.HasSuffix(a, "o") && argv[i+1] == "pipefail" {
			pipefail = a[0] == '-'
		}
	}
	return pipefail
}

func checkStatusAfterPipe(scripts []*script, _ string, _ Config) string {
	for _, sc := range scripts {
		pipefail, afterPipe := false, false
		for _, pl := range sc.pipelines {
			if afterPipe && !pipefail && statusRead(pl.raw()) {
				return "blocked by guard: `$?` right after a pipe is the status of the LAST stage only, " +
					"so a failure of an earlier stage looks like success. Use `set -o pipefail`, `${PIPESTATUS[@]}`, " +
					"or check the command without a pipe."
			}
			for _, c := range pl.cmds {
				pipefail = applySet(argvOf(c.words), pipefail)
			}
			afterPipe = len(pl.cmds) > 1
		}
	}
	return ""
}

// pgrepValueFlags are pgrep/pkill short flags that take a value.
const pgrepValueFlags = "dgGPstuUFcJ"

// pgrepFull returns the pattern of a `pgrep -f` call and whether it is case-insensitive.
func pgrepFull(argv []string) (pattern string, icase, ok bool) {
	if len(argv) == 0 || base(argv[0]) != "pgrep" {
		return "", false, false
	}
	full := false
	for i := 1; i < len(argv); i++ {
		a := argv[i]
		switch {
		case a == "--full":
			full = true
		case a == "--ignore-case":
			icase = true
		case strings.HasPrefix(a, "--"):
		case strings.HasPrefix(a, "-") && len(a) > 1:
			full = full || strings.ContainsRune(a, 'f')
			icase = icase || strings.ContainsRune(a, 'i')
			if strings.ContainsRune(pgrepValueFlags, rune(a[len(a)-1])) {
				i++
			}
		default:
			pattern = a
		}
	}
	return pattern, icase, full
}

func checkSelfMatchingWait(scripts []*script, whole string, _ Config) string {
	for _, sc := range scripts {
		loop := false
		for _, pl := range sc.pipelines {
			for _, c := range pl.cmds {
				if len(c.words) > 0 && (c.words[0] == "while" || c.words[0] == "until") {
					loop = true
				}
			}
		}
		if !loop {
			continue
		}
		r := eachCmd([]*script{sc}, func(_ *simpleCmd, argv []string) string {
			pat, icase, ok := pgrepFull(argv)
			if !ok {
				return ""
			}
			if icase {
				pat = "(?i)" + pat
			}
			re, err := regexp.Compile(pat)
			// The loop's own shell has the whole command in its argv: a pattern matching it never ends.
			if err != nil || re.MatchString(whole) {
				return "blocked by guard: this wait-loop's `pgrep -f` pattern matches the loop's own command line, " +
					"so it never ends. Wait on a PID (`wait $pid`, `kill -0 $pid`) or a file, or use a pattern " +
					"that cannot match itself (e.g. `[s]erver`)."
			}
			return ""
		})
		if r != "" {
			return r
		}
	}
	return ""
}

// commitTexts returns everything that ends up in a commit's message, author or trailers.
// ok=false means a message file could not be read (fail closed).
func commitTexts(c *simpleCmd, args []string, cfg Config) (texts []string, ok bool) {
	ok = true
	readFile := func(f string) {
		if f == "-" {
			texts = append(texts, c.heredoc, c.raw)
			return
		}
		if !filepath.IsAbs(f) && cfg.Dir != "" {
			f = filepath.Join(cfg.Dir, f)
		}
		b, err := os.ReadFile(f)
		if err != nil {
			ok = false
			return
		}
		texts = append(texts, string(b))
	}
	for i := 0; i < len(args); i++ {
		a := args[i]
		next := func() string {
			if i+1 < len(args) {
				i++
				return args[i]
			}
			return ""
		}
		switch {
		case a == "--":
			return texts, ok
		case a == "--message" || a == "--trailer" || a == "--author":
			texts = append(texts, next())
		case a == "--file":
			readFile(next())
		case strings.HasPrefix(a, "--message="), strings.HasPrefix(a, "--trailer="), strings.HasPrefix(a, "--author="):
			texts = append(texts, a[strings.IndexByte(a, '=')+1:])
		case strings.HasPrefix(a, "--file="):
			readFile(strings.TrimPrefix(a, "--file="))
		case strings.HasPrefix(a, "--"):
		case strings.HasPrefix(a, "-") && len(a) > 1:
			for k := 1; k < len(a); k++ {
				l := a[k]
				if strings.IndexByte("mFCct", l) < 0 {
					continue
				}
				v := a[k+1:]
				if v == "" {
					v = next()
				}
				switch l {
				case 'm':
					texts = append(texts, v)
				case 'F':
					readFile(v)
				}
				break
			}
		}
	}
	return texts, ok
}

func checkOwnerInCommit(scripts []*script, _ string, cfg Config) string {
	return eachCmd(scripts, func(c *simpleCmd, argv []string) string {
		g, isGit := parseGit(argv)
		if !isGit || g.sub != "commit" {
			return ""
		}
		texts, ok := commitTexts(c, g.args, cfg)
		if !ok {
			return "blocked by guard: the commit message file is unreadable. Write the file in a separate call first."
		}
		low := strings.ToLower(strings.Join(texts, "\n"))
		for _, name := range cfg.OwnerNames {
			n := strings.ToLower(strings.TrimSpace(name))
			if n != "" && strings.Contains(low, n) {
				return fmt.Sprintf("blocked by guard: the owner's name (%q) must not appear in a commit.", name)
			}
		}
		return ""
	})
}

// gitConfigWrite returns the key (lower-cased) and value of a `git config` call that sets a value.
func gitConfigWrite(args []string) (key, value string, set bool) {
	var pos []string
	read := false
	for i := 0; i < len(args); i++ {
		a := args[i]
		switch a {
		case "--file", "-f", "--blob", "--type", "--default", "--comment", "--value":
			i++
			continue
		case "--get", "--get-all", "--get-regexp", "--get-urlmatch", "--unset", "--unset-all", "--list", "-l",
			"--rename-section", "--remove-section", "--edit", "-e", "--get-color", "--get-colorbool":
			read = true
			continue
		}
		if strings.HasPrefix(a, "-") {
			continue
		}
		pos = append(pos, a)
	}
	if len(pos) > 0 {
		switch pos[0] {
		case "set":
			pos = pos[1:]
		case "get", "list", "unset", "rename-section", "remove-section", "edit":
			return "", "", false
		}
	}
	if read || len(pos) < 2 {
		return "", "", false
	}
	return strings.ToLower(pos[0]), pos[1], true
}

// identityField maps a git config key to name/email when it sets an identity.
func identityField(key string) string {
	switch strings.ToLower(key) {
	case "user.name", "author.name", "committer.name":
		return "name"
	case "user.email", "author.email", "committer.email":
		return "email"
	}
	return ""
}

func allowedValue(field, v string, cfg Config) bool {
	switch field {
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

var (
	reIdentityEnv = regexp.MustCompile(`^GIT_(AUTHOR|COMMITTER)_(NAME|EMAIL)=(.*)$`)
	reConfigKey   = regexp.MustCompile(`^GIT_CONFIG_KEY_[0-9]+=(.*)$`)
)

func checkForeignIdentity(scripts []*script, _ string, cfg Config) string {
	return eachCmd(scripts, func(c *simpleCmd, argv []string) string {
		// Environment: prefixes, `env`, `export` — any word that assigns an identity variable.
		for _, w := range c.words {
			if m := reIdentityEnv.FindStringSubmatch(w); m != nil && !allowedValue(strings.ToLower(m[2]), m[3], cfg) {
				return foreign("GIT_" + m[1] + "_" + m[2])
			}
			if m := reConfigKey.FindStringSubmatch(w); m != nil && identityField(m[1]) != "" {
				return foreign("GIT_CONFIG_KEY_* " + m[1])
			}
			if strings.HasPrefix(w, "GIT_CONFIG_PARAMETERS=") {
				low := strings.ToLower(w)
				for _, k := range []string{"user.", "author.", "committer.", "alias."} {
					if strings.Contains(low, k) {
						return foreign("GIT_CONFIG_PARAMETERS")
					}
				}
			}
		}
		g, ok := parseGit(argv)
		if !ok {
			return ""
		}
		for _, kv := range g.configs {
			k, v, _ := strings.Cut(kv, "=")
			if f := identityField(k); f != "" && !allowedValue(f, v, cfg) {
				return foreign("-c " + k)
			}
		}
		switch g.sub {
		case "config":
			if k, v, set := gitConfigWrite(g.args); set {
				if f := identityField(k); f != "" && !allowedValue(f, v, cfg) {
					return foreign("git config " + k)
				}
			}
		case "commit":
			for i := 0; i < len(g.args); i++ {
				a := g.args[i]
				var v string
				switch {
				case a == "--author" && i+1 < len(g.args):
					v = g.args[i+1]
				case strings.HasPrefix(a, "--author="):
					v = strings.TrimPrefix(a, "--author=")
				default:
					continue
				}
				want := fmt.Sprintf("%s <%s>", cfg.GitName, cfg.GitEmail)
				if cfg.GitName == "" || cfg.GitEmail == "" || v != want {
					return foreign("--author")
				}
			}
		}
		return ""
	})
}
