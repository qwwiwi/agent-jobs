// Package guard implements the PreToolUse hook that rejects tool calls whose
// failure would look like success, or that misattribute authorship:
//   - `$?` read right after a pipeline (it is the status of the last stage only);
//   - a wait-loop that uses `pgrep -f` (the loop's own shell matches its pattern, so it never ends);
//   - the owner's name inside a commit;
//   - a foreign git identity (author/committer other than the configured one);
//   - anything that goes outside: git push/tag; any gh call except read-only subcommands (view, list,
//     diff, checks, status …), so PR/issue create/comment/edit/merge/review/close --comment are rejected.
//
// Commands are parsed (quotes, separators, env prefixes, wrappers), not matched as raw text, so
// spelling variants such as `git -C x 'push'` are still seen. The guard FAILS CLOSED: a form it
// cannot resolve statically (git/gh inside a substitution, eval or `sh -c`; stdin, here-doc or
// here-string into git/gh; a command name or git subcommand from a variable; `env -S`; `xargs git`;
// attached `-cKEY=V` / `-C.`; a shell reading commands from stdin) is rejected, not guessed at.
package guard

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"slices"
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
		checkUnverifiable, checkNothingOut, checkStatusAfterPipe, checkSelfMatchingWait, checkOwnerInCommit, checkForeignIdentity,
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
	configs  []string // -c key=value
	cfgIdx   []int    // argv indices of the -c values
	sub      string
	subIdx   int // argv index of sub (len(argv) when there is none)
	args     []string
	attached string // first global option the guard cannot resolve (`-cK=V`, `-C.`, --config-env, --exec-path)
}

// isProg reports whether argv0 runs the program name (case-insensitive: macOS file systems are).
func isProg(argv0, name string) bool { return strings.EqualFold(base(argv0), name) }

// gitValueOpts are git global options that take a separate value.
var gitValueOpts = map[string]bool{
	"-C": true, "-c": true, "--git-dir": true, "--work-tree": true, "--namespace": true,
	"--super-prefix": true, "--config-env": true, "--exec-path": true, "--list-cmds": true, "--attr-source": true,
}

func parseGit(argv []string) (gitCall, bool) {
	var g gitCall
	if len(argv) == 0 || !isProg(argv[0], "git") {
		return g, false
	}
	i := 1
	for i < len(argv) {
		a := argv[i]
		if !strings.HasPrefix(a, "-") {
			break
		}
		if ((strings.HasPrefix(a, "-c") || strings.HasPrefix(a, "-C")) && len(a) > 2) ||
			strings.HasPrefix(a, "--config-env") || strings.HasPrefix(a, "--exec-path") {
			if g.attached == "" {
				g.attached = a
			}
			i++
			continue
		}
		if gitValueOpts[a] && i+1 < len(argv) {
			if a == "-c" {
				g.configs = append(g.configs, argv[i+1])
				g.cfgIdx = append(g.cfgIdx, i+1)
			}
			i += 2
			continue
		}
		i++
	}
	g.subIdx = i
	if i < len(argv) {
		g.sub, g.args = argv[i], argv[i+1:]
	}
	return g, true
}

// ghPositional returns the argv indices of the first two positional words of a `gh` call.
func ghPositional(argv []string) []int {
	var pos []int
	for i := 1; i < len(argv) && len(pos) < 2; i++ {
		a := argv[i]
		switch {
		case a == "-R" || a == "--repo" || a == "--hostname":
			i++
		case strings.HasPrefix(a, "-"):
		default:
			pos = append(pos, i)
		}
	}
	return pos
}

// parseGh returns the first two positional words of a `gh` call (group, subcommand).
func parseGh(argv []string) (string, string, bool) {
	if len(argv) == 0 || !isProg(argv[0], "gh") {
		return "", "", false
	}
	pos := []string{"", ""}
	for k, i := range ghPositional(argv) {
		pos[k] = argv[i]
	}
	return pos[0], pos[1], true
}

func cannot(form string) string {
	return "blocked by guard: guard cannot verify this form (" + form + "); write it as a plain command " +
		"(no substitution, eval, sh -c, stdin/here-doc, variable or attached option around git/gh)."
}

// indirect are programs that, as an argument of another program (find -exec, flock, watch, …),
// would run something the guard does not see as a command.
var indirect = map[string]bool{
	"git": true, "gh": true, "eval": true, "env": true, "xargs": true, "exec": true, "command": true,
	"nohup": true, "sudo": true, "doas": true, "timeout": true, "nice": true, "stdbuf": true, "source": true,
}

// dataReaders only print, match, inspect or move files named by their arguments: a word "git"
// there is data. Every program NOT positively classified as data-only (here, dataSubcommands or a
// python script file) has its arguments scanned, and a git/gh/shell word among them is rejected
// (fail closed: awk system(), vim -c '!…', docker exec, gdb -ex shell, sed …/e and any runner the
// guard does not know).
var dataReaders = map[string]bool{
	"echo": true, "printf": true, "grep": true, "egrep": true, "fgrep": true, "rg": true, "cat": true,
	"ls": true, "head": true, "tail": true, "wc": true, "file": true, "stat": true, "which": true,
	"type": true, "whereis": true, "man": true, "test": true, "[": true, "cd": true, "pgrep": true,
	"ps": true, "mkdir": true, "touch": true, "cp": true, "mv": true, "rm": true, "find": true,
}

// dataSubcommands are package managers whose listed subcommands only take package names.
var dataSubcommands = map[string]map[string]bool{
	"brew":    {"install": true, "info": true, "search": true, "list": true},
	"apt-get": {"install": true},
	"go":      {"get": true, "install": true},
	"pip":     {"install": true},
	"pip3":    {"install": true},
	"npm":     {"install": true},
}

// findExec are the find actions that run a command.
var findExec = map[string]bool{"-exec": true, "-execdir": true, "-ok": true, "-okdir": true}

// dataOnly reports whether name (lower-cased base name) with args is positively known to treat
// its arguments as data only.
func dataOnly(name string, args []string, dyn []bool) bool {
	switch {
	case name == "find":
		return !slices.ContainsFunc(args, func(a string) bool { return findExec[a] })
	case name == "rg": // --pre runs a command on every file
		return !slices.ContainsFunc(args, func(a string) bool { return strings.HasPrefix(a, "--pre") && !strings.HasPrefix(a, "--pre-glob") })
	case name == "man": // -P / --pager / -H run a program
		return !slices.ContainsFunc(args, func(a string) bool { return strings.HasPrefix(a, "-") })
	case dataReaders[name]:
		return true
	case dataSubcommands[name] != nil:
		return len(args) > 0 && !dyn[0] && dataSubcommands[name][args[0]]
	case strings.HasPrefix(name, "python"):
		return pythonScriptFile(args, dyn)
	}
	return false
}

// pythonValueOpts are the python short options that take a value (attached or as the next word).
const pythonValueOpts = "WX"

// pythonScriptFile reports a plain `python3 [opts] script.py args…` call: no -c / -m anywhere
// (fail closed even after the script name), option values skipped, a literal script file.
func pythonScriptFile(args []string, dyn []bool) bool {
	for _, a := range args {
		if a == "--command" || (strings.HasPrefix(a, "-") && !strings.HasPrefix(a, "--") && strings.ContainsAny(a[1:], "cm")) {
			return false
		}
	}
	for i := 0; i < len(args); i++ {
		a := args[i]
		switch {
		case a == "--":
			return i+1 < len(args) && !dyn[i+1] && args[i+1] != "-"
		case a == "-":
			return false // program from stdin
		case strings.HasPrefix(a, "--"):
			if a == "--check-hash-based-pycs" {
				i++
			}
		case strings.HasPrefix(a, "-"):
			for k := 1; k < len(a); k++ {
				if strings.IndexByte(pythonValueOpts, a[k]) >= 0 {
					if k == len(a)-1 {
						i++ // value is the next word
					}
					break
				}
			}
		default:
			return !dyn[i] // the script file; the rest are its arguments, i.e. data
		}
	}
	return false
}

var reMention = regexp.MustCompile(`(?i)(^|[^a-z0-9_.-])(git|gh)($|[^a-z0-9_-])`)

// checkUnverifiable fails closed on every form whose effect on git/gh cannot be resolved statically.
func checkUnverifiable(scripts []*script, _ string, _ Config) string {
	for _, sc := range scripts {
		for _, pl := range sc.pipelines {
			for _, c := range pl.cmds {
				if r := unverifiable(sc, c); r != "" {
					return r
				}
			}
		}
	}
	return ""
}

func unverifiable(sc *script, c *simpleCmd) string {
	v := split(c.words)
	if v.splitString {
		return cannot("env -S")
	}
	if v.start >= len(c.words) {
		return ""
	}
	argv, dyn := c.words[v.start:], c.dyn[v.start:]
	name := strings.ToLower(base(argv[0]))
	if dyn[0] {
		return cannot("command name from a variable or substitution")
	}
	if name == "git" || name == "gh" {
		switch {
		case sc.depth > 0:
			return cannot(name + " inside a substitution, eval or sh -c")
		case c.stdin:
			return cannot("stdin redirect, here-doc or here-string into " + name)
		case slices.Contains(v.wrappers, "xargs"):
			return cannot("xargs " + name)
		}
		if name == "git" {
			g, _ := parseGit(argv)
			if g.attached != "" {
				return cannot("attached git option " + g.attached)
			}
			if g.subIdx < len(argv) && dyn[g.subIdx] {
				return cannot("git subcommand from a variable")
			}
			for _, k := range g.cfgIdx {
				if dyn[k] {
					return cannot("git -c value from a variable")
				}
			}
			return ""
		}
		for _, k := range ghPositional(argv) {
			if dyn[k] {
				return cannot("gh subcommand from a variable")
			}
		}
		return ""
	}
	switch {
	case name == "eval":
		// The text is parsed as a nested script (git there is rejected); a variable is not visible.
		if slices.Contains(dyn[1:], true) {
			return cannot("eval of a variable or substitution")
		}
	case shells[name]:
		cflag, file, stdin := false, false, false
		for k := 1; k < len(argv); k++ {
			a := argv[k]
			if strings.HasPrefix(a, "--") {
				cflag = cflag || a == "--command" || strings.HasPrefix(a, "--command=") // fish
				continue
			}
			if strings.HasPrefix(a, "-") && len(a) > 1 {
				cflag = cflag || strings.ContainsRune(a, 'c')
				stdin = stdin || strings.ContainsRune(a, 's')
				continue
			}
			file = true
			break
		}
		switch {
		case cflag:
			if slices.Contains(dyn[1:], true) {
				return cannot(name + " -c of a variable or substitution")
			}
		case !file || stdin:
			return cannot(name + " reading commands from stdin")
		}
	case name == "alias":
		for k, a := range argv[1:] {
			if dyn[k+1] || reMention.MatchString(a) {
				return cannot("alias")
			}
		}
	}
	if name == "eval" || dataOnly(name, argv[1:], dyn[1:]) {
		return ""
	}
	// Any other program (find -exec, flock, trap 'git push' EXIT, python -c, awk system(), vim -c '!…',
	// an unknown runner): the guard cannot see what it runs, so a git/gh (or shell/eval) word among its
	// arguments is rejected.
	for _, a := range argv[1:] {
		if b := strings.ToLower(base(a)); indirect[b] || shells[b] || reMention.MatchString(a) {
			return cannot(name + " running git/gh or a shell indirectly")
		}
	}
	return ""
}

// ghRead is the only gh surface a job may use: read-only subcommands. Everything else is rejected
// (fail closed): writes that publish or reach a human (pr/issue create, comment, edit, merge, review,
// close/reopen --comment, gist, repo, label, secret, workflow run, release, api, alias set …) and
// any subcommand gh adds later. A group with no subcommand (`gh pr`) only prints help.
var ghRead = map[string]map[string]bool{
	"":         {"": true},
	"help":     nil, // any
	"version":  nil,
	"status":   {"": true},
	"search":   nil,
	"browse":   nil,
	"pr":       {"": true, "view": true, "list": true, "diff": true, "checks": true, "status": true, "checkout": true},
	"issue":    {"": true, "view": true, "list": true, "status": true},
	"repo":     {"": true, "view": true, "list": true, "clone": true},
	"run":      {"": true, "view": true, "list": true, "watch": true, "download": true},
	"workflow": {"": true, "view": true, "list": true},
	"gist":     {"": true, "view": true, "list": true, "clone": true},
	"label":    {"": true, "list": true},
	"auth":     {"": true, "status": true},
	"alias":    {"": true, "list": true},
}

func ghAllowed(group, sub string) bool {
	subs, ok := ghRead[group]
	if !ok {
		return false
	}
	return subs == nil || subs[sub]
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
			if !ghAllowed(group, sub) {
				return outside(strings.TrimSpace("gh " + group + " " + sub))
			}
		}
		return ""
	})
}

// statusRead reports an unescaped `$?` / `${?}` (single-quoted text is already removed from raw).
func statusRead(raw string) bool {
	for i := 0; i < len(raw); i++ {
		if raw[i] == '\\' {
			i++ // the next character is literal
			continue
		}
		if raw[i] == '$' && (strings.HasPrefix(raw[i+1:], "?") || strings.HasPrefix(raw[i+1:], "{?}")) {
			return true
		}
	}
	return false
}

// pipefailSet returns +1 / -1 when argv turns pipefail on / off, 0 otherwise.
func pipefailSet(argv []string) int {
	if len(argv) == 0 || argv[0] != "set" {
		return 0
	}
	r := 0
	for i := 1; i < len(argv)-1; i++ {
		a := argv[i]
		if len(a) > 1 && (a[0] == '-' || a[0] == '+') && !strings.HasPrefix(a, "--") &&
			strings.HasSuffix(a, "o") && argv[i+1] == "pipefail" {
			r = 1
			if a[0] == '+' {
				r = -1
			}
		}
	}
	return r
}

var (
	ctlOpen  = map[string]bool{"if": true, "while": true, "until": true, "for": true, "case": true, "select": true, "{": true}
	ctlClose = map[string]bool{"fi": true, "done": true, "esac": true, "}": true}
)

// checkStatusAfterPipe: pipefail counts as on only after a top-level, unconditional, foreground
// `set -o pipefail` (a simple command, not inside if/loop/braces/subshell, not after && / ||);
// any `set +o pipefail` anywhere later turns it off.
func checkStatusAfterPipe(scripts []*script, _ string, _ Config) string {
	for _, sc := range scripts {
		pipefail, afterPipe, ctl := false, false, 0
		for _, pl := range sc.pipelines {
			if afterPipe && !pipefail && statusRead(pl.raw()) {
				return "blocked by guard: `$?` right after a pipe is the status of the LAST stage only, " +
					"so a failure of an earlier stage looks like success. Use `set -o pipefail` (top level, before the pipe), " +
					"`${PIPESTATUS[@]}`, or check the command without a pipe."
			}
			for _, c := range pl.cmds {
				for _, w := range c.words {
					if ctlOpen[w] {
						ctl++
					} else if ctlClose[w] {
						ctl--
					} else if !keywords[w] {
						break
					}
				}
				switch pipefailSet(argvOf(c.words)) {
				case -1:
					pipefail = false
				case 1:
					top := len(pl.cmds) == 1 && len(c.words) > 0 && c.words[0] == "set" && ctl == 0 && pl.nest == 0 &&
						(pl.before == "" || pl.before == ";" || pl.before == "\n") && pl.after != "&"
					pipefail = pipefail || top
				}
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
	if len(argv) == 0 || !isProg(argv[0], "pgrep") {
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
				// Loop keywords anywhere in the leading keywords: `then while …`, `{ until …`, `do while …`.
				for _, w := range c.words {
					if w == "while" || w == "until" {
						loop = true
					}
					if !keywords[w] {
						break
					}
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

// staticHeredoc matches `$(cat <<'EOF' … EOF)`: a message whose text is literally in the command.
var staticHeredoc = regexp.MustCompile(`(?s)^\$\(cat <<-?[ \t]*['"]([A-Za-z_][A-Za-z0-9_]*)['"][ \t]*\n(.*)\n[ \t]*([A-Za-z_][A-Za-z0-9_]*)[ \t]*\n?[ \t]*\)$`)

// literalText reports whether a commit text word is fully visible to the guard.
func literalText(w string, dyn bool) bool {
	if !dyn {
		return true
	}
	m := staticHeredoc.FindStringSubmatch(w)
	if m == nil || m[1] != m[3] {
		return false
	}
	for _, line := range strings.Split(m[2], "\n") {
		if strings.TrimSpace(line) == m[1] {
			return false // the here-doc ends early; the rest would run
		}
	}
	return true
}

// commitTexts returns everything that ends up in a commit's message, author or trailers.
// A non-empty problem means the text cannot be known statically (fail closed).
func commitTexts(args []string, dyn []bool, cfg Config) (texts []string, problem string) {
	readFile := func(f string, d bool) {
		switch {
		case d:
			problem = "commit message file from a variable"
			return
		case f == "-" || f == "/dev/stdin" || strings.HasPrefix(f, "/dev/fd/") || strings.HasPrefix(f, "/proc/"):
			problem = "commit message from stdin; write it to a file first, then `git commit -F <file>`"
			return
		}
		if !filepath.IsAbs(f) && cfg.Dir != "" {
			f = filepath.Join(cfg.Dir, f)
		}
		b, err := os.ReadFile(f)
		if err != nil {
			problem = "the commit message file is unreadable; write the file in a separate call first"
			return
		}
		texts = append(texts, string(b))
	}
	text := func(w string, d bool) {
		if !literalText(w, d) {
			problem = "commit message from a variable or substitution; write it literally or use -F <file>"
			return
		}
		texts = append(texts, w)
	}
	reuse := func(form string) {
		problem = "commit message reused from another commit (" + form + "): its text and author are not visible to the guard; " +
			"write the message with -m or -F <file>"
	}
	for i := 0; i < len(args) && problem == ""; i++ {
		a := args[i]
		if strings.HasPrefix(a, "--") && a != "--" {
			name, _, _ := strings.Cut(a[2:], "=")
			if reuseLong(name) {
				reuse("--" + name)
				return texts, problem
			}
			if abbrev(name) {
				problem = "abbreviated long option --" + name + "; write commit options in full"
				return texts, problem
			}
		}
		next := func() (string, bool) {
			if i+1 < len(args) {
				i++
				return args[i], dyn[i]
			}
			return "", false
		}
		switch {
		case a == "--":
			return texts, problem
		case a == "--message" || a == "--trailer" || a == "--author":
			text(next())
		case a == "--file" || a == "--template":
			readFile(next())
		case strings.HasPrefix(a, "--message="), strings.HasPrefix(a, "--trailer="), strings.HasPrefix(a, "--author="):
			text(a[strings.IndexByte(a, '=')+1:], dyn[i])
		case strings.HasPrefix(a, "--file="), strings.HasPrefix(a, "--template="):
			readFile(a[strings.IndexByte(a, '=')+1:], dyn[i])
		case strings.HasPrefix(a, "--"):
		case strings.HasPrefix(a, "-") && len(a) > 1:
			for k := 1; k < len(a); k++ {
				l := a[k]
				if l == 'C' || l == 'c' {
					reuse("-" + string(l))
					return texts, problem
				}
				if strings.IndexByte("mFt", l) < 0 {
					continue
				}
				v, d := a[k+1:], dyn[i]
				if v == "" {
					v, d = next()
				}
				switch l {
				case 'm':
					text(v, d)
				case 'F', 't':
					readFile(v, d)
				}
				break
			}
		}
	}
	return texts, problem
}

// commitReuse are the `git commit` long options that take the message (and with -C/-c the
// authorship) from another commit: --reuse-message, --reedit-message, --fixup (incl. amend:/reword:),
// --squash.
var commitReuse = []string{"reuse-message", "reedit-message", "fixup", "squash"}

func reuseLong(name string) bool { return slices.Contains(commitReuse, name) }

// commitTextOpts are the long options whose value ends up in the commit (or reuses another one).
var commitTextOpts = []string{"message", "file", "template", "trailer", "author", "reuse-message", "reedit-message", "fixup", "squash"}

// abbrev reports a long option git would expand (git accepts unique prefixes) into one of
// commitTextOpts: `--mess=…` is `--message=…`, `--reuse` is `--reuse-message`.
func abbrev(name string) bool {
	if name == "" || slices.Contains(commitTextOpts, name) {
		return false
	}
	return slices.ContainsFunc(commitTextOpts, func(o string) bool { return strings.HasPrefix(o, name) })
}

func checkOwnerInCommit(scripts []*script, _ string, cfg Config) string {
	return eachCmd(scripts, func(c *simpleCmd, argv []string) string {
		g, isGit := parseGit(argv)
		if !isGit || g.sub != "commit" {
			return ""
		}
		dyn := c.dyn[len(c.words)-len(argv):]
		texts, problem := commitTexts(g.args, dyn[g.subIdx+1:], cfg)
		if problem != "" {
			return "blocked by guard: " + problem + "."
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
