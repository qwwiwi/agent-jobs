// Package guard implements the PreToolUse hook that rejects tool calls whose
// failure would look like success, or that misattribute authorship or reach outside the job:
//   - `$?` read right after a pipeline (it is the status of the last stage only);
//   - a wait-loop that uses `pgrep -f` (the loop's own shell matches its pattern, so it never ends);
//   - the owner's name inside a commit;
//   - a foreign git identity (author/committer other than the configured one);
//   - anything that goes outside: git push/tag/send-pack/http-push; any gh call except read-only
//     subcommands (view, list, diff, checks, status …), so PR/issue create/comment/edit/merge/review/
//     close --comment are rejected;
//   - git configuration that runs programs (hooks, pager, editor, ssh command, credential helper,
//     external diff, aliases …): `git -c` / `git config` only for an allowlist of harmless keys, and no
//     change to the git environment the wrapper sets up (GIT_*, PAGER, EDITOR, VISUAL; env -i / -u, unset);
//   - git subcommands outside git's own command list (aliases, external git-*) or that run commands
//     (rebase -x, submodule foreach, bisect run, difftool, filter-branch, …), and writes into
//     .git/config, .git/hooks or $GIT_DIR;
//   - nested agents (claude, claude-code, codex, gemini, aider, cursor-agent): a job is a leaf;
//   - agent-jobs itself in any form except `agent-jobs mcp --role job` (no nested jobs), and any change
//     to the AGENT_JOBS_* environment (the AGENT_JOBS_JOB marker the wrapper sets).
//
// Commands are parsed (quotes, separators, env prefixes, wrappers), not matched as raw text, so
// spelling variants such as `git -C x 'push'`, `/usr/lib/git-core/git-push` or `{git,push}` are still
// seen, and a form the guard cannot resolve statically around git/gh (git/gh inside a substitution,
// eval or `sh -c`; stdin, here-doc or here-string into git/gh; a command name or git subcommand from a
// variable or a glob/brace; `env -S`; `xargs git`; attached `-cKEY=V` / `-C.`; a shell reading
// commands from stdin) is rejected, not guessed at.
//
// The guard is NOT a sandbox. It blocks the direct spellings of outward actions in a Bash call, and
// together with the git environment scrub in the wrapper (own GIT_CONFIG_GLOBAL, no system config,
// empty hooks dir, cat/true as pager and editor) it keeps git from running programs the guard does
// not see. A script the job writes and then runs is allowed by design (as in the reference): what
// such a script does is not inspected.
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
	GitDirs    []string // the job worktree's real git dirs (admin dir, common dir): file tools never write there
	Dir        string   // working directory of the call (resolves `git commit -F <file>`)
}

// HookInput is the subset of the Claude Code PreToolUse payload the guard reads.
type HookInput struct {
	ToolName  string `json:"tool_name"`
	Cwd       string `json:"cwd"`
	ToolInput struct {
		Command      string `json:"command"`
		FilePath     string `json:"file_path"`
		NotebookPath string `json:"notebook_path"`
	} `json:"tool_input"`
}

// fileTools write the file named in their input (file_path, or notebook_path for NotebookEdit).
var fileTools = map[string]bool{"Write": true, "Edit": true, "MultiEdit": true, "NotebookEdit": true}

// Decide parses the hook JSON and returns a non-empty reason when the call must be blocked.
func Decide(r io.Reader, cfg Config) (string, error) {
	var in HookInput
	if err := json.NewDecoder(r).Decode(&in); err != nil {
		return "", fmt.Errorf("guard: parse hook input: %w", err)
	}
	if cfg.Dir == "" {
		cfg.Dir = in.Cwd
	}
	if fileTools[in.ToolName] {
		p := in.ToolInput.FilePath
		if in.ToolName == "NotebookEdit" {
			p = in.ToolInput.NotebookPath
		}
		return CheckFileWrite(in.ToolName, p, cfg), nil
	}
	if in.ToolName != "Bash" {
		return "", nil
	}
	return Check(in.ToolInput.Command, cfg), nil
}

// resolvePath returns path made absolute (against dir), cleaned, with symlinks resolved on its
// longest existing prefix (the file itself when it exists, else the nearest existing parent). A
// dangling symlink on the way is followed by its target text (Write would create that target).
func resolvePath(path, dir string) string { return resolveDepth(path, dir, 0) }

func resolveDepth(path, dir string, depth int) string {
	if !filepath.IsAbs(path) {
		path = filepath.Join(dir, path)
	}
	path = filepath.Clean(path)
	rest := ""
	for p := path; ; p = filepath.Dir(p) {
		if r, err := filepath.EvalSymlinks(p); err == nil {
			return filepath.Join(r, rest)
		}
		if fi, err := os.Lstat(p); err == nil && fi.Mode()&os.ModeSymlink != 0 && depth < 40 {
			if target, err := os.Readlink(p); err == nil {
				return resolveDepth(filepath.Join(target, rest), filepath.Dir(p), depth+1)
			}
		}
		if p == filepath.Dir(p) {
			return path
		}
		rest = filepath.Join(filepath.Base(p), rest)
	}
}

// inGitDir reports a path with a `.git` segment: the git dir itself, a file in it, or a worktree's
// `.git` file.
func inGitDir(p string) bool {
	return slices.ContainsFunc(strings.Split(filepath.ToSlash(p), "/"), func(s string) bool {
		return strings.EqualFold(s, ".git")
	})
}

// within reports whether p is d or lies inside it (both absolute and clean).
func within(p, d string) bool {
	rel, err := filepath.Rel(d, p)
	return err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) && !filepath.IsAbs(rel)
}

// inJobGitDir reports a path inside one of the job's real git dirs (as given or symlinks resolved):
// they need not be named .git (a separate git dir, a worktree admin dir).
func inJobGitDir(p string, gitDirs []string) bool {
	for _, d := range gitDirs {
		if d == "" {
			continue
		}
		d = filepath.Clean(d)
		if within(p, d) {
			return true
		}
		if r, err := filepath.EvalSymlinks(d); err == nil && within(p, r) {
			return true
		}
	}
	return false
}

// hasDotDot reports a `..` segment in a path as written: the kernel resolves `link/..` after the
// symlink, while filepath.Clean drops it lexically, so such a path is not checked, it is rejected.
func hasDotDot(path string) bool {
	return slices.Contains(strings.Split(filepath.ToSlash(path), "/"), "..")
}

// CheckFileWrite returns a non-empty reason when a file tool (Write, Edit, MultiEdit, NotebookEdit)
// would write inside a git dir: any `.git` directory, the job's own git dirs (cfg.GitDirs), git
// config/hooks paths. A path with a `..` segment is rejected outright; otherwise both the path as
// given and the path with symlinks resolved are checked (a link in the job dir pointing into .git).
func CheckFileWrite(tool, path string, cfg Config) string {
	if path == "" {
		return ""
	}
	if hasDotDot(path) {
		return "blocked by guard: " + tool + " path `" + path + "` contains `..`; write the path without `..` " +
			"(through a symlink `..` is resolved by the kernel, not as written)."
	}
	given := filepath.Clean(path)
	if !filepath.IsAbs(given) {
		given = filepath.Join(cfg.Dir, given)
	}
	for _, p := range []string{given, resolvePath(path, cfg.Dir)} {
		if inGitDir(p) || gitFile(p) || inJobGitDir(p, cfg.GitDirs) {
			return "blocked by guard: " + tool + " into `" + p + "`: a job does not write inside a .git directory " +
				"(git would run programs from its config and hooks)."
		}
	}
	return ""
}

// Check returns a non-empty reason when the shell command must be blocked.
func Check(cmd string, cfg Config) string {
	scripts := parseAll(cmd)
	for _, check := range []func([]*script, string, Config) string{
		checkUnverifiable, checkNestedAgents, checkSelf, checkJobEnv, checkUserManager, checkPackageRunners, checkNothingOut, checkGitEnv, checkGitFiles, checkStatusAfterPipe, checkSelfMatchingWait,
		checkOwnerInCommit, checkForeignIdentity,
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

// isGitName reports a lower-cased base name that runs git: `git` itself or a dashed form such as
// `git-push` (/usr/lib/git-core/git-push is `git push`).
func isGitName(name string) bool {
	return name == "git" || (strings.HasPrefix(name, "git-") && len(name) > 4)
}

// hasGlob reports a glob or brace character: the shell expands such a word into names the guard
// does not see (`{git,push}`, `/usr/bin/g[i]t`, `git pu?h` with a file named push).
func hasGlob(w string) bool { return strings.ContainsAny(w, "{[?*") }

// gitValueOpts are git global options that take a separate value.
var gitValueOpts = map[string]bool{
	"-C": true, "-c": true, "--git-dir": true, "--work-tree": true, "--namespace": true,
	"--super-prefix": true, "--config-env": true, "--exec-path": true, "--list-cmds": true, "--attr-source": true,
}

func parseGit(argv []string) (gitCall, bool) {
	var g gitCall
	if len(argv) == 0 {
		return g, false
	}
	name := strings.ToLower(base(argv[0]))
	if !isGitName(name) {
		return g, false
	}
	if name != "git" { // git-<sub>: the subcommand is in the program name
		g.sub, g.subIdx, g.args = name[len("git-"):], 0, argv[1:]
		return g, true
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
// Before the subcommand only `-R`/`--repo`/`--hostname` with their value (separate, `=` or attached
// `-Rowner/repo`) are understood; any other flag there could take a value and shift which words are
// read as the subcommand, so it is returned as unknown (the caller rejects the call as unverifiable).
func ghPositional(argv []string) (pos []int, unknown string) {
	for i := 1; i < len(argv) && len(pos) < 2; i++ {
		a := argv[i]
		switch {
		case a == "-R" || a == "--repo" || a == "--hostname":
			i++
		case strings.HasPrefix(a, "--repo=") || strings.HasPrefix(a, "--hostname=") ||
			(strings.HasPrefix(a, "-R") && len(a) > 2):
		case (a == "--version" || a == "--help" || a == "-h") && len(pos) == 0 && i == len(argv)-1:
			// `gh --version` / `gh --help` alone only print.
		case strings.HasPrefix(a, "-"):
			return pos, a
		default:
			pos = append(pos, i)
		}
	}
	return pos, ""
}

// parseGh returns the first two positional words of a `gh` call (group, subcommand).
func parseGh(argv []string) (string, string, bool) {
	if len(argv) == 0 || !isProg(argv[0], "gh") {
		return "", "", false
	}
	idx, unknown := ghPositional(argv)
	if unknown != "" { // rejected as unverifiable first; never read as an allowed group
		return "unverifiable " + unknown, "", true
	}
	pos := []string{"", ""}
	for k, i := range idx {
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
	"npx": true, "pnpx": true, "bunx": true, "uvx": true,
	"systemd-run": true, "busctl": true, "dbus-send": true, "gdbus": true, "qdbus": true, "dbus-run-session": true,
	"systemctl": true,
}

// agents are coding agents a job must not start: a job is a leaf, nested agents would run outside
// the job's settings, hook and limits. Package names (`claude-code`, `gemini-cli`, `aider-chat`) and
// any other `<agent>-<suffix>` count too (isAgent).
var agents = map[string]bool{
	"claude": true, "claude-code": true, "codex": true, "gemini": true, "gemini-cli": true, "aider": true,
	"aider-chat": true, "cursor-agent": true,
}

// isAgent reports a lower-cased base name that is an agent or an agent with a `-suffix`.
func isAgent(b string) bool {
	if agents[b] {
		return true
	}
	for a := range agents {
		if strings.HasPrefix(b, a+"-") {
			return true
		}
	}
	return false
}

// execName reports a lower-cased base name that, as an argument, names something that runs commands
// the guard would not see: git (any git-<sub>), gh, a shell, a wrapper/indirect program, a package
// runner or an agent.
func execName(b string) bool {
	return indirect[b] || shells[b] || isAgent(b) || isGitName(b) || isSelf(b)
}

// isSelf reports a lower-cased base name that runs agent-jobs (also `agent-jobs-<suffix>` builds).
func isSelf(b string) bool { return b == "agent-jobs" || strings.HasPrefix(b, "agent-jobs-") }

// jobRoleMCP reports whether args (after the program name) are exactly `mcp --role job` with an
// optional --agent value: the one agent-jobs form a job may run (it is the job's own MCP server).
func jobRoleMCP(args []string) bool {
	if len(args) == 0 || args[0] != "mcp" {
		return false
	}
	roles, agent := 0, 0
	for k := 1; k < len(args); k++ {
		name, val, eq := strings.Cut(strings.TrimLeft(args[k], "-"), "=")
		if !strings.HasPrefix(args[k], "-") || (name != "role" && name != "agent") {
			return false
		}
		if !eq {
			if k+1 >= len(args) {
				return false
			}
			k++
			val = args[k]
		}
		switch name {
		case "role":
			if val != "job" {
				return false
			}
			roles++
		case "agent":
			agent++
		}
	}
	return roles == 1 && agent <= 1
}

// mentionsExec reports a word (e.g. a substitution `$(which git)`) that names an execName program.
func mentionsExec(w string) bool {
	return slices.ContainsFunc(strings.FieldsFunc(strings.ToLower(w), func(r rune) bool {
		return !(r >= 'a' && r <= 'z' || r >= '0' && r <= '9' || r == '-' || r == '_')
	}), execName)
}

// copiers place a file at another name; with git/gh/a shell/an agent as an argument they make a
// renamed executable (`cp /usr/bin/git ./x; ./x push`), so such a call is not data.
var copiers = map[string]bool{"cp": true, "mv": true, "install": true, "ln": true}

// dataReaders only print, match, inspect or move files named by their arguments: a word "git"
// there is data. Every program NOT positively classified as data-only (here, dataSubcommands or a
// python script file) has its arguments scanned, and a git/gh/shell word among them is rejected
// (fail closed: awk system(), vim -c '!…', docker exec, gdb -ex shell, sed …/e and any runner the
// guard does not know).
var dataReaders = map[string]bool{
	"echo": true, "printf": true, "grep": true, "egrep": true, "fgrep": true, "rg": true, "cat": true,
	"ls": true, "head": true, "tail": true, "wc": true, "file": true, "stat": true, "which": true,
	"type": true, "whereis": true, "man": true, "test": true, "[": true, "cd": true, "pgrep": true,
	"ps": true, "mkdir": true, "touch": true, "rm": true, "find": true,
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
	case copiers[name]:
		for k, a := range args {
			if !strings.HasPrefix(a, "-") && (execName(strings.ToLower(base(a))) || (dyn[k] && mentionsExec(a))) {
				return false
			}
		}
		return true
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

var (
	// reMention finds git/gh, an agent or a shell as a word inside an argument, e.g. in interpreter code
	// (`python3 -c '…subprocess.run(["claude", …])'`, `node -e '…codex…'`) or a package path
	// (`npx @anthropic-ai/claude-code`, `node …/claude-code/cli.js`).
	// Agent names may carry a `-suffix` (`gemini-cli`, `aider-chat`, `@google/gemini-cli`, `codex-cli`).
	reMention = regexp.MustCompile(`(?i)(^|[^a-z0-9_.-])((git|gh|sh|bash|zsh|dash|fish)($|[^a-z0-9_-])|` +
		`(claude|codex|gemini|aider|cursor-agent|agent-jobs|systemd-run|systemctl|busctl|dbus-send|gdbus|qdbus|dbus-run-session)($|[^a-z0-9_]))`)
	reGitDashCmd = regexp.MustCompile(`(?i)(^|[^a-z0-9_.-])git-[a-z]`)
)

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
	if v.envUnknown != "" {
		return cannot("abbreviated or unknown env option " + v.envUnknown)
	}
	// A glob/brace in any word that decides which program runs (wrappers, their option values, the
	// timeout duration, the program itself) is expanded by the shell into words the guard never sees.
	// Leading keywords (`{`, `!`) and shell assignments (not glob-expanded) are exempt, as are `[`/`[[`.
	for i := v.lead; i <= v.start && i < len(c.words); i++ {
		if w := c.words[i]; w != "[" && w != "[[" && hasGlob(w) {
			return cannot("glob/brace in the command or its wrappers: " + w)
		}
	}
	if v.start >= len(c.words) {
		return ""
	}
	argv, dyn := c.words[v.start:], c.dyn[v.start:]
	name := strings.ToLower(base(argv[0]))
	if dyn[0] {
		return cannot("command name from a variable or substitution")
	}
	if name == "systemctl" && slices.Contains(v.wrappers, "xargs") {
		return cannot("xargs systemctl (its arguments, e.g. --user, come from stdin)")
	}
	if isGitName(name) || name == "gh" {
		switch {
		case sc.depth > 0:
			return cannot(name + " inside a substitution, eval or sh -c")
		case c.stdin:
			return cannot("stdin redirect, here-doc or here-string into " + name)
		case slices.Contains(v.wrappers, "xargs"):
			return cannot("xargs " + name)
		}
		if name != "gh" {
			g, _ := parseGit(argv)
			for k := 1; k <= g.subIdx && k < len(argv); k++ {
				if hasGlob(argv[k]) {
					return cannot("glob/brace in a git option or subcommand: " + argv[k])
				}
			}
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
		pos, unknown := ghPositional(argv)
		if unknown != "" {
			return cannot("gh flag " + unknown + " before the subcommand (only -R/--repo/--hostname)")
		}
		for _, k := range pos {
			if dyn[k] {
				return cannot("gh subcommand from a variable")
			}
			if hasGlob(argv[k]) {
				return cannot("glob/brace in a gh subcommand: " + argv[k])
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
	for k, a := range argv[1:] {
		if b := strings.ToLower(base(a)); execName(b) || reMention.MatchString(a) || reGitDashCmd.MatchString(a) ||
			(dyn[k+1] && mentionsExec(a)) {
			return cannot(name + " running git/gh, a shell or an agent indirectly")
		}
	}
	return ""
}

// packageRunners fetch and run a package by name (npx, pnpm dlx, …): what runs is decided by a
// registry, not by the command line, and a job has no need for them. Value: the subcommand words
// that make the program a runner ("" = the program itself).
var packageRunners = map[string][]string{
	"npx": {""}, "pnpx": {""}, "bunx": {""}, "uvx": {""},
	"npm": {"exec", "x"}, "pnpm": {"dlx"}, "yarn": {"dlx"}, "bun": {"x"}, "pipx": {"run"}, "uv": {"tool run"},
}

// checkPackageRunners rejects package runners: npx/pnpx/bunx/uvx, npm exec|x, pnpm dlx, yarn dlx,
// bun x, pipx run, uv tool run. The subcommand is looked for among the first three positional words
// (an option value before it would otherwise hide it).
func checkPackageRunners(scripts []*script, _ string, _ Config) string {
	return eachCmd(scripts, func(_ *simpleCmd, argv []string) string {
		if len(argv) == 0 {
			return ""
		}
		name := strings.ToLower(base(argv[0]))
		subs, ok := packageRunners[name]
		if !ok {
			return ""
		}
		var pos []string
		for _, a := range argv[1:] {
			if !strings.HasPrefix(a, "-") && len(pos) < 3 {
				pos = append(pos, strings.ToLower(a))
			}
		}
		joined := " " + strings.Join(pos, " ") + " "
		for _, s := range subs {
			if s == "" || strings.Contains(joined, " "+s+" ") {
				return "blocked by guard: `" + strings.TrimSpace(name+" "+s) + "` fetches and runs a package by name " +
					"(npx, pnpm dlx, yarn dlx, bunx, uvx, pipx run …); a job does not use package runners."
			}
		}
		return ""
	})
}

// checkNestedAgents: a job is a leaf. claude/claude-code/codex/gemini/aider/cursor-agent are rejected as a
// command (also behind env/command/exec and as a path); as an argument they are caught by the
// indirect scan in unverifiable.
func checkNestedAgents(scripts []*script, _ string, _ Config) string {
	return eachCmd(scripts, func(_ *simpleCmd, argv []string) string {
		if len(argv) > 0 && isAgent(strings.ToLower(base(argv[0]))) {
			return "blocked by guard: `" + base(argv[0]) + "` starts a nested agent. A job is a leaf: it does the work itself " +
				"and reports; only the orchestrator starts jobs."
		}
		return ""
	})
}

// userManagerTools reach the user's systemd manager or session bus, where a new transient unit would
// run a process outside the job's cgroup scope.
var userManagerTools = map[string]bool{
	"systemd-run": true, "busctl": true, "dbus-send": true, "gdbus": true, "qdbus": true, "dbus-run-session": true,
}

// cgroupControlFiles move processes between cgroups (or enable controllers on a subtree): writing a pid
// there takes it out of the job's scope, wherever the file system is mounted.
var cgroupControlFiles = map[string]bool{"cgroup.procs": true, "cgroup.threads": true, "cgroup.subtree_control": true}

// pathSegs returns the non-empty segments of a cleaned slash path.
func pathSegs(p string) []string {
	var out []string
	for _, s := range strings.Split(filepath.ToSlash(filepath.Clean(p)), "/") {
		if s != "" && s != "." {
			out = append(out, s)
		}
	}
	return out
}

// hasSegs reports whether want occurs as consecutive segments of segs.
func hasSegs(segs []string, want ...string) bool {
	for i := 0; i+len(want) <= len(segs); i++ {
		if slices.Equal(segs[i:i+len(want)], want) {
			return true
		}
	}
	return false
}

// viaProcRoot reports a path through /proc/<pid>/root or /proc/<pid>/cwd (another process's view of
// the file system, which a lexical check cannot follow).
func viaProcRoot(segs []string) bool {
	for i := 0; i+2 < len(segs); i++ {
		if segs[i] == "proc" && (segs[i+2] == "root" || segs[i+2] == "cwd") {
			return true
		}
	}
	return false
}

// pathForms returns the spellings of a word a cgroup check must see: the value (of `--opt=value`) as
// written and cleaned, made absolute against dir when dir is known, and with symlinks resolved.
func pathForms(w, dir string) [][]string {
	if strings.HasPrefix(w, "-") {
		if _, v, ok := strings.Cut(w, "="); ok {
			w = v
		}
	}
	if w == "" {
		return nil
	}
	forms := [][]string{pathSegs(w)}
	if dir != "" || filepath.IsAbs(w) {
		abs := w
		if !filepath.IsAbs(abs) {
			abs = filepath.Join(dir, abs)
		}
		forms = append(forms, pathSegs(abs), pathSegs(resolvePath(abs, "/")))
	}
	return forms
}

// cgroupPath reports a word naming the cgroup file system or one of its control files in any spelling:
// a sys/fs/cgroup segment run (also after `//`, `/./`, `x/..`, relative to the call's directory, or
// through a symlink), a cgroup.procs / cgroup.threads / cgroup.subtree_control base name, or any path
// through /proc/<pid>/root or /proc/<pid>/cwd.
func cgroupPath(w, dir string) bool {
	for _, segs := range pathForms(w, dir) {
		if len(segs) == 0 {
			continue
		}
		if hasSegs(segs, "sys", "fs", "cgroup") || cgroupControlFiles[segs[len(segs)-1]] || viaProcRoot(segs) {
			return true
		}
	}
	return false
}

// sysDirTarget reports a directory argument of cd/pushd that is /sys or /sys/fs (from there a relative
// path reaches the cgroup file system unseen), absolute or relative to the call's directory.
func sysDirTarget(w, dir string) bool {
	if dir == "" && !filepath.IsAbs(w) {
		return false
	}
	for _, segs := range pathForms(w, dir)[1:] {
		if slices.Equal(segs, []string{"sys"}) || slices.Equal(segs, []string{"sys", "fs"}) {
			return true
		}
	}
	return false
}

func cgroupBlocked(w string) string {
	return "blocked by guard: `" + w + "` reaches the cgroup file system; a job does not move processes out of its scope."
}

// checkUserManager: a job stays in its cgroup scope. systemd-run, busctl, dbus-send, gdbus, qdbus,
// dbus-run-session are rejected as commands (behind wrappers too: argvOf strips env/sudo/timeout/…);
// as an argument or a word inside an argument of another program (xargs, find -exec, python -c) they
// and systemctl are caught by the indirect scan in unverifiable. `systemctl --user` (also abbreviated
// `--u`, `--us`, `--use`, `--user=…`) is rejected (the system manager, without --user, cannot start
// units for the job), and so is any write into the cgroup file system: an output redirection there, or
// such a path as an argument of a program that is not a plain reader (cgroupPath: every spelling), and
// cd/pushd into /sys or /sys/fs. Not a sandbox: a script the job writes is not inspected.
func checkUserManager(scripts []*script, _ string, cfg Config) string {
	return eachCmd(scripts, func(c *simpleCmd, argv []string) string {
		for _, t := range c.outRedirs {
			if cgroupPath(t, cfg.Dir) {
				return cgroupBlocked(t)
			}
		}
		if len(argv) == 0 {
			return ""
		}
		name := strings.ToLower(base(argv[0]))
		if userManagerTools[name] {
			return "blocked by guard: `" + name + "` talks to the user's service manager or session bus; a job does not " +
				"start units or processes outside its scope."
		}
		if name == "systemctl" {
			for _, a := range argv[1:] {
				opt, _, _ := strings.Cut(a, "=")
				if len(opt) >= 3 && strings.HasPrefix("--user", opt) {
					return "blocked by guard: `systemctl --user` controls the user's service manager; a job does not start " +
						"or stop units."
				}
			}
		}
		if gitFileReaders[name] {
			return ""
		}
		for _, a := range argv[1:] {
			if cgroupPath(a, cfg.Dir) || ((name == "cd" || name == "pushd") && sysDirTarget(a, cfg.Dir)) {
				return cgroupBlocked(a)
			}
		}
		return ""
	})
}

// checkSelf: no nested jobs. agent-jobs as a command (by name or path, behind env/command/exec/nohup,
// inside sh -c, eval or a substitution) is rejected unless it is exactly `agent-jobs mcp --role job`;
// as an argument of another program it is caught by the indirect scan in unverifiable.
func checkSelf(scripts []*script, _ string, _ Config) string {
	return eachCmd(scripts, func(_ *simpleCmd, argv []string) string {
		if len(argv) > 0 && isSelf(strings.ToLower(base(argv[0]))) && !jobRoleMCP(argv[1:]) {
			return "blocked by guard: `" + base(argv[0]) + "` controls jobs. A job never starts, cancels or manages jobs " +
				"(no nested jobs); only the orchestrator does."
		}
		return ""
	})
}

var (
	// jobEnv are the agent-jobs variables: AGENT_JOBS_JOB marks every process of a job, the others
	// select state (AGENT_JOBS_HOME, AGENT_JOBS_AGENT, binaries).
	reJobVar    = regexp.MustCompile(`^AGENT_JOBS_[A-Za-z0-9_]*$`)
	reJobAssign = regexp.MustCompile(`^AGENT_JOBS_[A-Za-z0-9_]*\+?=`)
	reJobExpand = regexp.MustCompile(`\$\{AGENT_JOBS_[A-Za-z0-9_]*:?=`)
)

// varSetters take bare variable names that they set, export, remove or turn into an array
// (`mapfile`/`readarray NAME`: an array is never exported, so the marker would vanish from children).
// printf is not here: only `printf -v NAME` assigns (printfTarget).
var varSetters = map[string]bool{
	"unset": true, "export": true, "declare": true, "typeset": true, "local": true, "readonly": true,
	"read": true, "env": true, "mapfile": true, "readarray": true,
}

// printfTarget returns the variable of `printf -v NAME` / `printf -vNAME` ("" when there is none).
func printfTarget(args []string) string {
	for k := 0; k < len(args); k++ {
		a := args[k]
		switch {
		case a == "--" || !strings.HasPrefix(a, "-"):
			return ""
		case a == "-v":
			if k+1 < len(args) {
				return args[k+1]
			}
			return ""
		case strings.HasPrefix(a, "-v"):
			return a[2:]
		}
	}
	return ""
}

// checkJobEnv keeps the job marker: an unset (`unset`, `env -u`), an assignment (prefix, export,
// declare, ${VAR:=…}), `printf -v`, or a setter (also `mapfile`/`readarray`, also behind
// `builtin`/`command`) naming an AGENT_JOBS_* variable – bare or attached to an option (`-vX`, `-tX`)
// – is rejected in any call. Emptying the whole environment (`env -i`) is rejected by checkGitEnv.
func checkJobEnv(scripts []*script, _ string, _ Config) string {
	blocked := func(what string) string {
		return "blocked by guard: `" + what + "` changes the agent-jobs environment of the job (AGENT_JOBS_JOB marks " +
			"the job's processes). Leave AGENT_JOBS_* unchanged."
	}
	for _, sc := range scripts {
		for _, pl := range sc.pipelines {
			for _, c := range pl.cmds {
				v := split(c.words)
				for _, u := range v.envUnset {
					if reJobVar.MatchString(u) {
						return blocked("env -u " + u)
					}
				}
				name := ""
				if v.start < len(c.words) {
					name = strings.ToLower(base(c.words[v.start]))
				}
				if name == "printf" {
					if t := printfTarget(c.words[v.start+1:]); strings.HasPrefix(t, "AGENT_JOBS_") {
						return blocked("printf -v " + t)
					}
				}
				setter := varSetters[name]
				for k, w := range c.words {
					if reJobAssign.MatchString(w) || reJobExpand.MatchString(w) {
						n, _, _ := strings.Cut(w, "=")
						return blocked(n)
					}
					if !setter || k <= v.start {
						continue
					}
					if reJobVar.MatchString(w) || (strings.HasPrefix(w, "-") && strings.Contains(w, "AGENT_JOBS_")) {
						return blocked(name + " " + w)
					}
				}
			}
		}
	}
	return ""
}

var (
	// scrubbed are the variables the wrapper sets for git (GIT_CONFIG_GLOBAL, GIT_CONFIG_NOSYSTEM,
	// GIT_CONFIG_COUNT/KEY/VALUE for core.hooksPath, GIT_PAGER, GIT_EDITOR, GIT_TERMINAL_PROMPT, PAGER,
	// EDITOR, VISUAL) or that redirect git (GIT_DIR, GIT_EXEC_PATH, GIT_SSH_COMMAND, …).
	reScrubVar    = regexp.MustCompile(`^(GIT_[A-Za-z0-9_]*|PAGER|EDITOR|VISUAL)$`)
	reScrubAssign = regexp.MustCompile(`^(GIT_[A-Za-z0-9_]*|PAGER|EDITOR|VISUAL)\+?=`)
	reScrubExpand = regexp.MustCompile(`\$\{(GIT_[A-Za-z0-9_]*|PAGER|EDITOR|VISUAL):?=`)
)

func envBlocked(what string) string {
	return "blocked by guard: `" + what + "` changes the git environment the job runs with (own config, no hooks, " +
		"cat/true as pager and editor). Run git with the environment unchanged."
}

// checkGitEnv keeps the wrapper's git environment intact: an emptied environment (`env -i`, `env -`,
// `exec -c`) or an unset scrubbed variable (`env -u`, `unset`) is always rejected; in a call that runs
// git, any assignment to or mention of a scrubbed variable (prefix, env, export, declare, read,
// printf -v, ${VAR:=…}) and sudo/doas (environment reset) are rejected too.
func checkGitEnv(scripts []*script, _ string, _ Config) string {
	runsGit := false
	_ = eachCmd(scripts, func(_ *simpleCmd, argv []string) string {
		runsGit = runsGit || (len(argv) > 0 && isGitName(strings.ToLower(base(argv[0]))))
		return ""
	})
	for _, sc := range scripts {
		for _, pl := range sc.pipelines {
			for _, c := range pl.cmds {
				v := split(c.words)
				if v.envClear {
					return envBlocked("env -i / env - / exec -c")
				}
				for _, u := range v.envUnset {
					if reScrubVar.MatchString(u) {
						return envBlocked("env -u " + u)
					}
				}
				argv := c.words[v.start:]
				if len(argv) > 0 && strings.ToLower(base(argv[0])) == "unset" {
					for _, a := range argv[1:] {
						if reScrubVar.MatchString(a) {
							return envBlocked("unset " + a)
						}
					}
				}
				if !runsGit {
					continue
				}
				if slices.Contains(v.wrappers, "sudo") || slices.Contains(v.wrappers, "doas") {
					return envBlocked("sudo/doas (resets the environment)")
				}
				for _, w := range c.words {
					if reScrubAssign.MatchString(w) || reScrubVar.MatchString(w) || reScrubExpand.MatchString(w) {
						name, _, _ := strings.Cut(w, "=")
						return envBlocked(name)
					}
				}
			}
		}
	}
	return ""
}

// reGitFiles matches the git files that make git run programs: the repository config, hooks and a
// worktree's own config (also via $GIT_DIR / ${GIT_DIR}).
var reGitFiles = regexp.MustCompile(`(?i)((^|/)\.git/(config|hooks|worktrees/[^/]+/(config|hooks))|\$\{?GIT_DIR\b)`)

// gitFileReaders only read the files named by their arguments, so naming .git/config there is harmless.
var gitFileReaders = map[string]bool{
	"echo": true, "printf": true, "grep": true, "egrep": true, "fgrep": true, "rg": true, "cat": true,
	"ls": true, "head": true, "tail": true, "wc": true, "file": true, "stat": true, "test": true, "[": true,
	"less": true, "diff": true, "pgrep": true, "ps": true, "which": true, "type": true, "whereis": true,
}

// gitDirTargets take a directory as a destination or a new working directory: with `.git` itself as
// an argument they reach its config and hooks under another spelling (`cd .git; echo > config`,
// `ln -s ../.git d`, `cp evil-config .git/`).
var gitDirTargets = map[string]bool{
	"cd": true, "pushd": true, "ln": true, "cp": true, "mv": true, "tee": true, "install": true, "rsync": true,
}

// pathOf returns the cleaned path of a word; for an option `--opt=value` that of its value.
func pathOf(w string) string {
	if strings.HasPrefix(w, "-") {
		if _, v, ok := strings.Cut(w, "="); ok {
			w = v
		}
	}
	return filepath.Clean(w)
}

// gitFile reports a word naming git's config, hooks or $GIT_DIR, also respelled (`.git//config`,
// `.git/./hooks`, `x/../.git/config`).
func gitFile(w string) bool { return reGitFiles.MatchString(w) || reGitFiles.MatchString(pathOf(w)) }

// gitDir reports a word naming a `.git` directory itself.
func gitDir(w string) bool { return strings.EqualFold(filepath.Base(pathOf(w)), ".git") }

// globBlocked is the reason for a glob/brace word where a write could land: the guard does not
// evaluate globs (dotglob, braces and `*` can all resolve to `.git`), it rejects them.
func globBlocked(w string) string {
	return "blocked by guard: `" + w + "` is a glob/brace word in a redirection or an argument of a program " +
		"that may write; the guard does not expand globs (one could resolve to .git). Write the paths out."
}

// dotGlobOn reports a command that makes globs match dot files (`.git`): bash `shopt -s dotglob`,
// a GLOBIGNORE assignment (bash then matches dot files), zsh `setopt globdots` / `set -o globdots`
// (zsh option names ignore case and underscores).
func dotGlobOn(c *simpleCmd, argv []string) bool {
	for _, w := range c.words {
		if strings.HasPrefix(w, "GLOBIGNORE=") {
			return true
		}
	}
	if len(argv) == 0 {
		return false
	}
	switch strings.ToLower(base(argv[0])) {
	case "shopt", "setopt", "unsetopt", "set":
		for _, a := range argv[1:] {
			n := strings.ReplaceAll(strings.ToLower(a), "_", "")
			if strings.Contains(n, "dotglob") || strings.Contains(n, "globdots") {
				return true
			}
		}
	}
	return false
}

// checkGitFiles rejects writes into the repository config, hooks or $GIT_DIR: an output redirection
// to such a path or to `.git` (any program), such a path as an argument of a program that is not a
// plain reader (tee, cp, mv, ln, sed -i, chmod, …), and `.git` itself as the target of
// cd/pushd/ln/cp/mv/tee/install/rsync. Globs are not evaluated: any glob/brace word in a redirection
// target or in an argument of a non-reader is rejected (fail closed), as is turning on dotglob/globdots.
// git itself is checked by the config allowlist instead.
func checkGitFiles(scripts []*script, _ string, _ Config) string {
	blocked := func(p string) string {
		return "blocked by guard: `" + p + "` is git's config, hooks or $GIT_DIR; a job does not write there " +
			"(git would run programs from it)."
	}
	for _, sc := range scripts {
		for _, pl := range sc.pipelines {
			for _, c := range pl.cmds {
				for _, t := range c.outRedirs {
					if gitFile(t) || gitDir(t) {
						return blocked(t)
					}
					if hasGlob(t) {
						return globBlocked(t)
					}
				}
				argv := argvOf(c.words)
				if dotGlobOn(c, argv) {
					return "blocked by guard: dotglob/globdots (or GLOBIGNORE) makes globs match .git; a job does not change glob options."
				}
				if len(argv) == 0 {
					continue
				}
				name := strings.ToLower(base(argv[0]))
				if isGitName(name) || gitFileReaders[name] {
					continue
				}
				// find without an action only matches names: `find . -name '*.go'` is a filter.
				findFilter := name == "find" && !slices.ContainsFunc(argv[1:], func(a string) bool {
					return findExec[a] || a == "-delete" || strings.HasPrefix(a, "-fprint") || a == "-fls"
				})
				for _, a := range argv[1:] {
					if gitFile(a) || (gitDirTargets[name] && gitDir(a)) {
						return blocked(a)
					}
					if hasGlob(a) && !findFilter {
						return globBlocked(a)
					}
				}
			}
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
	"auth":     {"": true}, // no `auth status`: its -t/--show-token prints the token
	"alias":    {"": true, "list": true},
}

func ghAllowed(group, sub string) bool {
	subs, ok := ghRead[group]
	if !ok {
		return false
	}
	return subs == nil || subs[sub]
}

func gitConfigBlocked(what string) string {
	return "blocked by guard: `git " + what + "` is not on the allowlist of harmless git config keys " +
		"(git may run programs from config: hooks, pager, editor, ssh, credential helper, external diff, aliases)."
}

func outside(what string) string {
	return "blocked by guard: `" + what + "` reaches outside the job. A job commits locally only; " +
		"the orchestrator publishes after reading the report and the diff."
}

// gitOutside are git subcommands that publish, mail, serve or reach another system.
var gitOutside = map[string]bool{
	"push": true, "tag": true, "send-pack": true, "http-push": true, "send-email": true, "imap-send": true,
	"svn": true, "p4": true, "daemon": true, "request-pull": true, "instaweb": true, "web--browse": true,
	"cvsexportcommit": true, "cvsimport": true, "cvsserver": true, "archimport": true, "http-backend": true,
	"upload-pack": true, "receive-pack": true, "upload-archive": true, "shell": true,
}

// gitRunsCmd are git subcommands that run arbitrary commands, other git commands the guard does
// not see (for-each-repo), or that install or outlive the run (maintenance start, daemons).
var gitRunsCmd = map[string]bool{
	"filter-branch": true, "filter-repo": true, "difftool": true, "difftool--helper": true, "mergetool": true,
	"for-each-repo": true, "maintenance": true, "hook": true, "remote-ext": true, "remote-fd": true,
	"credential-cache--daemon": true, "fsmonitor--daemon": true,
}

// gitKnown are git's own commands (`git --list-cmds=main`, git 2.43, plus later built-ins). A
// subcommand outside this list is an alias or an external git-* program: rejected, so an alias in
// a config the guard does not see cannot resolve to anything. The commands denied by name
// (gitOutside, gitRunsCmd) are listed too (second block), so their own check decides and names the
// reason, and dropping one from its deny list is not hidden by this one.
var gitKnown = func() map[string]bool {
	m := map[string]bool{}
	for _, c := range strings.Fields(`add am annotate apply archive bisect blame branch bugreport bundle cat-file
		check-attr check-ignore check-mailmap check-ref-format checkout checkout--worker checkout-index cherry
		cherry-pick clean clone column commit commit-graph commit-tree config count-objects credential
		credential-cache credential-store describe diagnose diff diff-files diff-index diff-tree fast-export
		fast-import fetch fetch-pack fmt-merge-msg for-each-ref format-patch fsck fsck-objects gc
		get-tar-commit-id grep hash-object help http-fetch index-pack init init-db interpret-trailers log
		ls-files ls-remote ls-tree mailinfo mailsplit merge merge-base merge-file merge-index merge-octopus
		merge-one-file merge-ours merge-recursive merge-recursive-ours merge-recursive-theirs merge-resolve
		merge-subtree merge-tree mktag mktree multi-pack-index mv name-rev notes pack-objects pack-redundant
		pack-refs patch-id pickaxe prune prune-packed pull quiltimport range-diff read-tree rebase reflog remote
		repack replace rerere reset restore rev-list rev-parse revert rm shortlog show show-branch show-index
		show-ref sparse-checkout stage stash status stripspace submodule submodule--helper subtree switch
		symbolic-ref unpack-file unpack-objects update-index update-ref update-server-info var verify-commit
		verify-pack verify-tag version whatchanged worktree write-tree replay refs backfill last-modified repo
		push tag send-pack http-push send-email imap-send svn p4 daemon request-pull instaweb web--browse
		cvsexportcommit cvsimport cvsserver archimport http-backend upload-pack receive-pack upload-archive shell
		filter-branch filter-repo difftool difftool--helper mergetool for-each-repo maintenance hook remote-ext
		remote-fd credential-cache--daemon fsmonitor--daemon`) {
		m[c] = true
	}
	return m
}()

// longPrefixOf reports a long option `--n[=v]` whose name n is a prefix of one of full: git's
// parse-options accepts unique abbreviations (`git rebase --ex=cmd` is `--exec=cmd`).
func longPrefixOf(a string, full ...string) bool {
	n, ok := strings.CutPrefix(a, "--")
	if !ok || n == "" {
		return false
	}
	n, _, _ = strings.Cut(n, "=")
	return slices.ContainsFunc(full, func(f string) bool { return strings.HasPrefix(f, n) })
}

// gitRunForm returns the form of a known subcommand that runs a command, or "".
func gitRunForm(g gitCall) string {
	var pos []string
	for _, a := range g.args {
		if longPrefixOf(a, "upload-pack", "receive-pack") {
			return g.sub + " --upload-pack/--receive-pack"
		}
		if !strings.HasPrefix(a, "-") {
			pos = append(pos, a)
		}
	}
	switch g.sub {
	case "rebase":
		for _, a := range g.args {
			if longPrefixOf(a, "exec") || (strings.HasPrefix(a, "-") && !strings.HasPrefix(a, "--") && strings.ContainsRune(a, 'x')) {
				return "rebase -x/--exec"
			}
		}
	case "submodule":
		if slices.Contains(pos, "foreach") {
			return "submodule foreach"
		}
	case "bisect":
		if len(pos) > 0 && pos[0] == "run" {
			return "bisect run"
		}
	case "archive":
		for _, a := range g.args {
			if longPrefixOf(a, "exec", "remote") {
				return "archive --exec/--remote"
			}
		}
	case "clone":
		for _, a := range g.args {
			if a == "-u" || (strings.HasPrefix(a, "-") && !strings.HasPrefix(a, "--") && strings.ContainsRune(a, 'u')) {
				return "clone -u"
			}
		}
	case "fetch", "fetch-pack", "ls-remote": // --exec is --upload-pack
		for _, a := range g.args {
			if longPrefixOf(a, "exec") {
				return g.sub + " --exec"
			}
		}
	case "grep": // -O / --open-files-in-pager runs a pager program
		for _, a := range g.args {
			if longPrefixOf(a, "open-files-in-pager") ||
				(strings.HasPrefix(a, "-") && !strings.HasPrefix(a, "--") && strings.ContainsRune(a, 'O')) {
				return "grep -O/--open-files-in-pager"
			}
		}
	}
	switch g.sub {
	case "clone", "init", "init-db": // a template directory brings its own hooks and config
		for _, a := range g.args {
			if longPrefixOf(a, "template") {
				return g.sub + " --template"
			}
		}
	case "merge", "pull", "rebase", "cherry-pick", "revert":
		if s, ok := mergeStrategies(g); !ok {
			return g.sub + " -s/--strategy " + s
		}
	}
	return ""
}

// gitStrategies are git's own merge strategies; any other name runs a `git-merge-<name>` program.
var gitStrategies = map[string]bool{"resolve": true, "recursive": true, "ort": true, "octopus": true, "ours": true, "subtree": true}

// mergeStrategies checks every -s/--strategy value (also abbreviated `--strat=…`, attached `-sX`, in a
// short cluster) of merge/pull/rebase/cherry-pick/revert; it returns the first one that is not one of
// git's own strategies. -X/--strategy-option is a strategy's option and is left alone. cherry-pick and
// revert have -s as --signoff, so only their long form is a strategy.
func mergeStrategies(g gitCall) (string, bool) {
	short := g.sub == "merge" || g.sub == "pull" || g.sub == "rebase"
	for i := 0; i < len(g.args); i++ {
		a := g.args[i]
		var v string
		switch {
		case a == "--":
			return "", true
		case longPrefixOf(a, "strategy"):
			if _, val, ok := strings.Cut(a, "="); ok {
				v = val
			} else if i+1 < len(g.args) {
				i++
				v = g.args[i]
			}
		case short && strings.HasPrefix(a, "-") && !strings.HasPrefix(a, "--") && strings.ContainsRune(a, 's'):
			v = a[strings.IndexByte(a, 's')+1:]
			if v == "" && i+1 < len(g.args) {
				i++
				v = g.args[i]
			}
		default:
			continue
		}
		if !gitStrategies[v] {
			return v, false
		}
	}
	return "", true
}

// cloneConfigs returns the `git clone -c/--config key=value` values (also `--conf=…`, attached `-ckv`,
// in a short cluster): clone writes them into the new repository's config.
func cloneConfigs(g gitCall) []string {
	var out []string
	for i := 0; i < len(g.args); i++ {
		a := g.args[i]
		next := func() string {
			if i+1 < len(g.args) {
				i++
				return g.args[i]
			}
			return ""
		}
		switch {
		case a == "--":
			return out
		case longPrefixOf(a, "config"):
			if _, val, ok := strings.Cut(a, "="); ok {
				out = append(out, val)
			} else {
				out = append(out, next())
			}
		case strings.HasPrefix(a, "-") && !strings.HasPrefix(a, "--"):
			for k := 1; k < len(a); k++ {
				if strings.IndexByte("objc", a[k]) < 0 {
					continue
				}
				v := a[k+1:]
				if v == "" {
					v = next()
				}
				if a[k] == 'c' {
					out = append(out, v)
				}
				break
			}
		}
	}
	return out
}

func gitRunsBlocked(what string) string {
	return "blocked by guard: `git " + what + "` runs commands or git subcommands the guard does not see " +
		"(or is not one of git's own commands: aliases and external git-* programs are not allowed)."
}

// gitConfigSafe are the git config keys a job may set (`git -c`, `git config`): none of them makes
// git run a program. Everything else is rejected (fail closed): core.hooksPath, core.pager,
// core.editor, core.sshCommand, core.fsmonitor, credential.*, diff.external, *.textconv, filter.*,
// alias.*, include.*, url.* … Identity keys are allowed for `git config` only with the configured
// value (checkForeignIdentity); via `-c` they are rejected: the job's identity comes from the
// wrapper's GIT_CONFIG_GLOBAL.
var gitConfigSafe = map[string]bool{
	"user.name": true, "user.email": true, "commit.gpgsign": true, "commit.verbose": true, "commit.cleanup": true,
	"core.autocrlf": true, "core.safecrlf": true, "core.filemode": true, "core.quotepath": true,
	"color.ui": true, "diff.renames": true, "diff.algorithm": true, "diff.context": true, "diff.noprefix": true,
	"merge.conflictstyle": true, "rerere.enabled": true, "init.defaultbranch": true, "log.date": true,
	"status.short": true, "status.showuntrackedfiles": true, "advice.detachedhead": true,
}

func configKey(kv string) string {
	k, _, _ := strings.Cut(kv, "=")
	return strings.ToLower(strings.TrimSpace(k))
}

func checkNothingOut(scripts []*script, _ string, cfg Config) string {
	return eachCmd(scripts, func(_ *simpleCmd, argv []string) string {
		if g, ok := parseGit(argv); ok {
			for _, kv := range g.configs {
				k := configKey(kv)
				if identityField(k) != "" {
					return foreign("-c " + k + "; the job identity is set by the wrapper")
				}
				if !gitConfigSafe[k] {
					return gitConfigBlocked("-c " + k)
				}
			}
			if g.sub == "clone" {
				for _, kv := range cloneConfigs(g) {
					k := configKey(kv)
					_, v, _ := strings.Cut(kv, "=")
					if f := identityField(k); f != "" && !allowedValue(f, v, cfg) {
						return foreign("clone -c " + k)
					}
					if !gitConfigSafe[k] {
						return gitConfigBlocked("clone -c " + k)
					}
				}
			}
			if gitOutside[g.sub] {
				return outside("git " + g.sub)
			}
			if g.sub != "" && (gitRunsCmd[g.sub] || !gitKnown[g.sub]) {
				return gitRunsBlocked(g.sub)
			}
			if f := gitRunForm(g); f != "" {
				return gitRunsBlocked(f)
			}
			if g.sub == "config" {
				if slices.Contains(g.args, "--rename-section") || slices.Contains(g.args, "rename-section") {
					return gitConfigBlocked("config rename-section")
				}
				if k, _, set := gitConfigWrite(g.args); set && !gitConfigSafe[k] {
					return gitConfigBlocked("config " + k)
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
