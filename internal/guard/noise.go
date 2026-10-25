package guard

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
)

// Relaxations of false refusals measured in docs/BENCHMARK.md («Guard refusals»): forms the guard
// refused although they reach nothing a script file with the same text would not reach. Each one is
// narrow: the surrounding checks still run on everything else the command does.

// gitSubstReads are git subcommands that only print repository data: inside a command substitution
// (`n=$(git rev-list --count HEAD)`) they are checked like the same call written as a plain command.
var gitSubstReads = map[string]bool{
	"log": true, "diff": true, "rev-list": true, "show": true, "rev-parse": true, "status": true,
	"ls-files": true, "ls-tree": true, "merge-base": true, "describe": true, "shortlog": true,
	"blame": true, "diff-tree": true,
}

// substGitRead reports a read-only git call whose script is reached from the command through command
// or process substitutions only (no eval, `sh -c` or runner body), with no `-c` configuration: the
// substitution prints what the plain call prints, and every other rule still checks the nested call.
func substGitRead(sc *script, argv []string) bool {
	if !sc.subst || len(argv) == 0 || progName(argv[0]) != "git" {
		return false
	}
	g, _ := parseGit(argv)
	if _, writes := gitOutputs(g.args); writes {
		return false
	}
	return len(g.configs) == 0 && g.attached == "" && g.subIdx < len(argv) && gitSubstReads[g.sub]
}

// gitOutputs returns the values of git's --output option among the subcommand's args, in any
// abbreviation parse-options accepts (`--outp=f`, `--output f`), up to `--`: git diff, log, show and
// the other diff-family commands then write to that file instead of stdout. found also reports an
// option with no value word after it.
func gitOutputs(args []string) (vals []string, found bool) {
	for k := 0; k < len(args); k++ {
		a := args[k]
		if a == "--" {
			break
		}
		if !longPrefixOf(a, "output") {
			continue
		}
		found = true
		if _, v, ok := strings.Cut(a, "="); ok {
			vals = append(vals, v)
		} else if k+1 < len(args) {
			k++
			vals = append(vals, args[k])
		}
	}
	return vals, found
}

// dataSinks copy their standard input to standard output or to the files they name: a here-doc they
// read is file content, not code.
var dataSinks = map[string]bool{"cat": true, "tee": true}

// dataHeredoc reports a here-doc that is only written out as data (`cat > notes.md <<'EOF'`): the
// command is cat or tee with no wrapper, alone in its pipeline at the top level (nothing reads its
// output as code), with no substitution, process substitution or expanding redirection, and every
// here-doc of it literal (quoted delimiter, or a body without $ and backticks) and no here-string.
// Its body is then not scanned for paths: mentioning a path in a file's text does not open it.
func dataHeredoc(sc *script, pl *pipeline, c *simpleCmd) bool {
	if sc.depth != 0 || c.heredoc == "" || c.expandingStdin || len(pl.cmds) != 1 {
		return false
	}
	if len(c.feeds) > 0 || len(c.incoming) > 0 || len(c.subSources) > 0 || len(c.inputSources) > 0 ||
		slices.Contains(c.outDyn, true) || slices.Contains(c.dyn, true) {
		return false
	}
	v := split(c.words)
	if len(v.wrappers) > 0 || v.start >= len(c.words) {
		return false
	}
	return dataSinks[progName(c.words[v.start])]
}

// codeGlobSeps split inline code into the pieces a language may pass to its glob (perl glob() and
// <…>, python glob.glob, node fs.globSync): quotes, white space, brackets of calls and operators.
// Glob characters (`*?[]{}`) and `,` (a brace list) stay inside a piece.
const codeGlobSeps = " \t\r\n'\"`();=<>|&+"

// gitAdminNames are the entries of a git dir that run programs (config, hooks, a worktree's admin dir).
var gitAdminNames = []string{"config", "hooks", "worktrees"}

// maxBraceAlternatives bounds the brace expansion of one piece: past it the piece is refused.
const maxBraceAlternatives = 64

// codeGlobReach returns the glob pattern in inline code (one quoted word: the shell does not glob
// it, the language may) that can name a path a job never writes or reads: git's config or hooks
// (`.gi?/config`, `.g*t/hooks/*`, `*/hooks`, `.{g,x}it/config`), one of the job's git dirs, a
// protected path, a no-read dir, or the agent state outside the job's own folder. Relative patterns
// are resolved against the call's directory, `~/` against $HOME; a wildcard-only component may match
// a dot name (pathlib and include_hidden do). Pieces made of `*` only are operators (`a * b`, `2 ** 8`).
// A glob built at run time is not seen (the residual gap of inline code, checkProtected).
func codeGlobReach(code string, cfg Config) string {
	for _, piece := range strings.FieldsFunc(code, func(r rune) bool { return strings.ContainsRune(codeGlobSeps, r) }) {
		if !hasGlob(piece) || strings.Trim(piece, "*") == "" {
			continue
		}
		alts, ok := braceAlternatives(piece)
		if !ok {
			return piece
		}
		for _, alt := range alts {
			for _, pat := range strings.Split(alt, ",") {
				if pat != "" && strings.Trim(pat, "*") != "" && (gitAdminPattern(pat) || stateGlob(pat, cfg)) {
					return piece
				}
			}
		}
	}
	return ""
}

// braceAlternatives expands the brace lists of a pattern (`.{g,x}it` gives `.git` and `.xit`), as
// perl's glob does; a brace without a top-level comma is kept. ok is false past maxBraceAlternatives.
func braceAlternatives(pat string) ([]string, bool) {
	out, todo := []string(nil), []string{pat}
	for len(todo) > 0 {
		p := todo[len(todo)-1]
		todo = todo[:len(todo)-1]
		open, close, commas := braceList(p)
		if open < 0 {
			out = append(out, p)
		} else {
			for k := 0; k+1 < len(commas); k++ {
				todo = append(todo, p[:open]+p[commas[k]+1:commas[k+1]]+p[close+1:])
			}
		}
		if len(out)+len(todo) > maxBraceAlternatives {
			return nil, false
		}
	}
	return out, true
}

// braceList finds the first `{…}` of p with a comma at its top level: the indices of its braces and
// the separators (open, each top-level comma, close).
func braceList(p string) (open, close int, seps []int) {
	for i := 0; i < len(p); i++ {
		if p[i] != '{' {
			continue
		}
		depth, sp := 0, []int{i}
		for j := i; j < len(p); j++ {
			switch p[j] {
			case '{':
				depth++
			case '}':
				depth--
			case ',':
				if depth == 1 {
					sp = append(sp, j)
				}
			}
			if depth == 0 {
				if len(sp) > 1 {
					return i, j, append(sp, j)
				}
				break
			}
		}
	}
	return -1, -1, nil
}

// globMatch reports a pattern component matching a name, case-folded (a case-insensitive volume
// opens `.GIT/config` too); a malformed pattern matches nothing (the language rejects it as well).
func globMatch(pat, name string) bool {
	ok, err := filepath.Match(strings.ToLower(pat), strings.ToLower(name))
	return err == nil && ok
}

// gitAdminPattern reports a pattern that can name a path inside a `.git` dir that runs programs: a
// component spelled for .git (`.gi?`, `.g*t`, `.[g]it`), a glob component spelled for config, hooks
// or worktrees (`hoo*`), or a glob component or one matching .git (`*`, `.*`, `i?` of a string
// concatenated in the code) followed by a component matching one of them (`*/config`).
func gitAdminPattern(pat string) bool {
	var comps []string
	for _, c := range strings.Split(pat, "/") {
		if c != "" && c != "." {
			comps = append(comps, c)
		}
	}
	admin := func(c string) bool {
		return slices.ContainsFunc(gitAdminNames, func(n string) bool { return globMatch(c, n) })
	}
	for i, c := range comps {
		dotGit := globMatch(c, ".git")
		switch {
		case dotGit && strings.Trim(c, ".*?") != "":
			return true
		case hasGlob(c) && strings.Trim(c, "*?") != "" && admin(c):
			return true
		case (dotGit || hasGlob(c)) && i+1 < len(comps) && admin(comps[i+1]):
			return true
		}
	}
	return false
}

// stateGlob reports a pattern that can name one of the job's git dirs, a protected path, a no-read
// dir, or a path in the agent state outside the job's own folder (its literal part is not inside it).
func stateGlob(pat string, cfg Config) bool {
	switch {
	case strings.HasPrefix(pat, "~/"):
		pat = filepath.Join(os.Getenv("HOME"), pat[2:])
	case filepath.IsAbs(pat):
	case cfg.Dir != "":
		pat = filepath.Join(cfg.Dir, pat)
	default:
		return false
	}
	pat = filepath.Clean(pat)
	for _, t := range slices.Concat(cfg.GitDirs, cfg.Protected, cfg.NoRead) {
		if t != "" && globReaches(pat, t) {
			return true
		}
	}
	if cfg.StateDir == "" || !globReaches(pat, cfg.StateDir) {
		return false
	}
	return cfg.OwnDir == "" || !within(literalPrefix(pat), cfg.OwnDir)
}

// globReaches reports an absolute pattern some match of which is the path t or lies below it: each
// component of t is matched by the pattern's, and `**` reaches whatever is below.
func globReaches(pat, t string) bool {
	ps := strings.Split(strings.TrimPrefix(pat, "/"), "/")
	for i, c := range strings.Split(strings.TrimPrefix(filepath.Clean(t), "/"), "/") {
		if i >= len(ps) {
			return false
		}
		if strings.Contains(ps[i], "**") {
			return true
		}
		if !globMatch(ps[i], c) {
			return false
		}
	}
	return true
}

// literalPrefix is the directory a cleaned absolute pattern names before its first glob component.
func literalPrefix(pat string) string {
	ps := strings.Split(pat, "/")
	for i, c := range ps {
		if hasGlob(c) {
			return strings.Join(ps[:i], "/")
		}
	}
	return pat
}
