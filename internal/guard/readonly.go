package guard

import (
	"path/filepath"
	"regexp"
	"slices"
	"strings"
)

// Review read-only mode (Config.ReadOnly): a review job reads the repository but never changes its
// worktree. Claude Code deny rules are prefix patterns, so this check is the enforcement; the deny
// rules stay as a first layer. Paths are resolved by pathScope (pathres.go) with the worktree as the
// only protected path. Refused when the target is inside the worktree (or cannot be resolved):
//   - file tools (Write, Edit, MultiEdit, NotebookEdit) by path;
//   - output redirections (`>`, `>>`, `&>`, `<>`), `tee`, `dd of=`;
//   - in-place editors (`sed -i`, `perl -i`, `ruby -i`, `gawk -i inplace`) and awk programs that
//     write (`print > f`, pipes, system());
//   - tree writers (rm, mv, cp/install/ln/rsync destinations, chmod, touch, mkdir, truncate, …),
//     extractors (tar -x, unzip, patch) by their target directory, find -delete/-fprint and
//     find -exec of a writer, and any writer fed by xargs (its operands are not known);
//   - git subcommands that change the worktree, the index or the history, in any global-option
//     spelling (`git -C <dir>`, `-c k=v`, `--git-dir`), wherever they point: a review job has no
//     repository of its own to change;
//   - interpreters running inline code (`python -c`, `node -e`, `perl -e`, `ruby -e`, `php -r`, a
//     here-doc, here-string or pipe as the program) and shells reading their commands from stdin:
//     what such code writes is not visible, so it is refused outright.
//
// Writes outside the worktree (the job directory, /tmp, test caches) and test runners (`go test`,
// `go vet`, `npm test`, `pytest`) stay allowed. Not covered: a program that writes into its working
// directory by itself (`go build`, `npm install`, `make`), a script the job writes and runs, a write
// in an earlier call through a symlink made then. The job-done line warns when a review job's
// worktree differs from its base at the end (the second layer).

// readOnlyBlocked is the refusal of a write into the review job's worktree.
func readOnlyBlocked(what string) string {
	return "blocked by guard: this is a review job, read-only: " + what + " would write inside the job's worktree. " +
		"Read, search and run tests; write notes into the job directory, not the repository."
}

// roBlocked is the refusal of a write the read-only check found: into the review job's worktree, or
// into the job's sources (G02).
func (s *pathScope) roBlocked(what string) string {
	if s.sources {
		return sourcesWriteBlocked(what)
	}
	return readOnlyBlocked(what)
}

// readOnlyFile reports a file tool path inside the read-only worktree (the worktree as given or with
// symlinks resolved).
func readOnlyFile(p string, cfg Config) bool {
	return cfg.ReadOnly != "" && inJobGitDir(p, []string{cfg.ReadOnly})
}

// checkReadOnly refuses Bash writes into the review job's worktree (cfg.ReadOnly).
func checkReadOnly(scripts []*script, _ string, cfg Config) string {
	if cfg.ReadOnly == "" {
		return ""
	}
	rc := cfg
	rc.Protected = []string{cfg.ReadOnly}
	s := newPathScope(scripts, rc)
	r := s.readOnlyScripts(scripts)
	if s.expired {
		return "blocked by guard: the command is too complex to verify against the review job's read-only worktree " +
			"within the guard's time budget; split it."
	}
	return r
}

// roHit returns the worktree path a write target word may reach. A word expanded at run time (a
// variable other than $HOME, a substitution), a `~user` path, or a relative word after a cd the
// guard cannot resolve is a hit: where it leads is not known.
func (s *pathScope) roHit(w string, dyn, tree bool) (string, bool) {
	if s.over() {
		return w, true
	}
	if dyn && strings.ContainsAny(strings.NewReplacer("${HOME}", "", "$HOME", "").Replace(w), "$`") &&
		(!s.sources || s.namesProtected(w)) {
		return w + " (expanded at run time)", true
	}
	exp, ok := s.expandHome(w)
	if !ok {
		return w, true
	}
	if !filepath.IsAbs(exp) && s.dynamic != "" {
		return w + " (after cd " + s.dynamic + ")", true
	}
	if s.makesLinks && s.linkDotDot(exp) {
		return w, true
	}
	return s.match(s.candidates(exp), tree)
}

// roWord is roHit for an argument word: an `--opt=value` / `key=value` word is checked by its value.
func (s *pathScope) roWord(w string, dyn, tree bool) (string, bool) {
	if v := wordValue(w); v != w && strings.HasPrefix(w, "-") {
		w = v
	}
	return s.roHit(w, dyn, tree)
}

// gitWorktreeWriters are git subcommands that change the worktree, the index, refs or history.
var gitWorktreeWriters = map[string]bool{
	"add": true, "am": true, "apply": true, "checkout": true, "checkout-index": true, "cherry-pick": true,
	"clean": true, "commit": true, "commit-tree": true, "filter-branch": true, "merge": true, "mergetool": true,
	"mv": true, "notes": true, "pull": true, "push": true, "read-tree": true, "rebase": true, "replace": true,
	"reset": true, "restore": true, "revert": true, "rm": true, "sparse-checkout": true, "stash": true,
	"submodule": true, "switch": true, "symbolic-ref": true, "update-index": true, "update-ref": true,
	"worktree": true,
}

// roOperandWriters write (create, remove, change) every file operand; the value is the short options
// taking a value (the word after them is not an operand).
var roOperandWriters = map[string]string{
	"rm": "", "rmdir": "", "unlink": "", "shred": "ns", "touch": "drt", "truncate": "sr", "mkdir": "m",
	"mkfifo": "m", "mknod": "m", "tee": "", "chmod": "", "chown": "", "chgrp": "", "gzip": "S", "gunzip": "S",
	"bzip2": "", "bunzip2": "", "xz": "", "unxz": "", "zstd": "o",
}

// compressors write next to their operands unless they write to stdout, list or test.
var compressors = map[string]bool{"gzip": true, "gunzip": true, "bzip2": true, "bunzip2": true, "xz": true, "unxz": true, "zstd": true}

// roTree are the operand writers that also reach the worktree through an ancestor (`rm -r ..`).
var roTree = map[string]bool{"rm": true, "rmdir": true, "unlink": true, "shred": true, "chmod": true, "chown": true, "chgrp": true}

// reAwkWrite is an awk program that writes a file or runs a command: `print > f`, `| "cmd"`, system().
var reAwkWrite = regexp.MustCompile(`printf?\b[^;}\n]*>|\|\s*["$a-zA-Z_(]|"\s*\||\|&|system\s*\(`)

func (s *pathScope) readOnlyScripts(scripts []*script) string {
	for _, sc := range scripts {
		for _, pl := range sc.pipelines {
			for i, c := range pl.cmds {
				for k, t := range c.outRedirs {
					if isDigits(t) || t == "-" { // `2>&1`, `>&-`: a descriptor, not a file
						continue
					}
					if p, ok := s.roHit(t, k < len(c.outDyn) && c.outDyn[k], false); ok {
						return s.roBlocked("redirection to `" + p + "`")
					}
				}
				v := split(c.words)
				argv := c.words[v.start:]
				if len(argv) == 0 {
					continue
				}
				if r := s.readOnlyCmd(argv, c.dyn[v.start:], c, i > 0, slices.Contains(v.wrappers, "xargs")); r != "" {
					return r
				}
			}
		}
	}
	return ""
}

// readOnlyCmd checks one command (program and args). piped: it reads a pipe; xargs: it runs under
// xargs (more operands come from stdin).
func (s *pathScope) readOnlyCmd(argv []string, dyn []bool, c *simpleCmd, piped, xargs bool) string {
	name := progName(argv[0])
	args, adyn := argv[1:], dyn[1:]
	if g, ok := parseGit(argv); ok {
		if gitWorktreeWriters[strings.ToLower(g.sub)] && (!s.sources || s.pointsInside(args, adyn)) {
			return s.roBlocked("`git " + g.sub + "`")
		}
		// archive and format-patch write files (format-patch into the worktree by default).
		if gitFileWriters[strings.ToLower(g.sub)] {
			return readOnlyBlocked("`git " + g.sub + "` (it writes files)")
		}
		// `git diff --output=<file>` writes the file.
		outs, _ := gitWriteTargets(argv, g)
		for _, t := range outs {
			if p, ok := s.roHit(t, strings.ContainsAny(t, "$`"), false); ok {
				return readOnlyBlocked("`git " + g.sub + " --output` on `" + p + "`")
			}
		}
		return ""
	}
	if r := inlineInterp(name, args, c, piped); r != "" && (!s.sources || s.pointsInside(args, adyn)) {
		return s.roBlocked(r)
	}
	check := func(w string, d, tree bool) string {
		if p, ok := s.roWord(w, d, tree); ok {
			return s.roBlocked("`" + name + "` on `" + p + "`")
		}
		return ""
	}
	operands := func(valueShort string) (ops []int) {
		for k := 0; k < len(args); k++ {
			a := args[k]
			switch {
			case a == "--":
				for j := k + 1; j < len(args); j++ {
					ops = append(ops, j)
				}
				return ops
			case strings.HasPrefix(a, "--"):
				if _, _, eq := strings.Cut(a, "="); eq {
					ops = append(ops, k) // `--reference=f`, `--target-directory=d`: checked by value
				}
			case strings.HasPrefix(a, "-") && len(a) > 1:
				if strings.IndexByte(valueShort, a[len(a)-1]) >= 0 && !strings.ContainsAny(a[1:len(a)-1], valueShort) {
					k++
				}
			default:
				ops = append(ops, k)
			}
		}
		return ops
	}
	switch {
	case name == "find":
		return s.readOnlyFind(args, adyn)
	case name == "sed":
		script := sedScriptArgs(argv)
		for k := 1; k < len(argv); k++ {
			if script[k] && reSedWrite.MatchString(argv[k]) {
				return s.roBlocked("a `sed` script with a w command or flag")
			}
		}
		if !inPlace(name, args) {
			return ""
		}
		if xargs {
			return s.roBlocked("`xargs sed -i`")
		}
		for k := 1; k < len(argv); k++ {
			if _, notFile := script[k]; notFile || strings.HasPrefix(argv[k], "-") {
				continue
			}
			if r := check(argv[k], dyn[k], false); r != "" {
				return r
			}
		}
		return ""
	case name == "perl" || name == "ruby" || awks[name]:
		if awks[name] && awkWrites(args) {
			return s.roBlocked("an `" + name + "` program writing a file or running a command")
		}
		if !inPlace(name, args) {
			return ""
		}
		if xargs {
			return s.roBlocked("`xargs " + name + " -i`")
		}
		for _, k := range operands("") {
			if r := check(args[k], adyn[k], false); r != "" {
				return r
			}
		}
		return ""
	case name == "dd":
		for k, a := range args {
			if strings.HasPrefix(a, "of=") {
				if r := check(a[len("of="):], adyn[k], false); r != "" {
					return r
				}
			}
		}
		return ""
	case destCopiers[name]:
		if xargs {
			return s.roBlocked("`xargs " + name + "`")
		}
		// Destinations and sources lose their position in argv: roHit refuses any of them holding `$`.
		ca := parseCopyArgs(name, args)
		for _, d := range ca.dests {
			if r := check(d, true, ca.whole); r != "" {
				return r
			}
			for _, src := range ca.srcs {
				if r := check(d+"/"+filepath.Base(filepath.Clean(src)), true, false); r != "" {
					return r
				}
			}
		}
		if name == "mv" { // the sources are removed
			for _, src := range ca.srcs {
				if r := check(src, true, true); r != "" {
					return r
				}
			}
		}
		return ""
	case name == "rsync":
		if xargs {
			return s.roBlocked("`xargs rsync`")
		}
		var ops []int
		for _, k := range operands("eBfT") {
			if !strings.HasPrefix(args[k], "-") {
				ops = append(ops, k)
			}
		}
		if len(ops) < 2 {
			return "" // one operand lists it
		}
		k := ops[len(ops)-1]
		return check(args[k], adyn[k], false)
	case name == "tar":
		return s.readOnlyTar(args, adyn, check)
	case name == "unzip":
		if slices.ContainsFunc(args, func(a string) bool { return a == "-l" || a == "-t" || a == "-p" || a == "-v" || a == "-Z" }) {
			return ""
		}
		return s.extractInto(args, adyn, "-d", check)
	case name == "patch":
		if slices.Contains(args, "--dry-run") {
			return ""
		}
		for k, a := range args {
			if (a == "-o" || a == "--output") && k+1 < len(args) {
				return check(args[k+1], adyn[k+1], false)
			}
		}
		return s.extractInto(args, adyn, "-d", check)
	}
	vs, ok := roOperandWriters[name]
	if !ok {
		return ""
	}
	if xargs {
		return s.roBlocked("`xargs " + name + "`")
	}
	ops := operands(vs)
	if (name == "chmod" || name == "chown" || name == "chgrp") && len(ops) > 0 &&
		!slices.ContainsFunc(args, func(a string) bool { return strings.HasPrefix(a, "--reference") }) {
		ops = ops[1:] // the mode or owner
	}
	if compressors[name] && slices.ContainsFunc(args, func(a string) bool {
		return a == "--stdout" || a == "--to-stdout" || a == "-l" || a == "--list" || a == "-t" || a == "--test" ||
			strings.HasPrefix(a, "-") && !strings.HasPrefix(a, "--") && strings.ContainsAny(a, "clt")
	}) {
		return ""
	}
	for _, k := range ops {
		if r := check(args[k], adyn[k], roTree[name]); r != "" {
			return r
		}
	}
	return ""
}

// extractInto checks the directory an extractor writes into: the value of opt (`-d DIR`, `-dDIR`),
// else the working directory.
func (s *pathScope) extractInto(args []string, adyn []bool, opt string, check func(string, bool, bool) string) string {
	dir, d := ".", false
	for k, a := range args {
		switch {
		case a == opt && k+1 < len(args):
			dir, d = args[k+1], adyn[k+1]
		case strings.HasPrefix(a, opt) && len(a) > len(opt) && !strings.HasPrefix(a, "--"):
			dir, d = a[len(opt):], adyn[k]
		case strings.HasPrefix(a, "--directory="):
			dir, d = a[len("--directory="):], adyn[k]
		}
	}
	return check(dir, d, false)
}

// readOnlyTar refuses tar extracting into the worktree (-C, else the working directory) or writing an
// archive (-f) there.
func (s *pathScope) readOnlyTar(args []string, adyn []bool, check func(string, bool, bool) string) string {
	extract, list, file, fd := false, false, "", false
	for k := 0; k < len(args); k++ {
		a := args[k]
		switch {
		case a == "--extract" || a == "--get" || a == "--delete":
			extract = true
		case a == "--list":
			list = true
		case a == "--file" && k+1 < len(args):
			k++
			file, fd = args[k], adyn[k]
		case strings.HasPrefix(a, "--file="):
			file, fd = a[len("--file="):], adyn[k]
		case strings.HasPrefix(a, "--"):
		case k == 0 && !strings.HasPrefix(a, "-") || strings.HasPrefix(a, "-") && len(a) > 1:
			cl := strings.TrimPrefix(a, "-")
			extract = extract || strings.ContainsRune(cl, 'x')
			list = list || strings.ContainsRune(cl, 't')
			if j := strings.IndexByte(cl, 'f'); j >= 0 {
				switch {
				case j < len(cl)-1 && strings.HasPrefix(a, "-"):
					file, fd = cl[j+1:], adyn[k]
				case k+1 < len(args):
					k++
					file, fd = args[k], adyn[k]
				}
			}
		}
	}
	if extract {
		dir, d := ".", false
		for k, a := range args {
			switch {
			case (a == "-C" || a == "--directory") && k+1 < len(args):
				dir, d = args[k+1], adyn[k+1]
			case strings.HasPrefix(a, "--directory="):
				dir, d = a[len("--directory="):], adyn[k]
			}
		}
		return check(dir, d, false)
	}
	if file != "" && file != "-" && !list {
		return check(file, fd, false)
	}
	return ""
}

// readOnlyFind refuses find -delete / -fprint / -fls, and -exec / -execdir / -ok / -okdir of a writer,
// on a start path inside the worktree (none = the working directory).
func (s *pathScope) readOnlyFind(args []string, adyn []bool) string {
	var starts []int
	for k, a := range args {
		if strings.HasPrefix(a, "-") || a == "(" || a == "!" {
			break
		}
		starts = append(starts, k)
	}
	inTree := func() (string, bool) {
		if len(starts) == 0 {
			return s.roHit(".", false, false)
		}
		for _, k := range starts {
			if p, ok := s.roHit(args[k], adyn[k], false); ok {
				return p, true
			}
		}
		return "", false
	}
	for k := 0; k < len(args); k++ {
		a := args[k]
		switch {
		case a == "-delete" || strings.HasPrefix(a, "-fprint") || a == "-fls":
			if p, ok := inTree(); ok {
				return s.roBlocked("`find " + a + "` under `" + p + "`")
			}
		case findExec[a]:
			end := k + 1
			for end < len(args) && args[end] != ";" && args[end] != "+" {
				end++
			}
			sub := args[k+1 : end]
			if len(sub) > 0 {
				// `{}` stands for the paths found: the first start path stands in for them.
				first := "."
				if len(starts) > 0 {
					first = args[starts[0]]
				}
				words := make([]string, len(sub))
				for j, w := range sub {
					words[j] = strings.ReplaceAll(w, "{}", first)
				}
				if r := s.readOnlyCmd(words, adyn[k+1:end], &simpleCmd{}, false, false); r != "" {
					return r
				}
			}
			k = end
		}
	}
	return ""
}

// inPlace reports `sed -i`, `perl -i`, `ruby -i` or `gawk -i inplace`.
func inPlace(name string, args []string) bool {
	for k, a := range args {
		if a == "--" || !strings.HasPrefix(a, "-") {
			if name != "sed" {
				return false
			}
			continue
		}
		if strings.HasPrefix(a, "--") {
			if name == "sed" && len(a) > 4 && strings.HasPrefix("--in-place", strings.SplitN(a, "=", 2)[0]) {
				return true
			}
			if awks[name] && (a == "--include" && k+1 < len(args) && args[k+1] == "inplace" || a == "--include=inplace") {
				return true
			}
			continue
		}
		if awks[name] {
			if a == "-i" && k+1 < len(args) && args[k+1] == "inplace" || a == "-iinplace" {
				return true
			}
			continue
		}
		for j := 1; j < len(a); j++ {
			ch := a[j]
			if ch == 'i' {
				return true
			}
			if name == "sed" && (ch == 'e' || ch == 'f' || ch == 'l') {
				break // the rest is a value
			}
		}
	}
	return false
}

// awkWrites reports an awk program (the first operand, or an -f file not read here) that writes.
func awkWrites(args []string) bool {
	for k := 0; k < len(args); k++ {
		switch a := args[k]; {
		case a == "--":
			return k+1 < len(args) && reAwkWrite.MatchString(args[k+1])
		case a == "-F" || a == "-v":
			k++
		case strings.HasPrefix(a, "-f") || a == "--file":
			return false
		case strings.HasPrefix(a, "-") && a != "-":
		default:
			return reAwkWrite.MatchString(a)
		}
	}
	return false
}

// inlineInterp returns what a call runs as inline code (an interpreter's -c/-e program, a here-doc,
// here-string or pipe as the program of an interpreter or a shell), "" if none.
func inlineInterp(name string, args []string, c *simpleCmd, piped bool) string {
	interp := name
	if strings.HasPrefix(interp, "python") {
		interp = "python"
	}
	spec, isInterp := interpSpecs[interp]
	if !isInterp && !shells[name] {
		return ""
	}
	operand := -1
scan:
	for k := 0; k < len(args); k++ {
		a := args[k]
		switch {
		case a == "-":
			return "`" + name + " -` (program from stdin)"
		case a == "--":
			if k+1 < len(args) {
				operand = k + 1
			}
			break scan
		case strings.HasPrefix(a, "--"):
			opt, _, eq := strings.Cut(a, "=")
			if slices.Contains(spec.codeLong, opt) {
				return "`" + name + " " + opt + "` (inline code)"
			}
			if !eq && slices.Contains(spec.valueLong, opt) {
				k++
			}
		case strings.HasPrefix(a, "-") && len(a) > 1:
			if shells[name] {
				if strings.ContainsAny(a[1:], "c") {
					return "" // `sh -c <code>`: the code is parsed and checked as commands of its own
				}
				if strings.ContainsRune(a[1:], 's') {
					return "`" + name + " -s` (commands from stdin)"
				}
				continue
			}
			for j := 1; j < len(a); j++ {
				ch := a[j]
				switch {
				case strings.IndexByte(spec.inlineShort, ch) >= 0:
					return "`" + name + " -" + string(ch) + "` (inline code)"
				case strings.IndexByte(spec.codeShort, ch) >= 0:
					operand = k // -m module, -f file: a program from a file
					break scan
				case strings.IndexByte(spec.valueShort, ch) >= 0:
					if j == len(a)-1 {
						k++
					}
					continue scan
				case strings.IndexByte(spec.attachShort, ch) >= 0:
					continue scan
				}
			}
		default:
			operand = k
			break scan
		}
	}
	if operand < 0 && (c.heredoc != "" || piped) {
		return "`" + name + "` reading its program from a here-doc, here-string or pipe"
	}
	return ""
}
