package guard

import (
	"os"
	"path"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
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
// (path and pattern), LS (path).
var readTools = map[string]bool{"Read": true, "Grep": true, "Glob": true, "LS": true}

// recursiveReaders read (or list) every file under a directory argument (`rg x ~`, `tar c ~`,
// `rsync -a ~ x`, `tree ~`), so an ancestor of the state dir reaches it.
var recursiveReaders = map[string]bool{
	"rg": true, "ag": true, "ack": true, "zip": true, "tar": true, "rsync": true, "tree": true,
}

// where is the working directory a command may run in: every directory it may resolve a relative
// path against, and the first cd target the guard cannot resolve ("" = none).
type where struct {
	dirs       []string
	dynamic    string
	chdirWords []int // the words holding a -C/--chdir directory: checked as the directory, not as a path
}

// stateReadScope is what a job may not read: StateDir minus OwnDir, as configured and with symlinks
// resolved; for the ReadAllow scope, everything ("/") minus the allowed paths. The path words of a
// Bash call resolve as in the protected-path check (pathScope), each command of the top-level script
// against the directories in effect when it runs (at).
type stateReadScope struct {
	*pathScope
	state, own []string
	ownRests   [][]string // the components below StateDir of each own folder (jobs, <id>)
	allow      bool       // the ReadAllow scope: stricter on what the guard cannot resolve
	urls       bool       // the job has a read allowlist: a file URL with a query or a fragment is refused (fileURLs)
	at         map[*simpleCmd]where
}

// cleanForms are p cleaned and, when it differs, with symlinks resolved; ok is false when the
// resolution ran out of its link budget (resolveBounded).
func cleanForms(p string) ([]string, bool) {
	p = filepath.Clean(p)
	forms := []string{p}
	q, ok := resolveBounded(p, "/")
	if q != p {
		forms = append(forms, q)
	}
	return forms, ok
}

func newStateReadScope(scripts []*script, cfg Config) *stateReadScope {
	state, ok := cleanForms(cfg.StateDir)
	unresolved := !ok // where the root lies is unknown: the scope counts as expired (fail closed)
	r := &stateReadScope{allow: cfg.readAllowScope, urls: len(cfg.ReadAllow) > 0, state: state}
	owns := []string{cfg.OwnDir}
	if r.allow {
		owns = cfg.ReadAllow
	}
	for _, o := range owns {
		if o != "" {
			forms, ok := cleanForms(o)
			unresolved = unresolved || !ok
			r.own = append(r.own, forms...)
		}
	}
	for _, o := range r.own {
		if rel, err := filepath.Rel(r.state[0], o); err == nil && within(o, r.state[0]) && rel != "." {
			r.ownRests = append(r.ownRests, strings.Split(filepath.ToSlash(rel), "/"))
		}
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
	r.expired = r.expired || unresolved
	r.sequence(scripts)
	return r
}

// reFuncDef is a shell function definition (`f() {`, `function f`): its body runs where it is called.
var reFuncDef = regexp.MustCompile(`\(\s*\)|(^|[\s;&|({])function\s`)

// sequence records, for each command of the top-level script, the directories in effect when it
// runs: the call's directory and the targets of the cd/pushd commands before it (also when such a cd
// may fail), so `cat ../prompt.md; cd ..` is not resolved against the later cd. Commands of nested
// scripts (substitutions, `sh -c`) keep every directory of the call. When the order of execution is
// not the order of the text (a loop, a function, eval, source, trap, alias), every command does.
func (r *stateReadScope) sequence(scripts []*script) {
	if len(scripts) == 0 || reFuncDef.MatchString(scripts[0].src) {
		return
	}
	for _, sc := range scripts {
		for _, pl := range sc.pipelines {
			for _, c := range pl.cmds {
				if len(c.words) > 0 && sc.depth == 0 && slices.Contains([]string{"for", "while", "until", "select", "do"}, c.words[0]) {
					return
				}
				if argv := argvOf(c.words); len(argv) > 0 {
					switch progName(argv[0]) {
					case "eval", "source", ".", "trap", "alias":
						return
					}
				}
			}
		}
	}
	s := &pathScope{home: r.home, dirs: r.dirs[:1:1]}
	cdpathEnv := os.Getenv("CDPATH") != ""
	r.at = map[*simpleCmd]where{}
	for _, pl := range scripts[0].pipelines {
		for _, c := range pl.cmds {
			r.at[c] = where{dirs: slices.Clone(s.dirs), dynamic: s.dynamic}
			s.step(c, cdpathEnv)
		}
	}
}

// blocked reports an absolute path inside the state dir but outside the job's own folder, or, with
// tree set (a recursive read), an ancestor of the state dir. raw is the path as written, joined to its
// directory without cleaning: for a path with `..` it is also resolved the way the kernel does
// (`link/..` is the parent of the link's target).
func (r *stateReadScope) blocked(abs, raw string, tree bool) (string, bool) {
	cands := []string{filepath.Clean(abs), r.resolve(abs)}
	if len(r.futureLinks) > 0 {
		path := raw
		if path == "" {
			path = abs
		}
		cands = append(cands, r.futurePath(path))
	}
	if raw != "" && hasDotDot(raw) {
		if q, ok := kernelPath(raw); ok {
			cands = append(cands, q, r.resolve(q))
		}
	}
	for _, p := range cands {
		if r.allow && !topExists(p) {
			continue // nothing to read under a top-level name that does not exist
		}
		if inJobGitDir(p, r.state) && !inJobGitDir(p, r.own) {
			return p, true
		}
		if tree && slices.ContainsFunc(r.state, func(s string) bool { return within(s, p) }) {
			return p, true
		}
	}
	return "", false
}

// topExists reports whether the first component of the absolute path p exists ("/" does).
func topExists(p string) bool {
	c := splitPath(p)
	if len(c) == 0 {
		return true
	}
	_, err := os.Lstat(string(filepath.Separator) + c[0])
	return err == nil
}

// blockedMsg is the refusal of what reaching p: agent-jobs state, or for the ReadAllow scope a path
// outside what the job may read.
func (r *stateReadScope) blockedMsg(what, p string) string {
	if r.allow {
		return readAllowBlocked(what, p)
	}
	return stateBlocked(what, p)
}

// wordMode says how a path word is checked: tree for an argument of a recursive reader (an ancestor
// of the state dir counts), dynamic to compare it with the state after a cd the guard cannot resolve,
// strict to refuse a `..` after a component that does not exist yet (an operand; a path-like piece of
// inline code is checked leniently).
type wordMode struct{ tree, dynamic, strict bool }

// rawOf returns a word as written; for an option `--opt=value` its value.
func rawOf(w string) string {
	if strings.HasPrefix(w, "-") {
		if _, v, ok := strings.Cut(w, "="); ok {
			return v
		}
	}
	return w
}

// attachedPaths are the paths a word may carry attached ("" = none): the value after `@` (curl's
// `-d @p`) and, in a short option with its value attached (`-f/p`, `-T~/p`, `-xvf../p`), each suffix that
// may start it (the guard does not know which letter takes the value): the one after the first letter
// and each one starting a path (`/`, `~`, `.` after another character). rawOf sees only `--opt=value`.
func attachedPaths(w string) []string {
	switch {
	case strings.HasPrefix(w, "@") && len(w) > 1:
		return []string{w[1:]}
	case strings.HasPrefix(w, "-") && !strings.HasPrefix(w, "--"):
		var out []string
		for k := 2; k < len(w); k++ {
			if k == 2 || strings.IndexByte("/~.", w[k]) >= 0 && strings.IndexByte("/~.", w[k-1]) < 0 {
				out = append(out, w[k:])
			}
		}
		return out
	}
	return nil
}

// rePct is a percent-encoded byte (`%2F`).
var rePct = regexp.MustCompile(`%[0-9A-Fa-f]{2}`)

// percentDecoded returns w with every %xx decoded, round after round until none is left (`%252F`
// decodes to `%2F`, then to `/`), ok false when it has none: a URL reader (curl, python's urllib) opens
// `file:///tmp%2Fx` as /tmp/x, so the decoded form is checked as well. Each round shortens the word, so
// the rounds are at most a third of its length.
func percentDecoded(w string) (string, bool) {
	if !rePct.MatchString(w) {
		return "", false
	}
	for rePct.MatchString(w) {
		w = percentDecodedOnce(w)
	}
	return w, true
}

// percentDecodedOnce returns w with each %xx decoded once, as curl, wget and python's urllib decode the
// path of a file URL (`%252F` decodes to `%2F`).
func percentDecodedOnce(w string) string {
	return rePct.ReplaceAllStringFunc(w, func(h string) string {
		b, _ := strconv.ParseUint(h[1:], 16, 8)
		return string([]byte{byte(b)})
	})
}

// reFileURL is a file URL in a text, in any case, up to a quote, a blank or a shell operator.
var reFileURL = regexp.MustCompile("(?i)file:([^\\s'\"`<>|&;()]*)")

// fileURLPaths returns the paths of a file URL u (the text after `file:`) as curl, wget and python's
// urllib open it: after an optional host (`file:///p`, `file://localhost/p`, `file:/p`), cut at a raw
// query or fragment and percent-decoded once. curl cuts at the first raw `?` or `#`, urllib only at the
// first raw `#` (it opens `x?q` as is), so both forms are returned when they differ; an encoded
// `%3F`/`%23` stays a filename character. A host with no path has none.
func fileURLPaths(u string) []string {
	curl, _, _ := strings.Cut(u, "#")
	urllib := curl
	if i := strings.IndexByte(curl, '?'); i >= 0 {
		curl = curl[:i]
	}
	cuts := []string{curl}
	if urllib != curl {
		cuts = append(cuts, urllib)
	}
	var out []string
	for _, c := range cuts {
		if rest, ok := strings.CutPrefix(c, "//"); ok {
			i := strings.IndexByte(rest, '/')
			if i < 0 {
				continue
			}
			c = rest[i:]
		}
		out = append(out, percentDecodedOnce(c))
	}
	return out
}

// reDotSegment is a `.` or `..` segment of a URL path.
var reDotSegment = regexp.MustCompile(`(^|/)\.\.?(/|$)`)

// unsafeFileURL returns why a file URL u (the text after `file:`) is refused without a path check (""
// = it is not): still percent-encoded after one decode (`%252F`: a URL reader opens a name with `%2F`
// in it, a full decode would check another path), a `.`/`..` segment as written or decoded once (the
// reader resolves it against a path the guard may see differently), and, with queries (a job with a
// read allowlist), a query or a fragment (`?`, `#`: a URL reader cuts the path there).
func unsafeFileURL(u string, queries bool) string {
	once := percentDecodedOnce(u)
	switch {
	case rePct.MatchString(once):
		return "a file URL still percent-encoded after one decode"
	case reDotSegment.MatchString(u) || reDotSegment.MatchString(once):
		return "a file URL with a dot segment"
	case queries && strings.ContainsAny(once, "?#"):
		return "a file URL with a query or a fragment"
	}
	return ""
}

// fileURLs returns a refusal when a file URL in s reaches the state. It fails closed in every scope: a
// URL still percent-encoded after one decode or with a dot segment is refused, for a job with a read
// allowlist also one with a query or a fragment (a sqlite URI `file:x.db?mode=ro` stays usable in
// other jobs), and the paths of any other, cut at a raw query or fragment as each reader cuts them and
// decoded once, are checked strictly.
func (r *stateReadScope) fileURLs(s string, at where, m wordMode) string {
	if !strings.Contains(strings.ToLower(s), "file:") {
		return ""
	}
	m.strict = true
	for _, u := range reFileURL.FindAllStringSubmatch(s, -1) {
		if why := unsafeFileURL(u[1], r.urls); why != "" {
			return r.blockedMsg("`"+u[0]+"`", u[0]+" ("+why+")")
		}
		for _, p := range fileURLPaths(u[1]) {
			if p == "" {
				continue
			}
			if reason := r.word(p, at, m); reason != "" {
				return reason
			}
		}
	}
	return ""
}

// word returns a refusal when a path word reaches the state from a directory the command may run in.
// A path attached to an option, a file URL and the percent-decoded word are checked too.
func (r *stateReadScope) word(w string, at where, m wordMode) string {
	if r.over() {
		return cannot("state read check exceeded the guard time budget")
	}
	lenient := wordMode{tree: m.tree, dynamic: m.dynamic}
	for _, p := range attachedPaths(w) {
		if reason := r.word(p, at, lenient); reason != "" {
			return reason
		}
	}
	if reason := r.fileURLs(w, at, m); reason != "" {
		return reason
	}
	if d, ok := percentDecoded(w); ok {
		// The decoded word as a text: its path-like pieces (the word itself when it has no separator)
		// and its file URLs; decoded to the end, none of them decodes again.
		if reason := r.text(d, at); reason != "" {
			return reason
		}
	}
	exp, ok := r.expandHome(rawOf(w))
	if !ok {
		p, rest := pathOf(w), ""
		if _, s, found := strings.Cut(p, "/"); found {
			rest = s
		}
		if m.dynamic && (r.allow || r.couldReach(rest, m.tree)) {
			return r.blockedMsg("`"+w+"`", w+" (a word the guard cannot expand)")
		}
		return ""
	}
	var raws []string
	if filepath.IsAbs(exp) {
		raws = []string{exp}
	} else {
		for _, d := range at.dirs {
			raws = append(raws, d+"/"+exp)
		}
	}
	for _, raw := range raws {
		if r.extractedLinkPath(raw) {
			return r.blockedMsg("`"+w+"`", w+" (through a link the extraction may create)")
		}
		if hasGlob(exp) {
			if reason := r.glob("`"+w+"`", raw, m.tree); reason != "" {
				return reason
			}
			continue
		}
		if hasDotDot(raw) {
			if _, ok := kernelPath(raw); !ok && m.strict {
				return dotDotBlocked("`" + w + "`")
			}
		}
		if q, ok := r.blocked(raw, raw, m.tree); ok {
			return r.blockedMsg("`"+w+"`", q)
		}
	}
	if m.dynamic && at.dynamic != "" && (r.allow && !filepath.IsAbs(exp) || r.couldReach(filepath.Clean(exp), m.tree)) {
		return r.blockedMsg("`"+w+"`", w+" (after cd "+at.dynamic+")")
	}
	return ""
}

// glob returns a refusal when a glob pattern (absolute, as written) can match a path in the state
// outside the job's own folder (with tree, also an ancestor of the state dir): a wildcard component
// at or above the state dir that can stand for one of its components, or an existing match (also
// through a symlink). The literal part before the first wildcard resolves like a path; a `..` after
// a wildcard, or after a component that does not exist yet, is refused, not modelled.
func (r *stateReadScope) glob(what, pat string, tree bool) string {
	comps := strings.Split(filepath.ToSlash(pat), "/")
	wi := slices.IndexFunc(comps, hasGlob)
	if wi < 0 {
		wi = len(comps)
	}
	var rest []string
	for _, c := range comps[wi:] {
		if c == ".." {
			return dotDotBlocked(what + " (after a wildcard)")
		}
		if c != "" && c != "." {
			rest = append(rest, c)
		}
	}
	lit := "/" + strings.Join(comps[:wi], "/")
	forms := []string{filepath.Clean(lit), r.resolve(lit)}
	if hasDotDot(lit) {
		k, ok := kernelPath(lit)
		if !ok {
			return dotDotBlocked(what)
		}
		forms = append(forms, k, r.resolve(k))
	}
	for _, f := range forms {
		full := path.Join(append([]string{filepath.ToSlash(f)}, rest...)...)
		pc := splitPath(full)
		for _, s := range r.state {
			if globReach(pc, splitPath(s), r.ownRests, tree) && (!r.allow || topCanMatch(pc)) {
				return r.blockedMsg(what, full)
			}
		}
		matches, _ := filepath.Glob(full)
		for _, m := range matches {
			if q, ok := r.blocked(m, "", tree); ok {
				return r.blockedMsg(what, q)
			}
		}
	}
	return ""
}

// splitPath returns the components of an absolute clean path.
func splitPath(p string) []string {
	p = strings.Trim(filepath.ToSlash(p), "/")
	if p == "" {
		return nil
	}
	return strings.Split(p, "/")
}

// topCanMatch reports whether the first component of the pattern components pat can stand for an
// existing top-level name (a wildcard one can).
func topCanMatch(pat []string) bool {
	return len(pat) == 0 || hasGlob(pat[0]) || topExists("/"+pat[0])
}

// globReach reports whether the pattern components pat can match the state dir s, a path inside it
// that is not inside an own folder (ownRests: their components below s), or, with tree, an ancestor
// of s. A brace or `**` component can stand for anything.
func globReach(pat, s []string, ownRests [][]string, tree bool) bool {
	for ; len(s) > 0; pat, s = pat[1:], s[1:] {
		if len(pat) == 0 {
			return tree
		}
		if pat[0] == "**" {
			return true
		}
		if !globComp(pat[0], s[0]) {
			return false
		}
	}
	return !slices.ContainsFunc(ownRests, func(own []string) bool { return ownCovers(pat, own) })
}

// ownCovers reports whether the pattern components pat stay inside the own folder whose components are
// own: its literal first components are own's.
func ownCovers(pat, own []string) bool {
	if len(own) == 0 || len(pat) < len(own) {
		return false
	}
	for i, o := range own {
		if hasGlob(pat[i]) || pat[i] != o {
			return false
		}
	}
	return true
}

// globComp reports whether one pattern component can match a name (a brace or a malformed pattern can).
func globComp(p, name string) bool {
	if strings.Contains(p, "{") {
		return true
	}
	ok, err := path.Match(p, name)
	return ok || err != nil
}

// tokens splits a word into the path-like pieces of its text: an option value (`if=…`), inline code
// (`python3 -c "open('…')"`), an assignment (`F=…`), a here-document body.
func tokens(w string) []string {
	return literalTokens(w, " \t\r\n`=:,;()[]{}<>|&")
}

// text returns a refusal when a path-like piece of a text, or the path of a file URL in it, names the
// state.
func (r *stateReadScope) text(w string, at where) string {
	if reason := r.fileURLs(w, at, wordMode{}); reason != "" {
		return reason
	}
	for _, t := range tokens(w) {
		if reason := r.word(t, at, wordMode{}); reason != "" {
			return reason
		}
	}
	return ""
}

// longOpt reports a long option word (`--name` or `--name=value`, also abbreviated to at least min
// characters) for one of names, and returns its value when attached.
func longOpt(w string, min int, names ...string) (val string, attached, ok bool) {
	opt, val, attached := strings.Cut(w, "=")
	for _, n := range names {
		if opt == n || (len(opt) >= min && strings.HasPrefix(n, opt)) {
			return val, attached, true
		}
	}
	return "", false, false
}

// grepRecurse reports a `-d`/`--directories` value that may mean recurse (GNU accepts abbreviations;
// anything that is not read or skip counts).
func grepRecurse(v string) bool {
	return v == "" || !(strings.HasPrefix("read", v) || strings.HasPrefix("skip", v))
}

// recursive reports a call that reads every file under its directory arguments: grep -r/-R, also as
// `-d recurse`, `--directories=recurse` and abbreviated long options; rg, tar, rsync, …; cp -r/-a,
// ls -R; find in any form (it lists every name below its start points and feeds -exec and xargs).
func recursive(name string, args []string) bool {
	switch {
	case recursiveReaders[name], name == "find":
		return true
	case name == "cp" || name == "ls":
		flags := "rRa"
		if name == "ls" {
			flags = "R"
		}
		return slices.ContainsFunc(args, func(a string) bool {
			if strings.HasPrefix(a, "--") {
				// GNU accepts any unambiguous prefix: `--rec` (ls has --reverse), cp `--ar`
				if _, _, ok := longOpt(a, len("--rec"), "--recursive"); ok {
					return true
				}
				_, _, ok := longOpt(a, len("--ar"), "--archive")
				return ok && name == "cp"
			}
			return strings.HasPrefix(a, "-") && strings.ContainsAny(a, flags)
		})
	case strings.HasSuffix(name, "grep"):
		for i, a := range args {
			next := ""
			if i+1 < len(args) {
				next = args[i+1]
			}
			if strings.HasPrefix(a, "--") {
				if _, _, ok := longOpt(a, 3, "--recursive", "--dereference-recursive"); ok {
					return true
				}
				if v, attached, ok := longOpt(a, 4, "--directories"); ok {
					if !attached {
						v = next
					}
					if grepRecurse(v) {
						return true
					}
				}
				continue
			}
			if !strings.HasPrefix(a, "-") || len(a) < 2 {
				continue
			}
			if strings.ContainsAny(a, "rR") {
				return true
			}
			if k := strings.IndexByte(a, 'd'); k > 0 {
				v := a[k+1:]
				if v == "" {
					v = next
				}
				if grepRecurse(v) {
					return true
				}
			}
		}
	}
	return false
}

// chdirShort are programs that run in a directory given by `-C dir` (git: a global option).
var chdirShort = map[string]bool{
	"git": true, "make": true, "gmake": true, "tar": true, "go": true, "ninja": true,
	"samu": true, "meson": true, "pnpm": true,
}

// chdirLong are long options that move any program into another directory.
var chdirLong = []string{"--chdir", "--cwd", "--prefix", "--dir", "--working-directory", "--workdir", "--wd"}

// chdirs returns the directories (as written, in order) a command runs in instead of the shell's
// working directory: `env -C|--chdir D`, `sudo -D|--chdir D`, `git -C D` (global options),
// `make|tar|go|ninja|… -C D` or `--directory D`, and `--chdir`, `--cwd`, `--prefix`, `--dir`, … of any
// program, with the indices of the words that hold them. unmodeled names a form the guard does not
// resolve: a directory from an expansion, chroot.
func chdirs(c *simpleCmd) (vals []string, idx []int, unmodeled string) {
	words := c.words
	add := func(i int, v string) {
		idx = append(idx, i)
		if c.dyn[i] || hasGlob(v) {
			if unmodeled == "" {
				unmodeled = v
			}
			return
		}
		vals = append(vals, v)
	}
	// value reads the value of option i: attached, or the next word (returns the index of the last word used).
	value := func(i int, v string, attached bool) int {
		if attached {
			add(i, v)
			return i
		}
		if i+1 < len(words) {
			add(i+1, words[i+1])
		}
		return i + 1
	}
	v := split(words)
	for i := v.lead; i < v.start; {
		name := progName(words[i])
		vf := wrapperValueFlags[name]
		target := map[string]byte{"env": 'C', "sudo": 'D'}[name]
		for i++; i < v.start; i++ {
			w := words[i]
			if w == "--" || !strings.HasPrefix(w, "-") || len(w) < 2 {
				if w == "--" {
					i++
				}
				break
			}
			if strings.HasPrefix(w, "--") {
				val, attached, isChdir := longOpt(w, len(w)+1, "--chdir") // exact: split refuses env abbreviations
				switch {
				case isChdir && target != 0:
					i = value(i, val, attached)
				case name == "env" && w == "--unset":
					i++ // its value
				}
				continue
			}
			for k := 1; k < len(w); k++ {
				if strings.IndexByte(vf, w[k]) < 0 {
					continue
				}
				if w[k] == target {
					i = value(i, w[k+1:], k+1 < len(w))
				} else if k+1 == len(w) {
					i++
				}
				break
			}
		}
		for i < v.start && name == "env" && (reAssign.MatchString(words[i]) || words[i] == "-") {
			i++
		}
		if name == "timeout" && i < v.start {
			i++ // duration
		}
	}
	argv := words[v.start:]
	if len(argv) == 0 {
		return vals, idx, unmodeled
	}
	name := progName(argv[0])
	if name == "chroot" {
		return vals, idx, "chroot"
	}
	for j := 1; j < len(argv); j++ {
		i, a := v.start+j, argv[j]
		if name == "git" && !strings.HasPrefix(a, "-") {
			break // global options end at the subcommand
		}
		if val, attached, ok := longOpt(a, len(a)+1, chdirLong...); ok {
			j = value(i, val, attached) - v.start
			continue
		}
		if !chdirShort[name] {
			continue
		}
		switch {
		case strings.HasPrefix(a, "-C"):
			j = value(i, a[2:], len(a) > 2) - v.start
		case name != "git" && strings.HasPrefix(a, "--"):
			if val, attached, ok := longOpt(a, 5, "--directory"); ok {
				j = value(i, val, attached) - v.start
			}
		case name == "git" && gitValueOpts[a]:
			j++
		}
	}
	return vals, idx, unmodeled
}

// whereOf returns the directories a command resolves its operands against (the shell's directory in
// effect, moved by its own -C/--chdir options), or a refusal for a working directory the guard does
// not resolve or one inside the state.
func (r *stateReadScope) whereOf(c *simpleCmd) (where, string) {
	at, ok := r.at[c]
	if !ok {
		at = where{dirs: r.dirs, dynamic: r.dynamic}
	}
	vals, idx, unmodeled := chdirs(c)
	at.chdirWords = idx
	if unmodeled != "" {
		return at, "blocked by guard: `" + unmodeled + "` moves the command into a directory the guard does not resolve, " +
			"so it cannot tell whether the command reads agent-jobs state; write the directory literally."
	}
	for _, v := range vals {
		p, ok := r.expandHome(v)
		if !ok {
			return at, "blocked by guard: `" + v + "` moves the command into a directory the guard does not resolve, " +
				"so it cannot tell whether the command reads agent-jobs state; write the directory literally."
		}
		dirs := slices.Clone(at.dirs)
		for _, d := range at.dirs {
			raw := p
			if !filepath.IsAbs(raw) {
				raw = d + "/" + p
			}
			n := r.resolve(raw)
			if hasDotDot(raw) {
				k, ok := kernelPath(raw)
				if !ok {
					return at, dotDotBlocked("`" + v + "`")
				}
				n = k
			}
			if q, bad := r.blocked(n, "", false); bad {
				return at, r.blockedMsg("the working directory `"+v+"`", q)
			}
			if !slices.Contains(dirs, n) && len(dirs) < maxScopeDirs {
				dirs = append(dirs, n)
			}
		}
		at.dirs = dirs
	}
	return at, ""
}

// symlinkOps returns, for `ln -s` and `cp -s|--symbolic-link`, the argv indices of the link targets
// and the directories (as written) their links are created in: definite (relative targets resolve
// against them) and speculative (the last operand when it does not exist yet: a directory the call
// may create).
func symlinkOps(argv []string, dirExists func(string) bool) (targets []int, definite, speculative []string) {
	name := progName(argv[0])
	if name != "ln" && name != "cp" {
		return nil, nil, nil
	}
	sym, tdir := false, ""
	var ops []int
	for i := 1; i < len(argv); i++ {
		a := argv[i]
		switch {
		case a == "--":
			for i++; i < len(argv); i++ {
				ops = append(ops, i)
			}
		case strings.HasPrefix(a, "--"):
			if name == "ln" && a == "--symbolic" || name == "cp" && a == "--symbolic-link" {
				sym = true
			}
			if v, attached, ok := longOpt(a, len(a)+1, "--target-directory", "--suffix"); ok {
				if !attached && i+1 < len(argv) {
					i++
					v = argv[i]
				}
				if strings.HasPrefix(a, "--target-directory") {
					tdir = v
				}
			}
		case strings.HasPrefix(a, "-") && len(a) > 1:
			for k := 1; k < len(a); k++ {
				if a[k] == 's' {
					sym = true
				}
				if a[k] == 't' || a[k] == 'S' {
					v := a[k+1:]
					if v == "" && i+1 < len(argv) {
						i++
						v = argv[i]
					}
					if a[k] == 't' {
						tdir = v
					}
					break
				}
			}
		default:
			ops = append(ops, i)
		}
	}
	if !sym || len(ops) == 0 {
		return nil, nil, nil
	}
	switch {
	case tdir != "":
		return ops, []string{tdir}, nil
	case len(ops) == 1:
		return ops, []string{"."}, nil
	}
	last := argv[ops[len(ops)-1]]
	if dirExists(last) {
		return ops[:len(ops)-1], []string{last}, nil
	}
	return ops[:len(ops)-1], []string{filepath.Dir(last)}, []string{last}
}

// checkSymlinks refuses a symlink the call creates whose target resolves into the state outside the
// job's own folder or to an ancestor of the state dir (a later `L/thrall/jobs.db` in the same call
// does not exist yet, so it is not resolved), and, in a call that creates a symlink, any other path
// with `..`: `ln -s .. L; cat L/../k/…` (the kernel resolves `..` after the link, which the guard
// cannot see before it exists). A link target with `..` is resolved against the link's directory.
func (r *stateReadScope) checkSymlinks(scripts []*script) string {
	exempt := map[*simpleCmd]map[int]bool{}
	for _, sc := range scripts {
		for _, pl := range sc.pipelines {
			for _, c := range pl.cmds {
				at, _ := r.whereOf(c)
				start := split(c.words).start
				argv := c.words[start:]
				if len(argv) == 0 {
					continue
				}
				dirExists := func(w string) bool {
					p, ok := r.expandHome(w)
					return ok && slices.ContainsFunc(at.dirs, func(d string) bool {
						if !filepath.IsAbs(p) {
							p = filepath.Join(d, p)
						}
						fi, err := os.Stat(p)
						return err == nil && fi.IsDir()
					})
				}
				targets, definite, speculative := symlinkOps(argv, dirExists)
				if targets == nil {
					continue
				}
				exempt[c] = map[int]bool{}
				for _, ti := range targets {
					exempt[c][start+ti] = true
					w := argv[ti]
					t, ok := r.expandHome(w)
					if !ok {
						return r.blockedMsg("the symlink target `"+w+"`", w)
					}
					// under places a link directory (or link) in each working directory; an absolute one
					// stays as written (`ln -s . ~/L` links to $HOME, not to <cwd>/$HOME).
					under := func(links []string) (out []string) {
						for _, l := range links {
							lp, ok := r.expandHome(l)
							if !ok {
								continue
							}
							if filepath.IsAbs(lp) {
								out = append(out, lp+"/"+t)
								continue
							}
							for _, d := range at.dirs {
								out = append(out, filepath.Join(d, lp)+"/"+t)
							}
						}
						return out
					}
					raws, lexical := under(definite), under(speculative)
					if filepath.IsAbs(t) {
						raws, lexical = []string{t}, nil
					}
					for _, raw := range raws {
						if hasDotDot(raw) {
							if _, ok := kernelPath(raw); !ok {
								return dotDotBlocked("the symlink target `" + w + "`")
							}
						}
						if q, bad := r.blocked(raw, raw, true); bad {
							return r.blockedMsg("the symlink target `"+w+"`", q)
						}
					}
					for _, raw := range lexical {
						if q, bad := r.blocked(filepath.Clean(raw), "", true); bad {
							return r.blockedMsg("the symlink target `"+w+"`", q)
						}
					}
				}
			}
		}
	}
	if len(exempt) == 0 {
		return ""
	}
	for _, sc := range scripts {
		for _, pl := range sc.pipelines {
			for _, c := range pl.cmds {
				for i, w := range append(append(slices.Clone(c.words), c.inRedirs...), c.outRedirs...) {
					if !exempt[c][i] && hasDotDot(rawOf(w)) {
						return dotDotBlocked("`" + w + "` in a call that creates a symlink")
					}
				}
			}
		}
	}
	return ""
}

func readAllowBlocked(what, p string) string {
	return "blocked by guard: " + what + " reaches `" + p + "`, outside what this job may read (its own job folder " +
		"with its work dir, its session dir and the system dirs programs run from); a gate job gets nothing but its input."
}

func stateBlocked(what, p string) string {
	return "blocked by guard: " + what + " reaches `" + p + "`, agent-jobs state (the job database, other jobs' " +
		"folders, the archive, the config, the shared checklist locks); a job reads only its own folder and its work."
}

func dotDotBlocked(what string) string {
	return "blocked by guard: " + what + " contains `..` that the guard cannot resolve (after a component that does " +
		"not exist yet or after a wildcard; the kernel resolves `..` after a symlink), so it may reach agent-jobs " +
		"state; write the path without `..`, or create the directory in an earlier call."
}

// checkStateRead rejects Bash calls that name the agent state outside the job's own folder: any word
// of any command (readers included: cat, grep -a, head, od, xxd, base64, cp …), a path-like piece of
// a word or here-document (inline code, option values, assignments), a redirection in either
// direction, a glob that can match the state, and, for a recursive reader (grep -r, rg, find, tar …)
// or the input of xargs, an ancestor of the state dir, also as its working directory. Each command
// resolves relative paths against the directories in effect when it runs (cd before it, its own
// -C/--chdir). Paths resolve as in checkProtected; a path built at run time and a script the job
// writes are the documented residual gap.
func checkStateRead(scripts []*script, _ string, cfg Config) string {
	for _, c := range readScopes(cfg) {
		if reason := checkStateReadIn(scripts, c); reason != "" {
			return reason
		}
	}
	return ""
}

// readScopes are the configurations of the state-read check: cfg (its StateDir and OwnDir) when it
// has a state dir, then one per NoRead directory, as a state dir without an own folder, then, with
// ReadAllow, the whole file system ("/") with the allowed paths as its own folders.
func readScopes(cfg Config) []Config {
	var out []Config
	if cfg.StateDir != "" {
		out = append(out, cfg)
	}
	for _, d := range cfg.NoRead {
		c := cfg
		c.StateDir, c.OwnDir, c.NoRead = d, "", nil // ReadAllow kept: it marks a job with a read allowlist
		out = append(out, c)
	}
	if len(cfg.ReadAllow) > 0 {
		c := cfg
		c.StateDir, c.OwnDir, c.NoRead, c.readAllowScope = "/", "", nil, true
		out = append(out, c)
	}
	return out
}

func checkStateReadIn(scripts []*script, cfg Config) string {
	r := newStateReadScope(scripts, cfg)
	if r.expired {
		return cannot("symlink preservation is too complex to verify within the guard's time budget")
	}
	// A cd/pushd target is checked as the directory it leads to (pathScope.dirs), not as a word
	// resolved against the directories later cds lead to (`cd .. && cat prompt.md`).
	for _, d := range r.dirs[1:] {
		if p, ok := r.blocked(d, "", false); ok {
			return r.blockedMsg("cd", p)
		}
	}
	if reason := r.checkSymlinks(scripts); reason != "" {
		return reason
	}
	for _, sc := range scripts {
		for _, pl := range sc.pipelines {
			for i, c := range pl.cmds {
				if r.over() {
					return cannot("state read check exceeded the guard time budget")
				}
				r.useLinks(c)
				at, reason := r.whereOf(c)
				if reason != "" {
					return reason
				}
				v := split(c.words)
				argv := c.words[v.start:]
				name := ""
				if len(argv) > 0 {
					name = progName(argv[0])
				}
				tree := recursive(name, argv)
				if tree { // `cd ~ && rg x`, `grep -r x` with no path: the working directory is read
					for _, d := range at.dirs {
						if p, ok := r.blocked(d, "", true); ok {
							return r.blockedMsg("`"+name+"` in `"+d+"`", p)
						}
					}
				}
				for _, w := range append(slices.Clone(c.inRedirs), c.outRedirs...) {
					if reason := r.word(w, at, wordMode{dynamic: true, strict: true}); reason != "" {
						return reason
					}
				}
				if r.allow {
					// An expansion is resolved only when the command runs: under an allowlist, no word of a
					// command or redirection may come from one.
					for k, w := range c.words {
						if k < len(c.dyn) && c.dyn[k] && !((name == "cd" || name == "pushd") && k > v.start) {
							return readAllowBlocked("`"+w+"`", w+" (an expansion the guard cannot resolve)")
						}
					}
					for k, w := range c.outRedirs {
						if k < len(c.outDyn) && c.outDyn[k] || strings.ContainsAny(w, "$`") {
							return readAllowBlocked("`"+w+"`", w+" (an expansion the guard cannot resolve)")
						}
					}
					for _, w := range c.inRedirs {
						if strings.ContainsAny(w, "$`") {
							return readAllowBlocked("`"+w+"`", w+" (an expansion the guard cannot resolve)")
						}
					}
				}
				words := c.words
				if name == "cd" || name == "pushd" {
					words = c.words[:v.start+1] // prefix and the name: the target is in r.dirs
				}
				for k, w := range words {
					if slices.Contains(at.chdirWords, k) {
						continue
					}
					if reason := r.word(w, at, wordMode{tree: tree && !strings.HasPrefix(w, "-"), dynamic: true, strict: true}); reason != "" {
						return reason
					}
					if reason := r.text(w, at); reason != "" {
						return reason
					}
				}
				if reason := r.text(c.heredoc, at); reason != "" && !dataHeredoc(sc, pl, c) {
					return reason
				}
				// xargs runs its command on what earlier stages print: a stage naming an ancestor of the
				// state dir (`find ~ | xargs cat`, `echo ~/.agent-jobs | xargs ls`) feeds it state paths.
				if slices.Contains(v.wrappers, "xargs") {
					inputs, _ := commandInputWords(c, nil, r.over)
					for _, w := range inputs {
						if strings.HasPrefix(w, "-") {
							continue
						}
						if reason := r.word(strings.TrimSpace(w), at, wordMode{tree: true, dynamic: true}); reason != "" {
							return reason
						}
					}
					for _, prev := range append(slices.Clone(pl.cmds[:i]), c.incoming...) {
						pat, _ := r.whereOf(prev)
						for _, w := range feederWords(prev, r.over) {
							if strings.HasPrefix(w, "-") {
								continue
							}
							if reason := r.word(w, pat, wordMode{tree: true, dynamic: true}); reason != "" {
								return "blocked by guard: xargs reads the output of `" + w + "`: " + strings.TrimPrefix(reason, "blocked by guard: ")
							}
						}
					}
				}
			}
		}
	}
	if r.over() {
		return cannot("state read check exceeded the guard time budget")
	}
	return ""
}

// CheckFileRead returns a non-empty reason when a read tool (Read, Grep, Glob) would read the agent
// state outside the job's own folder. Grep reads a directory recursively: an ancestor of the state
// dir is refused for it too. A Glob pattern is refused when a wildcard component at or above the
// state dir (or `**`) can match it, or when an existing match lies in the state.
func CheckFileRead(tool, path, pattern string, cfg Config) string {
	for _, c := range readScopes(cfg) {
		if reason := checkFileReadIn(tool, path, pattern, c); reason != "" {
			return reason
		}
	}
	return ""
}

func checkFileReadIn(tool, path, pattern string, cfg Config) string {
	r := newStateReadScope(nil, cfg)
	if path == "" {
		path = "."
	}
	raw := path
	if !filepath.IsAbs(path) {
		raw = cfg.Dir + "/" + path
	}
	if tool == "Glob" && pattern != "" {
		pat := pattern
		if !filepath.IsAbs(pattern) {
			pat = raw + "/" + pattern
		}
		if hasGlob(pat) {
			if reason := r.glob(tool+" of `"+pattern+"` in `"+path+"`", pat, false); reason != "" {
				return reason
			}
			return r.unverified(tool + " of `" + pattern + "` in `" + path + "`")
		}
		raw = pat
	}
	if hasDotDot(raw) {
		if _, ok := kernelPath(raw); !ok {
			return dotDotBlocked(tool + " of `" + path + "`")
		}
	}
	if p, ok := r.blocked(raw, raw, tool == "Grep"); ok {
		return r.blockedMsg(tool+" of `"+path+"`", p)
	}
	return r.unverified(tool + " of `" + path + "`")
}

// unverified refuses what when a path of the check (the state dir or a target) took more than the
// symlink resolution budget to resolve: where it lies is unknown, so the read is not allowed.
func (r *stateReadScope) unverified(what string) string {
	if !r.expired {
		return ""
	}
	return "blocked by guard: " + what + " runs through more than " + strconv.Itoa(maxLinks) + " symlinks or " +
		"symlink targets of more than " + strconv.Itoa(maxResolveSteps) + " path components (the path or a " +
		"directory the job does not read); the guard cannot verify it. Write the real path."
}
