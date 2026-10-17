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
// unresolved cd) is a hit when its segments could lead to a protected path from some directory.
func (s *pathScope) hit(w string, tree bool) (string, bool) {
	p := pathOf(w)
	if p == "." && !tree {
		return "", false
	}
	exp, ok := s.expandHome(p)
	if !ok {
		rest := ""
		if _, r, found := strings.Cut(p, "/"); found {
			rest = r
		}
		if s.couldReach(rest, tree) {
			return w, true
		}
		return "", false
	}
	if filepath.IsAbs(exp) {
		return s.match(exp, tree)
	}
	for _, d := range s.dirs {
		if q, ok := s.match(filepath.Join(d, exp), tree); ok {
			return q, true
		}
	}
	if s.dynamic != "" && s.couldReach(exp, tree) {
		return w + " (after cd " + s.dynamic + ")", true
	}
	return "", false
}

// match checks an absolute path, as written (cleaned) and with symlinks resolved.
func (s *pathScope) match(abs string, tree bool) (string, bool) {
	for _, p := range []string{filepath.Clean(abs), resolvePath(abs, "/")} {
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
	exp, ok := s.expandHome(pathOf(w))
	if !ok {
		return "", false
	}
	var cands []string
	if filepath.IsAbs(exp) {
		cands = []string{exp}
	} else {
		for _, d := range s.dirs {
			cands = append(cands, filepath.Join(d, exp))
		}
	}
	for _, c := range cands {
		for _, p := range []string{filepath.Clean(c), resolvePath(c, "/")} {
			for _, q := range s.protected {
				if filepath.Join(p, filepath.Base(q)) == q {
					return q, true
				}
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

// contains returns the protected path that a word of a non-reader mentions anywhere in its text, as
// an absolute path (homes expanded) or relative to a directory a literal cd led to: an option value
// (`dd of=…`, `--file=…`) or inline code (`python3 -c`, `perl -e`, `node -e`).
func (s *pathScope) contains(w string) (string, bool) {
	w = s.expandAllHomes(w)
	for _, q := range s.protected {
		if strings.Contains(w, q) {
			return q, true
		}
		for _, d := range s.dirs[1:] { // the call's own dir is checked by hit on each word
			if rel, err := filepath.Rel(d, q); err == nil && d != q && within(q, d) && strings.Contains(w, rel) {
				return q, true
			}
		}
	}
	return "", false
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
