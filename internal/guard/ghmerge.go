package guard

import (
	"slices"
	"strings"
)

// The reader of the merge hook (Wave F, F9 (b), `agent-jobs hook merge-gate`): which `gh pr merge` calls
// a Bash command of the orchestrator session runs, with the words that decide what they merge. It uses
// the guard's shell reader (chains, pipes, subshells, quotes, env prefixes, wrappers, sh -c and eval
// bodies); the policy is the hook's.

// GhMerge is one `gh pr merge` call of a command.
type GhMerge struct {
	// Repo is the repository the call names, as written: the PR URL's, else -R / --repo ("" = none: gh
	// takes it from GH_REPO or the directory).
	Repo string
	// GHRepos are the literal GH_REPO values set on the call or earlier in the command (an earlier one
	// counts only if the variable is exported, which the reader cannot tell: the hook checks them all).
	GHRepos []string
	// Dirs are the literal `cd` / `pushd` targets before the call, in order, as written ("" = a bare cd,
	// the home directory); the directory the command starts in is not listed.
	Dirs []string
	// MatchHead is the last --match-head-commit value; HasMatchHead says whether one was given.
	MatchHead    string
	HasMatchHead bool
	// Auto is --auto (auto-merge: GitHub merges a later head); DisableAuto is --disable-auto (gh only
	// turns auto-merge off and merges nothing).
	Auto        bool
	DisableAuto bool
	// Unsure says why the call's words cannot be read as written ("" = they can): a variable or
	// substitution in them, a call inside sh -c / eval / a substitution, a wrapper option that may move it.
	Unsure string
}

// ghMergeValueLong / ghMergeValueShort are the `gh pr merge` options that take a value (gh 2.x).
var (
	ghMergeValueLong = map[string]bool{"--author-email": true, "--body": true, "--body-file": true,
		"--match-head-commit": true, "--subject": true, "--repo": true}
	ghMergeValueShort = "AbFtR"
)

// GhPrMerges reads cmd for `gh pr merge` calls. unsure is why the command may run a merge that is not in
// calls ("" = it cannot): a form the guard's reader cannot verify (a program from a variable, a gh
// subcommand from a variable, gh flags it cannot read before the subcommand, a shell reading stdin, …) in
// a command that runs gh or mentions merge (git's own forms, such as git in a substitution, never run gh
// and do not count), or a command too long to read that mentions merge. What makes one call unreadable
// (a cd to a variable, GH_REPO from a variable, GIT_DIR / GIT_WORK_TREE set) is the call's Unsure.
func GhPrMerges(cmd string) (calls []GhMerge, unsure string) {
	mentions := strings.Contains(strings.ToLower(cmd), "merge")
	if len(cmd) > maxCommandBytes {
		if mentions {
			return nil, "the command is longer than the reader's limit"
		}
		return nil, ""
	}
	var dirs, ghRepos []string
	pending := "" // why every later call of the command cannot be read
	later := func(why string) {
		if pending == "" {
			pending = why
		}
	}
	for _, sc := range parseAll(cmd) {
		for _, pl := range sc.pipelines {
			for _, c := range pl.cmds {
				v := split(c.words)
				argv, dyn := c.words[v.start:], c.dyn[v.start:]
				gh := len(argv) > 0 && (dyn[0] || progName(argv[0]) == "gh")
				git := len(argv) > 0 && !dyn[0] && isGitName(progName(argv[0]))
				if r := unverifiable(sc, c); r != "" && unsure == "" && (mentions || gh) && !git && !ghOtherGroup(argv, dyn) {
					unsure = cannotForm(r)
				}
				if len(argv) == 0 {
					// `GH_REPO=o/r` alone sets it for the rest of the command (if exported already).
					if sc.depth == 0 {
						ghRepos = append(ghRepos, envAssigned(c.words, c.dyn, 0, len(c.words), later)...)
					}
					continue
				}
				switch name := progName(argv[0]); {
				case sc.depth == 0 && (name == "cd" || name == "pushd"):
					if d, why := cdTarget(argv, dyn); why != "" {
						later(why)
					} else {
						dirs = append(dirs, d)
					}
				case sc.depth == 0 && (name == "export" || name == "declare" || name == "typeset"):
					ghRepos = append(ghRepos, envAssigned(argv, dyn, 1, len(argv), later)...)
				case name == "gh":
					call, ok := ghMergeCall(sc, c, v)
					if !ok {
						continue
					}
					if call.Unsure == "" {
						call.Unsure = pending
					}
					call.Dirs = slices.Clone(dirs)
					call.GHRepos = append(slices.Clone(ghRepos), call.GHRepos...)
					calls = append(calls, call)
				}
			}
		}
	}
	return calls, unsure
}

// envAssigned is the literal GH_REPO values among the name=value words of words[from:to]; unsure is told
// about a GH_REPO from a variable or substitution and about GIT_DIR / GIT_WORK_TREE (gh then takes the
// repository from them, which the reader does not follow).
func envAssigned(words []string, dyn []bool, from, to int, unsure func(string)) (repos []string) {
	for i := from; i < to; i++ {
		w := words[i]
		switch {
		case strings.HasPrefix(w, "GH_REPO=") && dyn[i]:
			unsure("GH_REPO set from a variable or substitution")
		case strings.HasPrefix(w, "GH_REPO="):
			repos = append(repos, strings.TrimPrefix(w, "GH_REPO="))
		case strings.HasPrefix(w, "GIT_DIR=") || strings.HasPrefix(w, "GIT_WORK_TREE="):
			unsure("GIT_DIR or GIT_WORK_TREE set, which moves gh to another repository")
		}
	}
	return repos
}

// cdTarget is the directory `cd` / `pushd` argv moves to, as written ("" = home); why is set when it
// cannot be read (a variable, `cd -`, a pushd stack index).
func cdTarget(argv []string, dyn []bool) (dir, why string) {
	for i := 1; i < len(argv); i++ {
		a := argv[i]
		if dyn[i] {
			return "", "cd to a variable or substitution"
		}
		switch {
		case a == "--":
			if i+1 < len(argv) {
				if dyn[i+1] {
					return "", "cd to a variable or substitution"
				}
				return argv[i+1], ""
			}
			return "", ""
		case a == "-" || (len(a) > 1 && (a[0] == '+' || a[0] == '-') && isDigits(a[1:])):
			return "", "cd to the previous directory or a directory stack entry"
		case strings.HasPrefix(a, "-"):
			continue // -L, -P, -e, -@, pushd -n
		}
		return a, ""
	}
	return "", ""
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
		if r := prURLRepo(positional[0]); r != "" {
			call.Repo = r
		}
	}
	return call, true
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
			return !dyn[i] && a != "pr"
		}
	}
	return false
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
