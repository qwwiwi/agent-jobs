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
	if _, writes := gitWriteTargets(argv, g); writes {
		return false
	}
	return len(g.configs) == 0 && g.attached == "" && g.subIdx < len(argv) && gitSubstReads[g.sub]
}

// gitFileWriters are git subcommands that write files of their own besides stdout: archive with -o,
// format-patch into -o's directory or the current one, bundle create its bundle, fast-export and
// fast-import their marks files. A review job refuses them (readonly.go).
var gitFileWriters = map[string]bool{
	"archive": true, "format-patch": true, "bundle": true, "fast-export": true, "fast-import": true,
}

// gitStreamMarks is the write target gitWriteTargets reports for fast-import --allow-unsafe-features:
// the export-marks features of the stream on stdin then name files the guard does not see.
const gitStreamMarks = "--allow-unsafe-features"

// gitUnverifiedTarget reports a git write target the guard cannot check: computed at run time (a
// variable or a substitution: `-o "$G"`) or named by fast-import's stream (gitStreamMarks).
func gitUnverifiedTarget(t string) bool { return strings.ContainsAny(t, "$`") || t == gitStreamMarks }

// gitWriteTargets returns the files and directories a git call writes besides stdout, up to `--`:
// the value of --output in any abbreviation parse-options accepts (`--outp=f`, `--output f`; git diff,
// log, show and the other diff-family commands), of archive's and format-patch's -o (attached, in a
// cluster `-vo f`, or separate), of format-patch's --output-directory, the current directory for
// format-patch without -o unless the last of --stdout and --no-stdout (`--no-std`) is --stdout, the
// file of bundle create, and the marks files of fast-export and fast-import (gitMarksTargets). A
// relative target is joined to the call's -C dirs. found also reports such an option with no value
// word after it.
func gitWriteTargets(argv []string, g gitCall) (dests []string, found bool) {
	sub := strings.ToLower(g.sub)
	long := []string{"output"}
	if sub == "format-patch" {
		long = append(long, "output-directory")
	}
	stdout := false
	for k := 0; k < len(g.args); k++ {
		a := g.args[k]
		if a == "--" {
			break
		}
		v := ""
		switch {
		case longPrefixOf(a, long...):
			found = true
			if _, val, ok := strings.Cut(a, "="); ok {
				v = val
			} else if k+1 < len(g.args) {
				k++
				v = g.args[k]
			}
		case sub == "format-patch" && longPrefixOf(a, "stdout", "no-stdout"):
			// The negation is a prefix of no-stdout (`--no-std`); the last of the two decides.
			stdout = !strings.HasPrefix(a, "--no-")
		case (sub == "archive" || sub == "format-patch") && len(a) > 1 && a[0] == '-' && a[1] != '-' && strings.IndexByte(a, 'o') > 0:
			// A short cluster: the rest after `o` is its value, or the next word.
			found = true
			if v = a[strings.IndexByte(a, 'o')+1:]; v == "" && k+1 < len(g.args) {
				k++
				v = g.args[k]
			}
		}
		if v != "" {
			dests = append(dests, v)
		}
	}
	if sub == "format-patch" && !found && !stdout {
		dests, found = append(dests, "."), true
	}
	var more []string
	switch sub {
	case "bundle":
		more = gitBundleTarget(g.args)
	case "fast-export", "fast-import":
		more = gitMarksTargets(sub, g.args)
	}
	dests, found = append(dests, more...), found || len(more) > 0
	// Each -C applies in turn: a relative one below the last, an absolute or `~` one from there.
	dir := ""
	for i := 1; i+1 < g.subIdx; i++ {
		if argv[i] == "-C" {
			dir = joinRel(dir, argv[i+1])
		}
		if gitValueOpts[argv[i]] {
			i++
		}
	}
	for i, d := range dests {
		if dir != "" {
			dests[i] = joinRel(dir, d)
		}
	}
	if sub == "fast-import" && slices.ContainsFunc(g.args, func(a string) bool { return longPrefixOf(a, "allow-unsafe-features") }) {
		dests, found = append(dests, gitStreamMarks), true
	}
	return dests, found
}

// gitBundleTarget returns the file of `git bundle create [options] <file>`: the first word after
// create that is no option (`-` is stdout), skipping the value of a separate --version.
func gitBundleTarget(args []string) []string {
	if len(args) == 0 || args[0] != "create" {
		return nil
	}
	for k := 1; k < len(args); k++ {
		a := args[k]
		switch {
		case a == "--":
			if k+1 < len(args) && args[k+1] != "-" {
				return []string{args[k+1]}
			}
			return nil
		case a == "-":
			return nil
		case strings.HasPrefix(a, "-"):
			if longPrefixOf(a, "version") && !strings.Contains(a, "=") {
				k++
			}
		default:
			return []string{a}
		}
	}
	return nil
}

// gitMarksTargets returns the marks files fast-export and fast-import name with --export-marks,
// --import-marks or fast-import's --import-marks-if-exists (any abbreviation, `=` or a separate
// value). fast-import reads a relative one below .git/info/fast-import after --relative-marks, which
// the stream on stdin can turn on too (`feature relative-marks`): a relative file is also returned
// there (`../../config` is .git/config).
func gitMarksTargets(sub string, args []string) (dests []string) {
	names := []string{"export-marks", "import-marks"}
	if sub == "fast-import" {
		names = append(names, "import-marks-if-exists")
	}
	for k := 0; k < len(args); k++ {
		a := args[k]
		if a == "--" {
			break
		}
		if !longPrefixOf(a, names...) {
			continue
		}
		_, v, ok := strings.Cut(a, "=")
		if !ok && k+1 < len(args) {
			k++
			v = args[k]
		}
		if v == "" {
			continue
		}
		dests = append(dests, v)
		if sub == "fast-import" && !filepath.IsAbs(v) && !strings.HasPrefix(v, "~") {
			dests = append(dests, filepath.Join(".git", "info", "fast-import", v))
		}
	}
	return dests
}

// joinRel is p below dir, or p itself when it is absolute or starts with `~`.
func joinRel(dir, p string) string {
	if dir == "" || filepath.IsAbs(p) || strings.HasPrefix(p, "~") {
		return p
	}
	return filepath.Join(dir, p)
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
const codeGlobSeps = " \t\r\n'\"`();=<>|&+"

// gitAdminNames are the entries of a git dir that run programs (config, hooks, a worktree's admin dir).
var gitAdminNames = []string{"config", "hooks", "worktrees"}

// codeGlobReason returns the reason to refuse inline code (one quoted word: the shell does not glob
// it, the language may) for its glob characters, or "". `?`, `[`, `]` and `{` are refused as in any
// other argument of a writer: the languages' glob dialects differ there (python's fnmatch reads `[!x]`
// as a negation and `[]t]` as a set holding `]`, Go's filepath.Match does not), so the guard does not
// interpret them. `*` means the same in each: it is refused in a piece that can name git's config or
// hooks (`.g*t/hooks/*`, `*/config`), one of the job's git dirs, a protected path, a no-read dir or the
// agent state outside the job's own folder (relative pieces against the call's directory, `~/` against
// $HOME), and as `*(` (node's extglob). A piece made of `*` only is an operator (`a * b`, `2 ** 8`). A
// glob built at run time is not seen (the residual gap of inline code, checkProtected).
func codeGlobReason(code string, cfg Config) string {
	if strings.ContainsAny(code, "?[]{") {
		return globBlocked(code)
	}
	if strings.Contains(code, "*(") {
		return codeGlobBlocked("*(")
	}
	for _, piece := range strings.FieldsFunc(code, func(r rune) bool { return strings.ContainsRune(codeGlobSeps, r) }) {
		if strings.Trim(piece, "*") != "" && strings.Contains(piece, "*") && (gitAdminPattern(piece) || stateGlob(piece, cfg)) {
			return codeGlobBlocked(piece)
		}
	}
	return ""
}

// globMatch reports a pattern component matching a name, case-folded (a case-insensitive volume
// opens `.GIT/config` too); a malformed pattern matches nothing (the language rejects it as well).
func globMatch(pat, name string) bool {
	ok, err := filepath.Match(strings.ToLower(pat), strings.ToLower(name))
	return err == nil && ok
}

// gitAdminPattern reports a `*` pattern that can name a path inside a `.git` dir that runs programs:
// a component spelled for .git (`.g*t`, `.gi*`), a glob component spelled for config, hooks or
// worktrees (`hoo*`), or a glob component or one matching .git (`*`, `.*`, `i*` of a string
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
		case dotGit && strings.Trim(c, ".*") != "":
			return true
		case hasGlob(c) && strings.Trim(c, "*") != "" && admin(c):
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
