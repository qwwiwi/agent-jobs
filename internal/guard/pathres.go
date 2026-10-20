package guard

import (
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"
)

// Path resolution for the protected-path check: which file system paths a word of a Bash command can
// name. The guard does not run the command, so it over-approximates: a relative word is resolved
// against every directory the command may be in (the call's directory and each literal cd/pushd
// target, also when that cd may fail), `~/` and $HOME are expanded, and when a cd target cannot be
// resolved (a variable, `cd -`, CDPATH) a relative word is compared by its segments with the tail of
// each protected path ("could it reach it from some directory").

// maxScopeDirs bounds the directories tracked through cd chains; beyond it the directory is unknown.
const maxScopeDirs = 64

// A code word longer than maxWordBytes (PATH_MAX: the kernel opens no such path) that splits into
// several path tokens (inline code, a here-doc) is not resolved as one path: its tokens are.
const maxWordBytes = 4096

// protectedBudget is the time the protected-path check may take for one call; past it the call is
// refused as too complex to verify (fail closed, independent of the hook runner's timeout).
var protectedBudget = 5 * time.Second

// pathScope is what the guard knows about where the paths of one Bash call resolve.
type pathScope struct {
	home      string   // $HOME, "" = unknown
	dirs      []string // every directory a relative path may resolve against
	dynamic   string   // the first cd target the guard cannot resolve ("" = none)
	protected []string // protected paths, as configured and with symlinks resolved
	// makesLinks: a command of the call may create a symlink (ln, link, cp -s, a word naming
	// symlink), so a `..` after a path that does not exist yet may go through one.
	makesLinks bool
	deadline   time.Time // end of the time budget, zero = none
	expired    bool      // the budget ran out: every further check reports a hit
}

// newPathScope walks every command of the call (substitutions and `sh -c` code included) and
// collects the directories literal cd/pushd targets lead to.
func newPathScope(scripts []*script, cfg Config) *pathScope {
	s := &pathScope{dirs: []string{filepath.Clean(cfg.Dir)}, deadline: time.Now().Add(protectedBudget)}
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
				s.step(c, cdpathEnv)
			}
		}
	}
	return s
}

// over reports whether the time budget of the check has run out.
func (s *pathScope) over() bool {
	if !s.expired && !s.deadline.IsZero() && time.Now().After(s.deadline) {
		s.expired = true
	}
	return s.expired
}

// step adds the directories one command leads to (a literal cd/pushd target; a CDPATH assignment
// makes the directory unknown) and notes whether it may create a symlink.
func (s *pathScope) step(c *simpleCmd, cdpathEnv bool) {
	for _, w := range append(slices.Clone(c.words), c.heredoc) {
		if strings.Contains(w, "CDPATH=") {
			s.unknown("CDPATH=")
		}
		if strings.Contains(strings.ToLower(w), "symlink") {
			s.makesLinks = true
		}
	}
	argv := argvOf(c.words)
	if len(argv) == 0 {
		return
	}
	switch progName(argv[0]) {
	case "cd", "pushd":
		s.cd(argv, c.dyn[len(c.words)-len(argv):], cdpathEnv)
	case "ln", "link":
		s.makesLinks = true
	case "cp":
		s.makesLinks = s.makesLinks || slices.ContainsFunc(argv[1:], func(a string) bool {
			return a == "--symbolic-link" || (strings.HasPrefix(a, "-") && !strings.HasPrefix(a, "--") && strings.ContainsRune(a, 's'))
		})
	}
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
		// `cd` (logical) drops `x/..` lexically, `cd -P` lets the kernel resolve it after a symlink:
		// both directories are possible. A `..` after a component that does not exist yet (it may be a
		// symlink the call creates) leads somewhere the guard does not know.
		targets := []string{resolvePath(p, d)}
		if hasDotDot(p) {
			raw := p
			if !filepath.IsAbs(raw) {
				raw = d + "/" + p
			}
			k, ok := kernelPath(raw)
			if !ok {
				s.unknown(t)
				return
			}
			targets = append(targets, k)
		}
		for _, n := range targets {
			if !slices.Contains(s.dirs, n) {
				s.dirs = append(s.dirs, n)
			}
		}
	}
}

// kernelPath resolves an absolute path as written (not cleaned) the way the kernel does: component
// by component, each existing component replaced by its real path before a following `..` applies
// (`link/..` is the parent of the link's target). ok is false when a component before a `..` does
// not exist: at run time it may be a symlink the call creates, so where the path leads is unknown.
func kernelPath(raw string) (string, bool) {
	comps := strings.Split(filepath.ToSlash(raw), "/")
	cur := "/"
	for i, c := range comps {
		switch c {
		case "", ".":
			continue
		case "..":
			cur = filepath.Dir(cur)
			continue
		}
		next := filepath.Join(cur, c)
		real, err := filepath.EvalSymlinks(next)
		if err != nil {
			if slices.Contains(comps[i+1:], "..") {
				return "", false
			}
			return filepath.Join(append([]string{next}, comps[i+1:]...)...), true
		}
		cur = real
	}
	return cur, true
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

// bigText reports a word longer than any path the kernel opens that holds several path tokens. Only
// code (inline code by inlineCode, here-docs, here-strings) is skipped as one path for it: any
// other operand, option value or redirect target is always resolved whole (sqlite3 normalises a long name in
// userspace and opens it), and a heavy word is bounded by protectedBudget.
func bigText(w string) bool {
	return len(w) > maxWordBytes && len(pathTokens(w)) > 1
}

func (s *pathScope) hitPath(w string, tree bool) (string, bool) {
	if s.over() {
		return "", true
	}
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
	if s.makesLinks && s.linkDotDot(exp) {
		return w + " (`..` after a path a symlink made in this call may be)", true
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
// (symlinks followed), while filepath.Clean drops `link/..` lexically. The prefix is kept resolved,
// so each step looks at the new segment only; past a prefix that does not exist the walk is lexical.
func physical(abs string) string {
	cur, exists := "/", true
	for _, seg := range strings.Split(filepath.ToSlash(abs), "/") {
		switch seg {
		case "", ".":
		case "..":
			cur = filepath.Dir(cur)
			if !exists {
				// back at a directory that may exist: its symlinks count again
				_, err := os.Lstat(cur)
				exists = err == nil
			}
		default:
			next := filepath.Join(cur, seg)
			if exists {
				fi, err := os.Lstat(next)
				switch {
				case err != nil:
					exists = false
				case fi.Mode()&os.ModeSymlink != 0:
					next = resolvePath(next, "/")
				}
			}
			cur = next
		}
	}
	return cur
}

// linkDotDot reports a home-expanded path with a `..` whose prefix (the part before the first `..`)
// does not exist from some known directory: a symlink made earlier in the same call may be there, so
// where the `..` leads is unknown (`ln -s ../.. A && ln -s A/.. B`).
func (s *pathScope) linkDotDot(exp string) bool {
	segs := strings.Split(filepath.ToSlash(exp), "/")
	k := slices.Index(segs, "..")
	if k < 0 {
		return false
	}
	prefix := strings.Join(segs[:k], "/")
	if strings.Trim(prefix, "/.") == "" {
		return false
	}
	if filepath.IsAbs(prefix) {
		_, err := os.Lstat(prefix)
		return err != nil
	}
	for _, d := range s.dirs {
		if _, err := os.Lstat(d + "/" + prefix); err != nil {
			return true
		}
	}
	return false
}

// match checks absolute candidate paths against the protected paths (s.protected already holds
// their symlink-resolved forms).
func (s *pathScope) match(cands []string, tree bool) (string, bool) {
	for _, p := range cands {
		if slices.ContainsFunc(s.protected, func(q string) bool { return within(p, q) }) {
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
			if fold(filepath.Join(p, filepath.Base(q))) == fold(q) {
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
	segs := strings.Split(filepath.ToSlash(fold(rel)), "/")
	for _, q := range s.protected {
		qs := strings.Split(strings.Trim(filepath.ToSlash(fold(q)), "/"), "/")
		for k := range qs {
			n := min(len(segs), len(qs)-k)
			if slices.Equal(segs[:n], qs[k:k+n]) && (len(segs) >= len(qs)-k || tree) {
				return true
			}
		}
	}
	return false
}

// textHit is a protected path named inside the text of a word; anc is the token when it names an
// ancestor of q rather than q or a path inside it.
type textHit struct{ q, anc string }

func (h textHit) reason() string {
	if h.anc != "" {
		return ancestorBlocked(h.anc, h.q)
	}
	return protectedBlocked(h.q)
}

// reFileAuthority is a `file:` URI with an empty or `localhost` authority: SQLite opens the path
// after it.
var reFileAuthority = regexp.MustCompile(`(?i)file://(localhost)?/`)

// rePercent is one valid percent-encoded byte.
var rePercent = regexp.MustCompile(`%[0-9A-Fa-f]{2}`)

// contains returns the protected path that a word of a non-reader names inside its text: an option
// value (`dd of=…`, `--file=…`, `file:…`) or inline code (`python3 -c`, `perl -e`, `node -e`). It
// matches the protected path as a substring, absolute (homes expanded) or relative to any known
// directory (`../../../jobs.db`), and resolves every path-like token of the word like an argument
// (`//`, `/./`, `..` normalized; a token that cleans to `/` or `.` is skipped). With code set
// (inline code, a here-doc or here-string, an `--opt=` / `key=` value) a token is also refused when
// it is an ancestor of a protected path spelled with `//`, `/./`, `..`, or lying strictly below
// $HOME (`shutil.rmtree('<state>')`). Text with a `file:` URI is also checked with an empty or
// `localhost` authority dropped and percent-decoded.
func (s *pathScope) contains(w string, code bool) (textHit, bool) {
	w = s.expandAllHomes(w)
	texts := []string{w}
	if strings.Contains(strings.ToLower(w), "file:") {
		u := reFileAuthority.ReplaceAllString(w, "file:/")
		texts = append(texts, u)
		if d := percentDecode(u); d != u {
			texts = append(texts, d)
		}
	}
	for _, t := range texts {
		if h, ok := s.containsText(t, code); ok {
			return h, true
		}
	}
	return textHit{}, false
}

// percentDecode decodes every valid %XX sequence and keeps the rest (`'%d' % 1`, `100%`) as written.
func percentDecode(w string) string {
	return rePercent.ReplaceAllStringFunc(w, func(m string) string {
		b, err := strconv.ParseUint(m[1:], 16, 8)
		if err != nil {
			return m
		}
		return string([]byte{byte(b)})
	})
}

func (s *pathScope) containsText(w string, code bool) (textHit, bool) {
	fw := fold(w)
	for _, q := range s.protected {
		if strings.Contains(fw, fold(q)) {
			return textHit{q: q}, true
		}
		for _, d := range s.dirs {
			if rel, err := filepath.Rel(d, q); err == nil && !onlyDotDot(rel) && strings.Contains(fw, fold(rel)) {
				return textHit{q: q}, true
			}
		}
	}
	seen := map[string]bool{}
	for _, t := range pathTokens(w) {
		if seen[t] {
			continue
		}
		seen[t] = true
		if c := filepath.Clean(t); c == "/" || c == "." {
			continue // `7 // 2`, a `//` comment, `s,//,/,`: not a path a write could reach state by
		}
		if code { // a tree hit is a superset of a plain one: most tokens are settled by one resolution
			q, ok := s.hitPath(t, true)
			if !ok {
				continue
			}
			if p, in := s.hitPath(t, false); in {
				return textHit{q: p}, true
			}
			odd := strings.Contains(t, "//") || strings.Contains(t, "/./") || strings.HasPrefix(t, "./") || hasDotDot(t)
			if odd || s.belowHome(t) {
				return textHit{q: q, anc: t}, true
			}
			continue
		}
		if q, ok := s.hitPath(t, false); ok {
			return textHit{q: q}, true
		}
	}
	return textHit{}, false
}

// belowHome reports a token naming, from some known directory, a path strictly inside $HOME.
func (s *pathScope) belowHome(t string) bool {
	exp, ok := s.expandHome(t)
	if !ok || s.home == "" {
		return false
	}
	return slices.ContainsFunc(s.candidates(exp), func(p string) bool { return p != s.home && within(p, s.home) })
}

// reKeyValue is an `--opt=value`, `-o=value` or `key=value` word (no white space: not a message).
var reKeyValue = regexp.MustCompile(`^(--?[A-Za-z0-9][\w-]*|[A-Za-z_][\w.]*)=\S*$`)

// codeArg reports an argument whose text is code or an option value rather than free text (a commit
// message): an `--opt=` / `key=` word, or the value of a short option ending in c, e or E
// (`python3 -c`, `perl -e`, `node -pe`) or of --eval, --command, --execute, -cmd.
func codeArg(argv []string, k int) bool {
	if reKeyValue.MatchString(argv[k]) {
		return true
	}
	if k < 2 {
		return false
	}
	switch p := argv[k-1]; {
	case p == "--eval" || p == "--command" || p == "--execute" || p == "-cmd" || p == "--cmd":
		return true
	case strings.HasPrefix(p, "-") && !strings.HasPrefix(p, "--") && len(p) > 1:
		return strings.IndexByte("ceE", p[len(p)-1]) >= 0
	}
	return false
}

// shellFlags are shell options that take no value (not -o/-O), and `c`, which makes the first
// operand the code: `bash -ec <code>`, `-ce`, `-xc`, `-e -c`.
const shellFlags = "abcefhklmnprstuvxBCEHPT"

// awks take their program as the first operand unless given -f.
var awks = map[string]bool{"awk": true, "gawk": true, "mawk": true, "nawk": true, "busybox-awk": true}

// inlineCode reports argv[k] as inline code by the option semantics of the command named
// (`python3 -c`, `perl -e`, `node --eval`, `php -r`, `bash -c`, the awk program): only such a word
// over maxWordBytes is resolved by its tokens; any other word, an option value or key=value word
// too, is resolved whole (fail closed).
func inlineCode(name string, argv []string, k int) bool {
	switch {
	case shells[name]:
		// argv[k] is the first operand after flag clusters only, one of them holding `c`.
		c := false
		for _, a := range argv[1:k] {
			if len(a) < 2 || a[0] != '-' || strings.Trim(a[1:], shellFlags) != "" {
				return false
			}
			c = c || strings.Contains(a[1:], "c")
		}
		return c
	case awks[name]:
		for i := 1; i < len(argv); i++ {
			switch a := argv[i]; {
			case a == "--":
				return i+1 == k
			case a == "-F" || a == "-v":
				i++
			case strings.HasPrefix(a, "-F") || strings.HasPrefix(a, "-v"):
			case strings.HasPrefix(a, "-") && a != "-":
				return false
			default:
				return i == k
			}
		}
		return false
	}
	code := interpCode(name, argv[1:])
	return code >= 0 && code+1 == k
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
// `mv <state> x`, `ln -s <state> S; rm S/…`, `tar -C <state> -x`, `unzip a.zip -d <home>`,
// `unrar x a.rar <home>`). Looked up by progName, so the other names of a tool (toolAliases) count
// too.
var treeWriters = map[string]bool{
	"rm": true, "rmdir": true, "unlink": true, "mv": true, "cp": true, "install": true, "ln": true,
	"rsync": true, "chmod": true, "chown": true, "chgrp": true, "tar": true, "shred": true,
	"unzip": true, "ditto": true, "cpio": true, "pax": true, "7z": true, "p7zip": true, "unar": true,
	"unrar": true, "rar": true, "cabextract": true, "aunpack": true, "atool": true, "dtrx": true,
	"xar": true, "jar": true, "lha": true, "lhasa": true, "arj": true, "unarj": true,
}

// toolAliases map the other names of a tool to the one its rules are kept under: GNU and BSD
// builds and the p7zip and 7-Zip front ends take the same options and write the same way.
var toolAliases = map[string]string{
	"gtar": "tar", "gnutar": "tar", "bsdtar": "tar", "bsdcpio": "cpio", "bsdunzip": "unzip",
	"7za": "7z", "7zz": "7z", "7zzs": "7z", "7zr": "7z", "7zg": "7z",
}

// clusterDirOpts are the archive tools whose short options cluster like getopt: the directory
// option (`dir`) takes the rest of the word as its value (`tar -xvC<dir>`, `unzip -qd<dir>`,
// `cpio -idmD<dir>`), and so does any option in `vals`, which ends the cluster with a value of its
// own (`tar -xfC…` names the archive `C…`). Only the options that take a value in every variant
// (GNU and BSD tar, GNU and BSD cpio) are in `vals`: an unknown letter counts as a flag, so the scan
// goes on to the directory option (fail closed). A directory option ending the word takes the next
// word, which is checked whole as an ancestor already. unzip has no value option in `vals`: its `-P`
// takes the rest of the word only for the first password, a repeated `-P` is skipped and the scan
// goes on (`-Pdummy -Pd<dir>`), so any `d` in the word counts; a `-` inside a cluster negates the
// next option (`--qd<dir>`), so unzip clusters starting with `--` are scanned too.
var clusterDirOpts = map[string]struct {
	dir  byte
	vals string
}{
	"tar": {'C', "bITXf"}, "unzip": {'d', ""}, "cpio": {'D', "CEFHIMOR"}, "cabextract": {'d', "eF"},
	"xar": {'C', "fnsb"},
}

// dirSwitches are the directory switches that other extractors take with the directory in the same
// word, one switch per word, compared case-insensitively (`7z x -o<dir>`, `unrar x -op<dir>`,
// `arj x -ht<dir>`): a word starting with one names the directory after it.
var dirSwitches = map[string][]string{
	"7z": {"-o"}, "unrar": {"-op"}, "rar": {"-op"}, "unar": {"-o"}, "aunpack": {"-X"}, "atool": {"-X"},
	"arj": {"-ht"},
}

// attachedDir returns the directory attached to an archive tool's directory option in the same
// word (`-C<dir>`, `-xvC<dir>`, `-od<dir>`, `-O<dir>`, `-op<dir>`).
func attachedDir(name, a string) (string, bool) {
	if len(a) < 3 || a[0] != '-' || (a[1] == '-' && name != "unzip") {
		return "", false
	}
	if sw, ok := dirSwitches[name]; ok {
		for _, p := range sw {
			if len(a) > len(p) && strings.EqualFold(a[:len(p)], p) {
				return a[len(p):], true
			}
		}
		return "", false
	}
	opt, ok := clusterDirOpts[name]
	if !ok {
		return "", false
	}
	for j := 1; j < len(a)-1; j++ {
		switch c := a[j]; {
		case c == opt.dir:
			return a[j+1:], true
		case strings.IndexByte(opt.vals, c) >= 0:
			return "", false
		}
	}
	return "", false
}

// findActs reports a find with an action that writes or runs something (-delete, -exec, -fprint, …).
func findActs(args []string) bool {
	return slices.ContainsFunc(args, func(a string) bool {
		return findExec[a] || a == "-delete" || strings.HasPrefix(a, "-fprint") || a == "-fls"
	})
}

// destCopiers take sources and a destination: the ancestor rule applies to the sources only.
var destCopiers = map[string]bool{"cp": true, "install": true, "ln": true, "mv": true}

// copyArgs is the split of cp/install/ln/mv arguments into sources and destinations.
type copyArgs struct {
	dests   []string     // destination paths: the -t directory, else the last operand (`.` for `ln src`)
	destIdx map[int]bool // indexes of the arguments that hold a destination
	srcs    []string     // source operands
	whole   bool         // -T: the destination itself is replaced, so it is checked as an ancestor too
	parents bool         // --parents: a source lands at <dest>/<source path>, not <dest>/<name>
}

// copyLongVals are the long options of cp/install/ln/mv whose value may be the next word.
var copyLongVals = map[string]bool{
	"--suffix": true, "--mode": true, "--owner": true, "--group": true, "--no-preserve": true,
	"--sparse": true, "--strip-program": true,
}

// parseCopyArgs splits cp/install/ln/mv arguments like getopt: short clusters, `-t DIR`, `-tDIR`,
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
			case len(key) > 3 && strings.HasPrefix("--parents", key):
				ca.parents = true
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

// checkDest refuses a cp/install/ln/mv destination that is or holds a protected path, or whose
// <dest>/<source name> reaches one (`cp -r thrall ~/.agent-jobs`). A destination that is an ancestor
// of one is refused unless it is $HOME itself, written as such, without -T or --parents, and every
// source has a plain name: `cp f ~` writes ~/f, while `cp -r m/. ~` copies the contents of m.
func (s *pathScope) checkDest(d string, ca copyArgs) string {
	if p, ok := s.hitPath(d, false); ok {
		return protectedBlocked(p)
	}
	if p, ok := s.parentHit(d); ok {
		return protectedBlocked(p)
	}
	if p, ok := s.hitPath(d, true); ok && !s.plainIntoHome(d, ca) {
		return ancestorBlocked(d, p)
	}
	for _, src := range ca.srcs {
		if p, ok := s.hitPath(d+"/"+filepath.Base(filepath.Clean(src)), true); ok {
			return protectedBlocked(p)
		}
	}
	return ""
}

// plainIntoHome reports `cp|install|ln|mv [-t ~] SRC… [~]` that writes only $HOME/<name>: the
// destination is $HOME as written (`~`, `~/`, `$HOME`, the absolute path, no `..`), no -T or
// --parents, and every source is a bare name (no `/`, not `.` or `..`, no expansion or glob).
func (s *pathScope) plainIntoHome(d string, ca copyArgs) bool {
	exp, ok := s.expandHome(d)
	if !ok || s.home == "" || ca.whole || ca.parents || hasDotDot(exp) || filepath.Clean(exp) != s.home {
		return false
	}
	for _, src := range ca.srcs {
		t := strings.TrimRight(src, "/")
		name := filepath.Base(t)
		if t == "" || name == "." || name == ".." || strings.Contains(t, "/") ||
			strings.ContainsAny(src, "$`") || hasGlob(src) {
			return false
		}
	}
	return true
}
