package guard

import (
	"path/filepath"
	"regexp"
	"slices"
	"strings"
)

// The reader of the merge hook (Wave F, F9 (b), `agent-jobs hook merge-gate`): which `gh pr merge` calls
// a Bash command of the orchestrator session runs, with the words that decide what they merge. It uses
// the guard's shell reader (chains, pipes, subshells, quotes, env prefixes, wrappers, sh -c and eval
// bodies) and the guard's cd rules (cdView); the policy is the hook's.

// GhMerge is one `gh pr merge` call of a command.
type GhMerge struct {
	// Repo is the repository the call names, as written: the PR URL's or the OWNER/REPO#N reference's,
	// else -R / --repo ("" = none: gh takes it from GH_REPO or the directory).
	Repo string
	// GHRepos are the literal GH_REPO values set on the call or earlier in the command (an earlier one
	// counts only if the variable is exported, which the reader cannot tell: the hook checks them all).
	GHRepos []string
	// Dirs are the absolute directories the call may run in: the directory the command starts in (when
	// it is absolute) and, after each cd / pushd before the call, every earlier candidate moved by it. A
	// cd that fails, runs in a subshell or in a branch not taken leaves the shell where it was, so each
	// cd adds to the candidates and removes none: the list holds the real directory whichever way the
	// command runs.
	Dirs []string
	// MatchHead is the last --match-head-commit value; HasMatchHead says whether one was given.
	MatchHead    string
	HasMatchHead bool
	// Auto is --auto (auto-merge: GitHub merges a later head); DisableAuto is --disable-auto (gh only
	// turns auto-merge off and merges nothing).
	Auto        bool
	DisableAuto bool
	// Unsure says why the call cannot be read as written ("" = it can): a variable, substitution, glob,
	// brace or tilde in its words, a call inside sh -c / eval / a substitution, a wrapper option that may
	// move it, a PR argument tied to no one repository, a directory change before it the reader cannot
	// follow.
	Unsure string
}

// ghMergeValueLong / ghMergeValueShort are the `gh pr merge` options that take a value (gh 2.x).
var (
	ghMergeValueLong = map[string]bool{"--author-email": true, "--body": true, "--body-file": true,
		"--match-head-commit": true, "--subject": true, "--repo": true}
	ghMergeValueShort = "AbFtR"
)

// GhPrMerges reads cmd, run in directory cwd, for `gh pr merge` calls. unsure is why the command may run
// a merge that is not in calls ("" = it cannot): a form the guard's reader cannot verify (a program from
// a variable, a gh subcommand from a variable, gh flags it cannot read before the subcommand, a shell
// reading stdin, …), whatever the command's text says (git's own forms, such as git in a substitution,
// never run gh, and a gh call of another group or a literal `gh pr` subcommand other than merge merges
// nothing: neither counts), and a command the reader cannot read whole (longer than its limit, nested
// deeper than its limit). What makes one call unreadable (a cd it cannot follow, GH_REPO from a variable,
// GIT_DIR / GIT_WORK_TREE set) is the call's Unsure.
func GhPrMerges(cmd, cwd string) (calls []GhMerge, unsure string) {
	if len(cmd) > maxCommandBytes {
		return nil, "the command is longer than the reader's limit"
	}
	scripts := parseAll(cmd)
	for _, sc := range scripts {
		if sc.depth >= maxNesting {
			return nil, "the command nests deeper than the reader's limit"
		}
	}
	var ghRepos []string
	// Why every later call of the command cannot be read: dirPending where it runs or which GH_REPO it
	// takes (a call that names its repository, -R or a PR URL, does not depend on it), shellPending what
	// `gh` itself runs.
	dirPending, shellPending := "", ""
	later := func(why string) {
		if dirPending == "" {
			dirPending = why
		}
	}
	dirs := newMergeDirs(cmd, cwd)
	dirWhy, shellWhy := sameShellChanges(scripts)
	later(dirWhy)
	shellPending = shellWhy
	for _, sc := range scripts {
		for _, pl := range sc.pipelines {
			for _, c := range pl.cmds {
				v := split(c.words)
				argv, dyn := c.words[v.start:], c.dyn[v.start:]
				git := len(argv) > 0 && !dyn[0] && isGitName(progName(argv[0]))
				// Merges nothing itself: a gh call of another group or a literal gh pr subcommand other than
				// merge, or a shell running literal -c code (read as a nested script, where a gh call counts).
				quiet := (ghOtherGroup(argv, dyn) || ghPrNotMerge(argv, dyn) || literalShellCode(c, argv, dyn)) &&
					!v.splitString && v.wrapperUnknown == "" && v.envUnknown == ""
				if r := unverifiable(sc, c); r != "" && unsure == "" && !git && !quiet {
					unsure = cannotForm(r)
				}
				if g, ok := ghGroup(argv, dyn); ok && !ghGroups[g] && unsure == "" {
					unsure = "gh " + g + " is no built-in gh command (an alias or an extension, which may run gh pr merge)"
				}
				if sc.depth == 0 && git {
					if why := gitMovesRepo(argv); why != "" {
						later(why)
					}
				}
				if len(argv) == 0 {
					// `GH_REPO=o/r` alone sets it for the rest of the command (if exported already).
					if sc.depth == 0 {
						ghRepos = append(ghRepos, envAssigned(c.words, c.dyn, 0, len(c.words), later)...)
					}
					continue
				}
				switch name := progName(argv[0]); {
				case sc.depth == 0 && cdName(name):
					if why := dirs.cd(argv, dyn); why != "" {
						later(why)
					}
				case sc.depth == 0 && (name == "source" || name == "."):
					if shellPending == "" {
						shellPending = "source or . runs a file in this shell, which may change its directory, GH_REPO or what gh runs"
					}
				case sc.depth == 0 && (name == "export" || name == "declare" || name == "typeset" || name == "readonly" || name == "local"):
					ghRepos = append(ghRepos, envAssigned(argv, dyn, 1, len(argv), later)...)
				case sc.depth == 0 && name != "gh" && name != "unset" && slices.ContainsFunc(argv[1:], namesRepoEnv):
					later("GH_REPO, GIT_DIR, GIT_WORK_TREE or GIT_CONFIG set by " + name + ", which the reader does not follow")
				case name == "gh":
					call, ok := ghMergeCall(sc, c, v)
					if !ok {
						continue
					}
					switch {
					case call.Unsure != "":
					case shellPending != "":
						call.Unsure = shellPending
					case call.Repo == "":
						call.Unsure = dirPending
					}
					call.Dirs = slices.Clone(dirs.dirs)
					call.GHRepos = append(slices.Clone(ghRepos), call.GHRepos...)
					calls = append(calls, call)
				}
			}
		}
	}
	return calls, unsure
}

// literalShellCode reports a shell running -c code whose words are all literal: the reader parsed the
// code as a nested script (c.nested), so what it runs is read there; the extra words become $0, $1, …,
// which the nested script can only reach through a variable (unverifiable there).
func literalShellCode(c *simpleCmd, argv []string, dyn []bool) bool {
	if len(argv) == 0 || len(c.nested) == 0 || slices.Contains(dyn, true) || !shells[progName(argv[0])] {
		return false
	}
	code, ok := shellCode(argv)
	// `bash -c -e 'code'`: the shell reads options after -c too, and the code is a later word.
	return ok && !strings.HasPrefix(code, "-") && !strings.HasPrefix(code, "+")
}

// ghGroups are gh's built-in commands (gh 2.45 `gh help`, and later ones); an alias or extension cannot
// take their names.
var ghGroups = map[string]bool{"alias": true, "api": true, "at": true, "attestation": true, "auth": true, "browse": true,
	"cache": true, "co": true, "codespace": true, "completion": true, "config": true, "cs": true, "ext": true, "extension": true,
	"extensions": true, "gist": true, "gpg-key": true, "help": true, "issue": true, "label": true, "org": true, "pr": true,
	"preview": true, "project": true, "release": true, "repo": true, "rs": true, "ruleset": true, "run": true, "search": true,
	"secret": true, "ssh-key": true, "status": true, "variable": true, "version": true, "workflow": true}

// ghGroup is the literal first positional word of a gh call (ok false when there is none, it is not
// literal, or a flag before it may take a value).
func ghGroup(argv []string, dyn []bool) (string, bool) {
	if len(argv) == 0 || dyn[0] || !isProg(argv[0], "gh") {
		return "", false
	}
	pos, unknown := ghPositional(argv)
	if unknown != "" || len(pos) == 0 || dyn[pos[0]] {
		return "", false
	}
	return argv[pos[0]], true
}

// repoEnvNames are the variables that move gh to another repository or change what git reports as its
// remotes; HOME, XDG_CONFIG_HOME and GH_CONFIG_DIR change the configuration gh and git read (aliases,
// url.*.insteadOf).
var repoEnvNames = []string{"GH_REPO", "GIT_DIR", "GIT_WORK_TREE", "GIT_CONFIG", "GIT_CEILING_DIRECTORIES",
	"GIT_DISCOVERY_ACROSS_FILESYSTEM", "GH_CONFIG_DIR", "XDG_CONFIG_HOME"}

// namesRepoEnv reports a word naming one of repoEnvNames other than as an expansion ($GH_REPO,
// ${GH_REPO}): `printf -v GH_REPO`, `read GH_REPO`.
func namesRepoEnv(w string) bool {
	for _, n := range repoEnvNames {
		rest := strings.ReplaceAll(strings.ReplaceAll(w, "${"+n, ""), "$"+n, "")
		if strings.Contains(rest, n) {
			return true
		}
	}
	return false
}

// gitMovesRepo is why a git command before a gh pr merge may change the repository gh takes ("" = it
// cannot): a clone, init or worktree, a change of the remotes, any git config call.
func gitMovesRepo(argv []string) string {
	g, _ := parseGit(argv)
	if g.subIdx >= len(argv) {
		return ""
	}
	sub, next := argv[g.subIdx], ""
	if g.subIdx+1 < len(argv) {
		next = argv[g.subIdx+1]
	}
	switch {
	case sub == "clone" || sub == "init" || sub == "worktree" || sub == "config" || sub == "submodule":
	case sub == "remote" && slices.Contains([]string{"add", "rename", "remove", "rm", "set-url", "set-head"}, next):
	default:
		return ""
	}
	return "git " + sub + " in the command may change the repository or remotes gh takes"
}

// cdName is a command that changes the shell's directory (chdir as the guard's cdView counts it).
func cdName(name string) bool {
	return name == "cd" || name == "pushd" || name == "popd" || name == "chdir"
}

// sameShellChanges is why the directory or GH_REPO of a gh pr merge call may not follow from the
// command's text in order ("" = it does): an alias (its text runs where it is used), a cd, source or
// GH_REPO / GIT_DIR / GIT_WORK_TREE inside an eval or trap body (they run in the same shell, a trap at any
// later point), or a cd or source in a command with a loop or a function (reFuncDef; its body may run
// again, after the call's text, or before it more than once).
func sameShellChanges(scripts []*script) (dirWhy, shellWhy string) {
	if len(scripts) == 0 {
		return "", ""
	}
	moves, repeats := false, false
	for _, sc := range scripts {
		if reFuncDef.MatchString(sc.src) {
			repeats = true
		}
		for _, pl := range sc.pipelines {
			for _, c := range pl.cmds {
				v := split(c.words)
				for _, w := range c.words[:v.lead] {
					if w == "while" || w == "until" || w == "do" {
						repeats = true
					}
				}
				if slices.ContainsFunc(c.words, namesRepoEnv) {
					moves = true
				}
				if argv := c.words[v.start:]; len(argv) > 0 {
					switch name := progName(argv[0]); {
					case name == "for" || name == "select" || name == "function":
						repeats = true
					case cdName(name) || name == "source" || name == ".":
						moves = true
					case name == "alias":
						shellWhy = "an alias, whose text runs where it is used"
					}
				}
			}
		}
	}
	if moves && repeats {
		dirWhy = "a cd, source or GH_REPO / GIT_* change in a command with a loop or a function, which may run it more " +
			"than once or before a call written earlier"
		return dirWhy, shellWhy
	}
	var walk func(sc *script, top bool) string
	walk = func(sc *script, top bool) string {
		if !top && namesRepoEnv(sc.src) {
			return "GH_REPO, GIT_DIR, GIT_WORK_TREE or GIT_CONFIG inside an eval or trap body"
		}
		for _, pl := range sc.pipelines {
			for _, c := range pl.cmds {
				argv := argvOf(c.words)
				if len(argv) == 0 {
					continue
				}
				name := progName(argv[0])
				if !top && (cdName(name) || name == "source" || name == ".") {
					return "a cd or source inside an eval or trap body"
				}
				if name != "eval" && name != "trap" {
					continue
				}
				for _, child := range c.nested {
					if why := walk(child, false); why != "" {
						return why
					}
				}
			}
		}
		return ""
	}
	return walk(scripts[0], true), shellWhy
}

// mergeDirs are the candidate directories of the shell while a command is read (GhMerge.Dirs), with the
// guard's rules for a cd target (cdView: HOME, CDPATH and cdable_vars, `cd -`, the directory stack).
type mergeDirs struct {
	view   cdView
	cdpath bool
	cwd    bool // the start directory is absolute (else a relative cd cannot be followed)
	dirs   []string
}

func newMergeDirs(cmd, cwd string) *mergeDirs {
	home, cdpath := cdEnv(cmd)
	d := &mergeDirs{view: cdView{home: home}, cdpath: cdpath, cwd: filepath.IsAbs(cwd)}
	if d.cwd {
		d.dirs = []string{filepath.Clean(cwd)}
	}
	return d
}

// cd adds the directories cd / pushd / popd argv moves each candidate to; why is set when the target
// cannot be followed exactly (then the candidates are no longer known).
func (d *mergeDirs) cd(argv []string, dyn []bool) (why string) {
	if i := slices.Index(argv, "--"); i > 0 && i+1 < len(argv) && strings.HasPrefix(argv[i+1], "-") {
		return "a cd to a directory whose name starts with -"
	}
	t, ok := d.view.cdTarget(argv, dyn, d.cdpath)
	switch {
	case !ok:
		return "a cd the reader cannot follow (a variable or substitution, cd -, popd, pushd without a directory, " +
			"HOME set in the command, CDPATH or cdable_vars with a bare relative target, ~user)"
	case hasGlob(t) || strings.HasPrefix(t, "~"):
		return "a glob, brace or tilde in a cd target: " + t
	case slices.Contains(argv[1:], "-P") && slices.Contains(strings.Split(t, "/"), ".."):
		return "cd -P with .. (a physical parent the reader does not resolve)"
	case !filepath.IsAbs(t) && !d.cwd:
		return "a relative cd without an absolute start directory"
	}
	if filepath.IsAbs(t) && len(d.dirs) == 0 {
		d.dirs = []string{filepath.Clean(t)}
		return ""
	}
	for _, from := range slices.Clone(d.dirs) {
		to := filepath.Clean(t)
		if !filepath.IsAbs(t) {
			to = filepath.Join(from, t)
		}
		if slices.Contains(d.dirs, to) {
			continue
		}
		if len(d.dirs) >= maxCdDirs {
			return "more cd candidates than the reader's limit"
		}
		d.dirs = append(d.dirs, to)
	}
	return ""
}

// envAssigned is the literal GH_REPO values among the name=value words of words[from:to]; unsure is told
// about a GH_REPO from a variable or substitution or with a glob, brace or tilde, and about GIT_DIR /
// GIT_WORK_TREE (gh then takes the repository from them, which the reader does not follow).
func envAssigned(words []string, dyn []bool, from, to int, unsure func(string)) (repos []string) {
	for i := from; i < to; i++ {
		w := words[i]
		switch {
		case strings.HasPrefix(w, "GH_REPO=") && dyn[i]:
			unsure("GH_REPO set from a variable or substitution")
		case strings.HasPrefix(w, "GH_REPO=") && shellExpands(strings.TrimPrefix(w, "GH_REPO=")):
			unsure("a glob, brace or tilde in GH_REPO: " + w)
		case strings.HasPrefix(w, "GH_REPO="):
			repos = append(repos, strings.TrimPrefix(w, "GH_REPO="))
		case strings.HasPrefix(w, "GIT_DIR=") || strings.HasPrefix(w, "GIT_WORK_TREE="):
			unsure("GIT_DIR or GIT_WORK_TREE set, which moves gh to another repository")
		case reAssign.MatchString(w) && (strings.HasPrefix(w, "HOME=") || namesRepoEnv(strings.SplitN(w, "=", 2)[0])):
			unsure("HOME, GIT_CONFIG*, GH_CONFIG_DIR or XDG_CONFIG_HOME set, which changes the configuration or the " +
				"repository gh and git read")
		}
	}
	return repos
}

// shellExpands reports a word the shell may change before the program sees it beyond what the reader
// models: a glob or brace character, or a tilde at its start or after = or :.
func shellExpands(w string) bool {
	return hasGlob(w) || strings.HasPrefix(w, "~") || strings.Contains(w, "=~") || strings.Contains(w, ":~")
}

// ghMergeCall reads a gh command as a `gh pr merge` call (ok false for any other gh call).
func ghMergeCall(sc *script, c *simpleCmd, v cmdView) (GhMerge, bool) {
	argv, dyn := c.words[v.start:], c.dyn[v.start:]
	var call GhMerge
	pos, unknown := ghPositional(argv)
	if unknown != "" {
		// A flag before the subcommand that may take a value: which words are `pr merge` is unknown.
		if !slices.Contains(argv, "merge") || ghOtherGroup(argv, dyn) {
			return call, false
		}
		call.Unsure = "gh flag " + unknown + " before the subcommand"
		return call, true
	}
	if len(pos) < 2 || argv[pos[0]] != "pr" || argv[pos[1]] != "merge" {
		return call, false
	}
	switch {
	case slices.Contains(dyn, true):
		call.Unsure = "a variable or substitution in the words of gh pr merge"
	case slices.ContainsFunc(argv[1:], shellExpands):
		call.Unsure = "a glob, brace or tilde in the words of gh pr merge (the shell may expand it into other words)"
	case sc.depth > 0:
		call.Unsure = "gh pr merge inside a substitution, eval or sh -c"
	case c.stdin || slices.Contains(v.wrappers, "xargs"):
		call.Unsure = "gh pr merge with arguments or input the reader cannot see"
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
	flagRepo := ""
	// -R / --repo before the subcommand (ghPositional skipped them).
	for i := 1; i < pos[1]; i++ {
		switch a := argv[i]; {
		case (a == "-R" || a == "--repo") && i+1 < len(argv):
			flagRepo = argv[i+1]
			i++
		case strings.HasPrefix(a, "--repo="):
			flagRepo = strings.TrimPrefix(a, "--repo=")
		case strings.HasPrefix(a, "-R") && len(a) > 2:
			flagRepo = strings.TrimPrefix(strings.TrimPrefix(a, "-R"), "=")
		}
	}
	var positional []string
	set := func(name, value string, hasValue bool) {
		switch name {
		case "--match-head-commit":
			call.MatchHead, call.HasMatchHead = value, true
		case "--repo", "-R":
			flagRepo = value
		case "--auto":
			call.Auto = !hasValue || boolTrue(value)
		case "--disable-auto":
			call.DisableAuto = !hasValue || boolTrue(value)
		}
	}
	for i := pos[1] + 1; i < len(argv); i++ {
		a := argv[i]
		switch {
		case a == "--":
			positional = append(positional, argv[i+1:]...)
			i = len(argv)
		case strings.HasPrefix(a, "--"):
			name, value, eq := strings.Cut(a, "=")
			if ghMergeValueLong[name] && !eq && i+1 < len(argv) {
				i++
				value, eq = argv[i], true
			}
			set(name, value, eq)
		case strings.HasPrefix(a, "-") && len(a) > 1:
			for k := 1; k < len(a); k++ {
				if strings.IndexByte(ghMergeValueShort, a[k]) < 0 {
					continue // a boolean shorthand: -d, -m, -r, -s
				}
				value := strings.TrimPrefix(a[k+1:], "=")
				if value == "" && i+1 < len(argv) {
					i++
					value = argv[i]
				}
				set("-"+a[k:k+1], value, true)
				break
			}
		default:
			positional = append(positional, a)
		}
	}
	if flagRepo != "" {
		call.Repo = flagRepo
	}
	if len(positional) > 0 {
		repo, why := prArgRepo(positional[0])
		if repo != "" {
			call.Repo = repo
		}
		if why != "" && call.Unsure == "" {
			call.Unsure = why
		}
	}
	return call, true
}

var (
	// rePRNumber is a pull request number as gh takes it (`5`, `#5`).
	rePRNumber = regexp.MustCompile(`^#?[0-9]+$`)
	// rePRRef is an OWNER/REPO#N reference.
	rePRRef = regexp.MustCompile(`^([^/#\s]+/[^/#\s]+)#[0-9]+$`)
)

// prArgRepo is the repository the PR argument of gh pr merge names ("" = the one gh takes from -R,
// GH_REPO or the directory: a number or a branch name); why is set when the argument cannot be tied to
// one repository: an OWNER/REPO#N reference (gh may read it as a reference or as a branch name, and -R
// may name another repository), any other argument with #, a URL that is no pull request URL.
func prArgRepo(arg string) (repo, why string) {
	switch {
	case rePRNumber.MatchString(arg):
		return "", ""
	case strings.Contains(arg, "://"):
		if r := prURLRepo(arg); r != "" {
			return r, ""
		}
		return "", "the PR argument " + arg + " is a URL but no pull request URL"
	case rePRRef.MatchString(arg):
		return "", "the PR argument " + arg + " is an OWNER/REPO#N reference, which the reader cannot tie to one repository " +
			"(use the number with -R OWNER/REPO)"
	case strings.Contains(arg, "#"):
		return "", "the PR argument " + arg + " is neither a number, a branch name nor a pull request URL"
	}
	return "", "" // a branch name of the repository gh takes from -R, GH_REPO or the directory
}

// ghOtherGroup reports a gh call whose first positional word is a literal group other than pr (`gh api
// -X PUT …/merge`, `gh repo view`): it is no `gh pr merge`, whatever follows.
func ghOtherGroup(argv []string, dyn []bool) bool {
	if len(argv) == 0 || dyn[0] || !isProg(argv[0], "gh") {
		return false
	}
	for i := 1; i < len(argv); i++ {
		switch a := argv[i]; {
		case a == "-R" || a == "--repo" || a == "--hostname":
			i++
		case strings.HasPrefix(a, "-"):
			return false // a flag that may take a value: the group is not known
		default:
			return !dyn[i] && !hasGlob(a) && a != "pr"
		}
	}
	return false
}

// ghPrNotMerge reports a gh call whose words are all literal and whose group is pr with a literal
// subcommand other than merge (`gh pr view 5 --json mergeable`): it merges nothing, wherever it is nested
// (a substitution, sh -c, eval).
func ghPrNotMerge(argv []string, dyn []bool) bool {
	if len(argv) == 0 || slices.Contains(dyn, true) || !isProg(argv[0], "gh") {
		return false
	}
	pos, unknown := ghPositional(argv)
	if unknown != "" || len(pos) < 2 {
		return false
	}
	group, sub := argv[pos[0]], argv[pos[1]]
	return group == "pr" && sub != "merge" && !hasGlob(sub)
}

// cannotForm is the form named by a cannot() reason, without the guard's wording around it.
func cannotForm(r string) string {
	const head = "blocked by guard: guard cannot verify this form ("
	if rest, ok := strings.CutPrefix(r, head); ok {
		if i := strings.LastIndex(rest, "); write it as a plain command"); i >= 0 {
			return rest[:i]
		}
	}
	return r
}

// boolTrue is a true value of a gh (pflag) boolean flag.
func boolTrue(s string) bool {
	switch s {
	case "1", "t", "T", "true", "TRUE", "True":
		return true
	}
	return false
}

// prURLRepo is HOST/OWNER/REPO of a pull request URL (https://HOST/OWNER/REPO/pull/N…), "" otherwise.
func prURLRepo(s string) string {
	scheme, rest, ok := strings.Cut(s, "://")
	if !ok || !strings.EqualFold(scheme, "https") && !strings.EqualFold(scheme, "http") {
		return ""
	}
	parts := strings.Split(rest, "/")
	if len(parts) < 5 || parts[3] != "pull" || parts[0] == "" || parts[1] == "" || parts[2] == "" {
		return ""
	}
	return parts[0] + "/" + parts[1] + "/" + parts[2]
}
