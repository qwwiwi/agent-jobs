package guard

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
)

// Path resolution for the protected-path check: which file system paths a word of a Bash command can
// name. The guard does not run the command, so it over-approximates: a relative word is resolved
// against every directory the command may be in (the call's directory and each literal cd/pushd
// target, also when that cd may fail), `~/` and $HOME are expanded, and when a cd target cannot be
// resolved (a variable, `cd -`, CDPATH) a relative word is compared by its segments with the tail of
// each protected path ("could it reach it from some directory").

// maxScopeDirs bounds the directories tracked through cd chains; beyond it the directory is unknown.
const maxScopeDirs = 64

// pathScope is what the guard knows about where the paths of one Bash call resolve.
type pathScope struct {
	home      string   // $HOME, "" = unknown
	dirs      []string // every directory a relative path may resolve against
	dynamic   string   // the first cd target the guard cannot resolve ("" = none)
	protected []string // protected paths, as configured and with symlinks resolved
}

// newPathScope walks every command of the call (substitutions and `sh -c` code included) and
// collects the directories literal cd/pushd targets lead to.
func newPathScope(scripts []*script, cfg Config) *pathScope {
	s := &pathScope{dirs: []string{filepath.Clean(cfg.Dir)}}
	if h, err := os.UserHomeDir(); err == nil && filepath.IsAbs(h) {
		s.home = filepath.Clean(h)
	}
	for _, p := range cfg.Protected {
		if p == "" {
			continue
		}
		p = filepath.Clean(p)
		s.protected = append(s.protected, p)
		if r := resolvePath(p, "/"); r != p {
			s.protected = append(s.protected, r)
		}
	}
	cdpathEnv := os.Getenv("CDPATH") != ""
	for _, sc := range scripts {
		for _, pl := range sc.pipelines {
			for _, c := range pl.cmds {
				for _, w := range c.words {
					if strings.Contains(w, "CDPATH=") {
						s.unknown("CDPATH=")
					}
				}
				argv := argvOf(c.words)
				if len(argv) == 0 {
					continue
				}
				switch strings.ToLower(base(argv[0])) {
				case "cd", "pushd":
					s.cd(argv, c.dyn[len(c.words)-len(argv):], cdpathEnv)
				}
			}
		}
	}
	return s
}

func (s *pathScope) unknown(why string) {
	if s.dynamic == "" {
		s.dynamic = why
	}
}

// cd adds the directories `cd|pushd [opts] target` leads to from every known directory.
func (s *pathScope) cd(argv []string, dyn []bool, cdpathEnv bool) {
	k := 1
	for k < len(argv) && strings.HasPrefix(argv[k], "-") && len(argv[k]) > 1 && argv[k] != "--" {
		if argv[k][1] >= '0' && argv[k][1] <= '9' { // pushd -N: a stack entry
			s.unknown(argv[k])
			return
		}
		k++
	}
	if k < len(argv) && argv[k] == "--" {
		k++
	}
	var t string
	switch {
	case k >= len(argv):
		if strings.EqualFold(base(argv[0]), "pushd") { // swaps the top two stack entries
			s.unknown("pushd")
			return
		}
		t = "~"
	case dyn[k] || hasGlob(argv[k]) || argv[k] == "-" || strings.HasPrefix(argv[k], "+"):
		s.unknown(argv[k])
		return
	default:
		t = argv[k]
	}
	p, ok := s.expandHome(t)
	if !ok {
		s.unknown(t)
		return
	}
	if cdpathEnv && !filepath.IsAbs(p) && p != "." && p != ".." &&
		!strings.HasPrefix(p, "./") && !strings.HasPrefix(p, "../") {
		s.unknown("CDPATH")
		return
	}
	for _, d := range slices.Clone(s.dirs) {
		if len(s.dirs) >= maxScopeDirs {
			s.unknown("too many cd")
			return
		}
		if n := resolvePath(p, d); !slices.Contains(s.dirs, n) {
			s.dirs = append(s.dirs, n)
		}
	}
}

// expandHome expands a leading `~` / `~/` and $HOME / ${HOME} the way the shell would. ok is false
// for a home the guard does not know (`~user/…`, `~+`, `~-`, or $HOME unset).
func (s *pathScope) expandHome(p string) (string, bool) {
	for _, v := range []string{"${HOME}", "$HOME"} {
		if p == v || strings.HasPrefix(p, v+"/") {
			if s.home == "" {
				return "", false
			}
			return s.home + p[len(v):], true
		}
	}
	if !strings.HasPrefix(p, "~") {
		return p, true
	}
	if (p == "~" || strings.HasPrefix(p, "~/")) && s.home != "" {
		return s.home + p[1:], true
	}
	return "", false
}

// expandAllHomes replaces every `~/` that starts a word or follows `=`, `:`, a quote, a space or a
// bracket, and every $HOME / ${HOME}, with the home directory: for the substring check of option
// values (`of=~/…`) and inline code (`python3 -c "open('~/…')"`, os.path.expanduser).
func (s *pathScope) expandAllHomes(w string) string {
	if s.home == "" {
		return w
	}
	w = strings.ReplaceAll(strings.ReplaceAll(w, "${HOME}", s.home), "$HOME", s.home)
	var b strings.Builder
	for i := 0; i < len(w); i++ {
		if w[i] == '~' && i+1 < len(w) && w[i+1] == '/' && (i == 0 || strings.IndexByte("=: '\"([{,", w[i-1]) >= 0) {
			b.WriteString(s.home)
			continue
		}
		b.WriteByte(w[i])
	}
	return b.String()
}

// hit returns the protected path a path word reaches: the word itself is protected or inside a
// protected path, or, with tree set (rm -r, mv, chmod -R, find -delete, …), a protected path lies
// inside the word's directory. A word the guard cannot resolve (`~user/…`, a relative word after an
// unresolved cd) is a hit when its segments could lead to a protected path from some directory. A
// `key=value` word is checked as written and by its value.
func (s *pathScope) hit(w string, tree bool) (string, bool) {
	if q, ok := s.hitPath(w, tree); ok {
		return q, true
	}
	if v := wordValue(w); v != w {
		return s.hitPath(v, tree)
	}
	return "", false
}

func (s *pathScope) hitPath(w string, tree bool) (string, bool) {
	if filepath.Clean(w) == "." && !tree {
		return "", false
	}
	exp, ok := s.expandHome(w)
	if !ok {
		rest := ""
		if _, r, found := strings.Cut(filepath.Clean(w), "/"); found {
			rest = r
		}
		if s.couldReach(rest, tree) {
			return w, true
		}
		return "", false
	}
	if q, ok := s.match(s.candidates(exp), tree); ok {
		return q, true
	}
	if !filepath.IsAbs(exp) && s.dynamic != "" && s.couldReach(exp, tree) {
		return w + " (after cd " + s.dynamic + ")", true
	}
	return "", false
}

// candidates returns the absolute paths a home-expanded path may name: against every known directory
// when relative; cleaned, with symlinks resolved, and, with a `..`, walked the way the kernel does.
func (s *pathScope) candidates(exp string) []string {
	bases := []string{exp}
	if !filepath.IsAbs(exp) {
		bases = bases[:0]
		for _, d := range s.dirs {
			bases = append(bases, d+"/"+exp)
		}
	}
	var out []string
	for _, b := range bases {
		out = append(out, filepath.Clean(b), resolvePath(b, "/"))
		if hasDotDot(b) {
			out = append(out, resolvePath(physical(b), "/"))
		}
	}
	return out
}

// physical walks an absolute path like the kernel: `..` leaves the directory its prefix resolves to
// (symlinks followed), while filepath.Clean drops `link/..` lexically.
func physical(abs string) string {
	cur := "/"
	for _, seg := range strings.Split(filepath.ToSlash(abs), "/") {
		switch seg {
		case "", ".":
		case "..":
			cur = filepath.Dir(resolvePath(cur, "/"))
		default:
			cur = filepath.Join(cur, seg)
		}
	}
	return cur
}

// match checks absolute candidate paths against the protected paths.
func (s *pathScope) match(cands []string, tree bool) (string, bool) {
	for _, p := range cands {
		if inJobGitDir(p, s.protected) {
			return p, true
		}
		if tree {
			for _, q := range s.protected {
				if within(q, p) {
					return q, true
				}
			}
		}
	}
	return "", false
}

// parentHit reports a word that resolves to the directory holding a protected path: a destination
// directory (`cp f <dir>`, `unzip -d <dir>`) writes join(dir, name), which is protected for that name.
func (s *pathScope) parentHit(w string) (string, bool) {
	exp, ok := s.expandHome(wordValue(w))
	if !ok {
		return "", false
	}
	for _, p := range s.candidates(exp) {
		for _, q := range s.protected {
			if filepath.Join(p, filepath.Base(q)) == q {
				return q, true
			}
		}
	}
	return "", false
}

// couldReach reports whether a relative path, joined to some unknown directory, can be or lie inside
// a protected path (with tree, also be an ancestor of one): its segments match the protected path's
// segments from some position on. A `..` segment can reach anything.
func (s *pathScope) couldReach(rel string, tree bool) bool {
	if hasDotDot(rel) {
		return true
	}
	rel = filepath.Clean(rel)
	if rel == "." {
		return tree
	}
	segs := strings.Split(filepath.ToSlash(rel), "/")
	for _, q := range s.protected {
		qs := strings.Split(strings.Trim(filepath.ToSlash(q), "/"), "/")
		for k := range qs {
			n := min(len(segs), len(qs)-k)
			if slices.Equal(segs[:n], qs[k:k+n]) && (len(segs) >= len(qs)-k || tree) {
				return true
			}
		}
	}
	return false
}

// contains returns the protected path that a word of a non-reader names inside its text: an option
// value (`dd of=…`, `--file=…`, `file:…`) or inline code (`python3 -c`, `perl -e`, `node -e`). It
// matches the protected path as a substring, absolute (homes expanded) or relative to any known
// directory (`../../../jobs.db`), and resolves every path-like token of the word like an argument
// (`//`, `/./`, `..` normalized); a token spelled with `//`, `/./` or `..` is also refused when it
// is an ancestor of a protected path.
func (s *pathScope) contains(w string) (string, bool) {
	w = s.expandAllHomes(w)
	for _, q := range s.protected {
		if strings.Contains(w, q) {
			return q, true
		}
		for _, d := range s.dirs {
			if rel, err := filepath.Rel(d, q); err == nil && !onlyDotDot(rel) && strings.Contains(w, rel) {
				return q, true
			}
		}
	}
	for _, t := range pathTokens(w) {
		odd := strings.Contains(t, "//") || strings.Contains(t, "/./") || strings.HasPrefix(t, "./") || hasDotDot(t)
		if q, ok := s.hitPath(t, odd); ok {
			return q, true
		}
	}
	return "", false
}

// onlyDotDot reports a relative path made of `.` and `..` segments only: from a directory inside a
// protected path (itself refused as a cd target) such a path would match any text.
func onlyDotDot(rel string) bool {
	for _, seg := range strings.Split(filepath.ToSlash(rel), "/") {
		if seg != ".." && seg != "." {
			return false
		}
	}
	return true
}

// pathTokens splits a word into the pieces a program may open as paths: on white space, quotes,
// brackets and `=`, `:`, `,`, `;`, `<`, `>`, `|`, `&`, `?` (`of=…`, `file:…?mode=rw`, `open('…')`).
func pathTokens(w string) []string {
	return strings.FieldsFunc(w, func(r rune) bool { return strings.ContainsRune(" \t\r\n'\"`=:,;()[]{}<>|&?", r) })
}

// treeWriters change or remove a whole directory tree, or place files into a directory, named by an
// argument: with an ancestor of a protected path as an argument they reach it (`rm -r <state>`,
// `mv <state> x`, `ln -s <state> S; rm S/…`, `tar -C <state> -x`).
var treeWriters = map[string]bool{
	"rm": true, "rmdir": true, "unlink": true, "mv": true, "cp": true, "install": true, "ln": true,
	"rsync": true, "chmod": true, "chown": true, "chgrp": true, "tar": true, "shred": true,
}

// findActs reports a find with an action that writes or runs something (-delete, -exec, -fprint, …).
func findActs(args []string) bool {
	return slices.ContainsFunc(args, func(a string) bool {
		return findExec[a] || a == "-delete" || strings.HasPrefix(a, "-fprint") || a == "-fls"
	})
}

// destCopiers take sources and a destination: the ancestor rule applies to the sources only.
var destCopiers = map[string]bool{"cp": true, "install": true, "ln": true}

// copyArgs is the split of cp/install/ln arguments into sources and destinations.
type copyArgs struct {
	dests   []string     // destination paths: the -t directory, else the last operand (`.` for `ln src`)
	destIdx map[int]bool // indexes of the arguments that hold a destination
	srcs    []string     // source operands
	whole   bool         // -T: the destination itself is replaced, so it is checked as an ancestor too
}

// copyLongVals are the long options of cp/install/ln whose value may be the next word.
var copyLongVals = map[string]bool{
	"--suffix": true, "--mode": true, "--owner": true, "--group": true, "--no-preserve": true,
	"--sparse": true, "--strip-program": true,
}

// parseCopyArgs splits cp/install/ln arguments like getopt: short clusters, `-t DIR`, `-tDIR`,
// `--target-directory[=]DIR` (and its abbreviations), `--`; with `install -d` every operand is a
// directory to create, so a destination.
func parseCopyArgs(name string, args []string) copyArgs {
	valOpts := "tS"
	if name == "install" {
		valOpts = "tSmog"
	}
	ca := copyArgs{destIdx: map[int]bool{}}
	var ops []int
	tdir, dirs := -1, false
	tval := ""
	for i := 0; i < len(args); i++ {
		a := args[i]
		switch {
		case a == "--":
			for j := i + 1; j < len(args); j++ {
				ops = append(ops, j)
			}
			i = len(args)
		case strings.HasPrefix(a, "--"):
			key, v, hasV := strings.Cut(a, "=")
			switch {
			case len(key) > 2 && strings.HasPrefix("--target-directory", key):
				if hasV {
					tdir, tval = i, v
				} else if i+1 < len(args) {
					i++
					tdir, tval = i, args[i]
				}
			case len(key) > 4 && strings.HasPrefix("--no-target-directory", key):
				ca.whole = true
			case name == "install" && len(key) > 2 && strings.HasPrefix("--directory", key):
				dirs = true
			case !hasV && copyLongVals[key]:
				i++
			}
		case strings.HasPrefix(a, "-") && len(a) > 1:
			for k := 1; k < len(a); k++ {
				c := a[k]
				if c == 'T' {
					ca.whole = true
				}
				if c == 'd' && name == "install" {
					dirs = true
				}
				if strings.IndexByte(valOpts, c) >= 0 {
					v, idx := a[k+1:], i
					if v == "" && i+1 < len(args) {
						i++
						v, idx = args[i], i
					}
					if c == 't' {
						tdir, tval = idx, v
					}
					break
				}
			}
		default:
			ops = append(ops, i)
		}
	}
	operands := func(ix []int) (out []string) {
		for _, j := range ix {
			out = append(out, args[j])
		}
		return out
	}
	switch {
	case dirs:
		ca.dests = operands(ops)
		for _, j := range ops {
			ca.destIdx[j] = true
		}
	case tdir >= 0:
		ca.dests, ca.destIdx[tdir], ca.srcs = []string{tval}, true, operands(ops)
	case len(ops) >= 2:
		last := ops[len(ops)-1]
		ca.dests, ca.destIdx[last], ca.srcs = []string{args[last]}, true, operands(ops[:len(ops)-1])
	case name == "ln" && len(ops) == 1:
		ca.dests, ca.srcs = []string{"."}, operands(ops)
	default:
		ca.srcs = operands(ops)
	}
	return ca
}

// checkDest refuses a cp/install/ln destination that is or holds a protected path, whose
// <dest>/<source name> reaches one (`cp -r thrall ~/.agent-jobs`), or, with -T, that is an ancestor.
func (s *pathScope) checkDest(d string, ca copyArgs) string {
	if p, ok := s.hitPath(d, false); ok {
		return protectedBlocked(p)
	}
	if p, ok := s.parentHit(d); ok {
		return protectedBlocked(p)
	}
	if p, ok := s.hitPath(d, true); ok && ca.whole {
		return ancestorBlocked(d, p)
	}
	for _, src := range ca.srcs {
		if p, ok := s.hitPath(d+"/"+filepath.Base(filepath.Clean(src)), true); ok {
			return protectedBlocked(p)
		}
	}
	return ""
}
