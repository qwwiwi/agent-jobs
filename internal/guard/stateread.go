package guard

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
)

// StateLayout derives the agent state dir and the job's own folder from the job's subagent log,
// <state>/subagents/<id>.log, with the job's folder at <state>/jobs/<id> (the layout of jobs.Manager:
// SubagentLogPath, JobDir). ok is false for a log path of any other shape.
func StateLayout(subagentLog string) (state, own string, ok bool) {
	if subagentLog == "" || !filepath.IsAbs(subagentLog) {
		return "", "", false
	}
	p := filepath.Clean(subagentLog)
	id, found := strings.CutSuffix(filepath.Base(p), ".log")
	logs := filepath.Dir(p)
	if !found || id == "" || filepath.Base(logs) != "subagents" {
		return "", "", false
	}
	state = filepath.Dir(logs)
	return state, filepath.Join(state, "jobs", id), true
}

// readTools read the files named in their input: Read (file_path), Grep (path, recursive), Glob
// (path and pattern).
var readTools = map[string]bool{"Read": true, "Grep": true, "Glob": true}

// recursiveReaders read every file under a directory argument (`grep -r x ~`, `rg x ~`), so an
// ancestor of the state dir reaches it.
var recursiveReaders = map[string]bool{"rg": true, "ag": true, "ack": true, "zip": true}

// stateReadScope is what a job may not read: StateDir minus OwnDir, as configured and with symlinks
// resolved. The path words of a Bash call resolve as in the protected-path check (pathScope).
type stateReadScope struct {
	*pathScope
	state, own []string
}

func newStateReadScope(scripts []*script, cfg Config) *stateReadScope {
	r := &stateReadScope{}
	for _, p := range []string{cfg.StateDir, cfg.OwnDir} {
		p = filepath.Clean(p)
		forms := []string{p}
		if q := resolvePath(p, "/"); q != p {
			forms = append(forms, q)
		}
		if r.state == nil {
			r.state = forms
		} else {
			r.own = forms
		}
	}
	if cfg.OwnDir == "" {
		r.own = nil
	}
	// The protected set of the scope is the state dir and its entries: after a cd the guard cannot
	// resolve, a relative word naming one of them (`jobs.db`, `jobs/k`) is refused.
	inner := cfg
	inner.Protected = slices.Clone(r.state)
	if entries, err := os.ReadDir(r.state[0]); err == nil {
		for _, e := range entries {
			inner.Protected = append(inner.Protected, filepath.Join(r.state[0], e.Name()))
		}
	}
	r.pathScope = newPathScope(scripts, inner)
	return r
}

// blocked reports an absolute path inside the state dir but outside the job's own folder, or, with
// tree set (a recursive read), an ancestor of the state dir. raw is the path as written, joined to its
// directory without cleaning: when it exists, it is also resolved the way the kernel does (`link/..`).
func (r *stateReadScope) blocked(abs, raw string, tree bool) (string, bool) {
	cands := []string{filepath.Clean(abs), resolvePath(abs, "/")}
	if raw != "" && hasDotDot(raw) {
		if q, err := filepath.EvalSymlinks(raw); err == nil {
			cands = append(cands, q)
		}
	}
	for _, p := range cands {
		if inJobGitDir(p, r.state) && !inJobGitDir(p, r.own) {
			return p, true
		}
		if tree && slices.ContainsFunc(r.state, func(s string) bool { return within(s, p) }) {
			return p, true
		}
	}
	return "", false
}

// word returns the state path a path word reaches from any directory the call may be in.
func (r *stateReadScope) word(w string, tree, dynamic bool) (string, bool) {
	p := pathOf(w)
	exp, ok := r.expandHome(p)
	if !ok {
		rest := ""
		if _, s, found := strings.Cut(p, "/"); found {
			rest = s
		}
		if dynamic && r.couldReach(rest, tree) {
			return w, true
		}
		return "", false
	}
	if filepath.IsAbs(exp) {
		return r.blocked(exp, exp, tree)
	}
	for _, d := range r.dirs {
		if q, ok := r.blocked(filepath.Join(d, exp), d+"/"+exp, tree); ok {
			return q, true
		}
	}
	if dynamic && r.dynamic != "" && r.couldReach(exp, tree) {
		return w + " (after cd " + r.dynamic + ")", true
	}
	return "", false
}

// tokens splits a word into the path-like pieces of its text: an option value (`if=…`), inline code
// (`python3 -c "open('…')"`), an assignment (`F=…`), a here-document body.
func tokens(w string) []string {
	return strings.FieldsFunc(w, func(c rune) bool {
		return c <= ' ' || strings.ContainsRune("'\"`=:,;()[]{}<>|&", c)
	})
}

// text returns the state path any path-like piece of a text names.
func (r *stateReadScope) text(w string) (string, bool) {
	for _, t := range tokens(w) {
		if q, ok := r.word(t, false, false); ok {
			return q, true
		}
	}
	return "", false
}

// recursive reports a call that reads every file under its directory arguments.
func recursive(name string, args []string) bool {
	switch {
	case recursiveReaders[name]:
		return true
	case name == "find":
		return findActs(args)
	case strings.HasSuffix(name, "grep"):
		return slices.ContainsFunc(args, func(a string) bool {
			if strings.HasPrefix(a, "--") {
				return a == "--recursive" || a == "--dereference-recursive" || a == "--directories=recurse"
			}
			return strings.HasPrefix(a, "-") && strings.ContainsAny(a, "rR")
		})
	}
	return false
}

func stateBlocked(what, p string) string {
	return "blocked by guard: " + what + " reaches `" + p + "`, agent-jobs state (the job database, other jobs' " +
		"folders, the archive, the config); a job reads only its own folder and its work."
}

// checkStateRead rejects Bash calls that name the agent state outside the job's own folder: any word
// of any command (readers included: cat, grep -a, head, od, xxd, base64, cp …), a path-like piece of
// a word or here-document (inline code, option values, assignments), a redirection in either
// direction, and, for a recursive reader (grep -r, rg, find with an action), an ancestor of the state
// dir. Paths resolve as in checkProtected; a path built at run time and a script the job writes are
// the documented residual gap.
func checkStateRead(scripts []*script, _ string, cfg Config) string {
	if cfg.StateDir == "" {
		return ""
	}
	r := newStateReadScope(scripts, cfg)
	// A cd/pushd target is checked as the directory it leads to (pathScope.dirs), not as a word
	// resolved against the directories later cds lead to (`cd .. && cat prompt.md`).
	for _, d := range r.dirs[1:] {
		if p, ok := r.blocked(d, "", false); ok {
			return stateBlocked("cd", p)
		}
	}
	for _, sc := range scripts {
		for _, pl := range sc.pipelines {
			for _, c := range pl.cmds {
				argv := argvOf(c.words)
				name := ""
				if len(argv) > 0 {
					name = strings.ToLower(base(argv[0]))
				}
				tree := recursive(name, argv)
				for _, w := range append(slices.Clone(c.inRedirs), c.outRedirs...) {
					if p, ok := r.word(w, false, true); ok {
						return stateBlocked("a redirection", p)
					}
				}
				words := c.words
				if name == "cd" || name == "pushd" {
					words = c.words[:len(c.words)-len(argv)+1] // prefix and the name: the target is in r.dirs
				}
				for _, w := range words {
					if p, ok := r.word(w, tree && !strings.HasPrefix(w, "-"), true); ok {
						return stateBlocked("`"+w+"`", p)
					}
					if p, ok := r.text(w); ok {
						return stateBlocked("`"+w+"`", p)
					}
				}
				if p, ok := r.text(c.heredoc); ok {
					return stateBlocked("a here-document", p)
				}
			}
		}
	}
	return ""
}

// CheckFileRead returns a non-empty reason when a read tool (Read, Grep, Glob) would read the agent
// state outside the job's own folder. Grep reads a directory recursively, as does a Glob pattern with
// `**`: an ancestor of the state dir is refused for them too.
func CheckFileRead(tool, path, pattern string, cfg Config) string {
	if cfg.StateDir == "" {
		return ""
	}
	r := newStateReadScope(nil, cfg)
	tree := tool == "Grep" || strings.Contains(pattern, "**")
	if tool == "Glob" {
		// The literal directory part of the pattern (up to the first wildcard) is where it reads.
		lit := pattern
		if i := strings.IndexAny(lit, "*?[{"); i >= 0 {
			lit = lit[:i]
		}
		if i := strings.LastIndexByte(lit, '/'); i >= 0 {
			lit = lit[:i+1]
		} else {
			lit = ""
		}
		switch {
		case filepath.IsAbs(lit):
			path = lit
		case lit != "" && path != "":
			path = path + "/" + lit
		case lit != "":
			path = lit
		}
	}
	if path == "" {
		path = "."
	}
	abs, raw := path, path
	if !filepath.IsAbs(path) {
		abs, raw = filepath.Join(cfg.Dir, path), cfg.Dir+"/"+path
	}
	if p, ok := r.blocked(abs, raw, tree); ok {
		return stateBlocked(tool+" of `"+path+"`", p)
	}
	return ""
}
