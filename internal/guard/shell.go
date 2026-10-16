package guard

import (
	"path/filepath"
	"regexp"
	"strings"
)

// A small POSIX-ish shell reader: enough to see which programs a Bash tool call runs and
// with which arguments (quotes removed), without executing anything. It is deliberately
// conservative: command/process substitutions, `sh -c` strings and `eval` arguments are
// parsed as scripts of their own, so a command cannot hide inside them.

// simpleCmd is one simple command: its words with quotes removed.
type simpleCmd struct {
	words   []string
	dyn     []bool // dyn[i]: words[i] contains an expansion ($, `, substitution) the guard cannot resolve
	raw     string // source text with single-quoted content removed (for `$?` detection)
	heredoc string // bodies of the here-documents attached to this command
	stdin   bool   // input redirected: `<file`, `<&fd`, here-doc or here-string
}

// pipeline is one or more commands joined by `|` / `|&`.
type pipeline struct {
	cmds   []*simpleCmd
	nest   int    // subshell `( … )` depth
	before string // separator before it: "" (start), ";", "\n", "&&", "||", "&", "(", ")"
	after  string // separator after it
}

func (p *pipeline) raw() string {
	var parts []string
	for _, c := range p.cmds {
		parts = append(parts, c.raw)
	}
	return strings.Join(parts, " | ")
}

// script is a list of pipelines in execution order (separated by ; && || & newline).
type script struct {
	src       string
	depth     int // 0 = the command itself; >0 = inside a substitution, `sh -c`, `eval`
	pipelines []*pipeline
}

type redirKind int

const (
	redirNone redirKind = iota
	redirFile
	redirHeredoc
	redirHeredocStrip
)

type pendingHeredoc struct {
	delim  string
	strip  bool
	quoted bool // quoted delimiter: the body is literal; otherwise substitutions in it run
	cmd    *simpleCmd
}

type parser struct {
	src  string
	i    int
	subs []string // command/process substitutions, parsed later as scripts

	out      *script
	pipe     *pipeline
	cmd      *simpleCmd
	word     strings.Builder
	started  bool
	dyn      bool // current word contains an expansion
	quoted   bool // current word contains quoting
	redir    redirKind
	heredocs []pendingHeredoc
	paren    int    // current `( … )` depth
	sep      string // separator that ended the previous pipeline
}

// parseScript reads src into a script plus the sources of its substitutions.
func parseScript(src string) (*script, []string) {
	p := &parser{src: src, out: &script{src: src}}
	p.pipe = &pipeline{}
	p.cmd = &simpleCmd{}
	p.run()
	p.endPipeline("")
	return p.out, p.subs
}

func (p *parser) endWord() {
	if !p.started {
		return
	}
	w := p.word.String()
	p.word.Reset()
	switch p.redir {
	case redirNone:
		p.cmd.words = append(p.cmd.words, w)
		p.cmd.dyn = append(p.cmd.dyn, p.dyn)
	case redirHeredoc, redirHeredocStrip:
		p.heredocs = append(p.heredocs, pendingHeredoc{delim: w, strip: p.redir == redirHeredocStrip, quoted: p.quoted, cmd: p.cmd})
	}
	p.started, p.dyn, p.quoted = false, false, false
	p.redir = redirNone
}

func (p *parser) endCmd() {
	p.endWord()
	if len(p.cmd.words) > 0 || p.cmd.heredoc != "" || len(p.heredocs) > 0 {
		p.pipe.cmds = append(p.pipe.cmds, p.cmd)
	}
	p.cmd = &simpleCmd{}
}

func (p *parser) endPipeline(sep string) {
	p.endCmd()
	if len(p.pipe.cmds) > 0 {
		p.pipe.before, p.pipe.after, p.pipe.nest = p.sep, sep, p.paren
		p.out.pipelines = append(p.out.pipelines, p.pipe)
		p.sep = sep
	} else if sep != "\n" || p.sep == "" {
		// `a &&\n b`: an empty line does not make b unconditional.
		p.sep = sep
	}
	p.pipe = &pipeline{}
}

func (p *parser) peek(off int) byte {
	if p.i+off < len(p.src) {
		return p.src[p.i+off]
	}
	return 0
}

// matchParen returns the index just past the `)` that closes the `(` at open.
func matchParen(s string, open int) int {
	depth := 0
	for i := open; i < len(s); i++ {
		switch s[i] {
		case '\\':
			i++
		case '\'':
			if j := strings.IndexByte(s[i+1:], '\''); j >= 0 {
				i += j + 1
			}
		case '(':
			depth++
		case ')':
			depth--
			if depth == 0 {
				return i + 1
			}
		}
	}
	return len(s)
}

// substitution consumes `$(...)`, `<(...)`, `>(...)` or a backtick block starting at p.i,
// records its inner source and returns the literal text.
func (p *parser) substitution() string {
	start := p.i
	if p.src[p.i] == '`' {
		end := p.i + 1
		for end < len(p.src) && p.src[end] != '`' {
			if p.src[end] == '\\' {
				end++
			}
			end++
		}
		inner := p.src[start+1 : min(end, len(p.src))]
		p.subs = append(p.subs, inner)
		p.i = min(end+1, len(p.src))
		return p.src[start:p.i]
	}
	end := matchParen(p.src, p.i+1)
	innerEnd := end - 1
	if innerEnd < p.i+2 {
		innerEnd = p.i + 2
	}
	p.subs = append(p.subs, p.src[p.i+2:min(innerEnd, len(p.src))])
	p.i = end
	return p.src[start:end]
}

func (p *parser) readHeredocs() {
	for _, h := range p.heredocs {
		var body strings.Builder
		for p.i < len(p.src) {
			nl := strings.IndexByte(p.src[p.i:], '\n')
			line := p.src[p.i:]
			if nl >= 0 {
				line = p.src[p.i : p.i+nl]
				p.i += nl + 1
			} else {
				p.i = len(p.src)
			}
			cmp := line
			if h.strip {
				cmp = strings.TrimLeft(line, "\t")
			}
			if cmp == h.delim {
				break
			}
			body.WriteString(line)
			body.WriteByte('\n')
		}
		h.cmd.heredoc += body.String()
		h.cmd.stdin = true
		if !h.quoted { // `$(…)` and backticks in an unquoted here-doc body are executed
			p.subs = append(p.subs, bodySubstitutions(body.String())...)
		}
	}
	p.heredocs = nil
}

// bodySubstitutions returns the command substitutions of an unquoted here-doc body.
func bodySubstitutions(body string) []string {
	q := &parser{src: body}
	for q.i < len(body) {
		switch {
		case body[q.i] == '\\':
			q.i += 2
		case body[q.i] == '$' && q.peek(1) == '(', body[q.i] == '`':
			q.substitution()
		default:
			q.i++
		}
	}
	return q.subs
}

func isDigits(s string) bool {
	if s == "" {
		return false
	}
	for _, c := range s {
		if c < '0' || c > '9' {
			return false
		}
	}
	return true
}

func (p *parser) run() {
	for p.i < len(p.src) {
		c := p.src[p.i]
		switch {
		case c == ' ' || c == '\t':
			p.endWord()
			p.cmd.raw += " "
			p.i++
		case c == '\\':
			if p.peek(1) == '\n' {
				p.i += 2
				continue
			}
			if p.i+1 < len(p.src) {
				p.word.WriteByte(p.src[p.i+1])
				p.cmd.raw += p.src[p.i : p.i+2]
			}
			p.started, p.quoted = true, true
			p.i += 2
		case c == '\'':
			end := strings.IndexByte(p.src[p.i+1:], '\'')
			if end < 0 {
				end = len(p.src) - p.i - 1
			}
			p.word.WriteString(p.src[p.i+1 : p.i+1+end])
			p.cmd.raw += "''"
			p.started, p.quoted = true, true
			p.i += end + 2
		case c == '"':
			p.double()
		case c == '$' && p.peek(1) == '(', c == '`':
			lit := p.substitution()
			p.word.WriteString(lit)
			p.cmd.raw += lit
			p.started, p.dyn = true, true
		case c == '#' && !p.started:
			nl := strings.IndexByte(p.src[p.i:], '\n')
			if nl < 0 {
				p.i = len(p.src)
			} else {
				p.i += nl
			}
		case c == '\n':
			p.endPipeline("\n")
			p.i++
			p.readHeredocs()
		case c == ';':
			p.endPipeline(";")
			p.i++
		case c == '&' && p.peek(1) == '&':
			p.endPipeline("&&")
			p.i += 2
		case c == '&' && p.peek(1) == '>':
			p.endWord()
			p.i += 2
			if p.peek(0) == '>' {
				p.i++
			}
			p.redir = redirFile
		case c == '&':
			p.endPipeline("&")
			p.i++
		case c == '|' && p.peek(1) == '|':
			p.endPipeline("||")
			p.i += 2
		case c == '|':
			p.endCmd()
			p.i++
			if p.peek(0) == '&' {
				p.i++
			}
		case c == '(':
			p.endPipeline("(")
			p.paren++
			p.i++
		case c == ')':
			p.endPipeline(")")
			p.paren--
			p.i++
		case c == '<' || c == '>':
			p.redirect()
		default:
			if c == '$' {
				p.dyn = true
			}
			p.word.WriteByte(c)
			p.cmd.raw += string(c)
			p.started = true
			p.i++
		}
	}
}

func (p *parser) redirect() {
	if p.peek(1) == '(' { // process substitution <(...) / >(...)
		lit := p.substitution()
		p.word.WriteString(lit)
		p.started, p.dyn = true, true
		return
	}
	if p.started && isDigits(p.word.String()) { // fd number: 2>&1
		p.word.Reset()
		p.started = false
	}
	p.endWord()
	op := p.src[p.i]
	p.i++
	if op == '<' {
		p.cmd.stdin = true
	}
	kind := redirFile
	if op == '<' && p.peek(0) == '<' {
		p.i++
		switch {
		case p.peek(0) == '<': // here-string <<<
			p.i++
		case p.peek(0) == '-':
			p.i++
			kind = redirHeredocStrip
		default:
			kind = redirHeredoc
		}
	} else if p.peek(0) == '>' || p.peek(0) == '&' || p.peek(0) == '|' {
		p.i++
	}
	p.redir = kind
}

func (p *parser) double() {
	p.i++ // opening quote
	p.started, p.quoted = true, true
	for p.i < len(p.src) {
		c := p.src[p.i]
		switch {
		case c == '"':
			p.i++
			return
		case c == '\\' && p.i+1 < len(p.src) && strings.IndexByte("$`\"\\\n", p.src[p.i+1]) >= 0:
			if p.src[p.i+1] != '\n' {
				p.word.WriteByte(p.src[p.i+1])
			}
			p.cmd.raw += p.src[p.i : p.i+2]
			p.i += 2
		case c == '$' && p.peek(1) == '(', c == '`':
			lit := p.substitution()
			p.word.WriteString(lit)
			p.cmd.raw += lit
			p.dyn = true
		default:
			if c == '$' {
				p.dyn = true
			}
			p.word.WriteByte(c)
			p.cmd.raw += string(c)
			p.i++
		}
	}
}

const maxNesting = 8

// shells are matched on the lower-cased base name (macOS file systems are case-insensitive:
// `BASH -c` runs bash). `busybox sh` is seen through the busybox wrapper (see wrapperValueFlags).
var shells = map[string]bool{
	"sh": true, "bash": true, "rbash": true, "zsh": true, "dash": true, "ash": true, "ksh": true,
	"mksh": true, "pdksh": true, "oksh": true, "yash": true, "posh": true, "csh": true, "tcsh": true, "fish": true,
}

// shellCode returns the command string a shell runs from -c / --command / --command=.
func shellCode(argv []string) (string, bool) {
	for i := 1; i < len(argv); i++ {
		a := argv[i]
		switch {
		case strings.HasPrefix(a, "--command="):
			return strings.TrimPrefix(a, "--command="), true
		case a == "--command" || (strings.HasPrefix(a, "-") && !strings.HasPrefix(a, "--") && strings.Contains(a, "c")):
			if i+1 < len(argv) {
				return argv[i+1], true
			}
			return "", false
		}
	}
	return "", false
}

// runnerCode returns the command strings of runners that take a shell command as an option value
// (`nix-shell --run 'git push'`, `nix-shell --command …`), so they are parsed like `sh -c`.
func runnerCode(name string, argv []string) []string {
	if name != "nix-shell" && name != "nix" {
		return nil
	}
	var out []string
	for i := 1; i < len(argv); i++ {
		a := argv[i]
		switch {
		case (a == "--run" || a == "--command") && i+1 < len(argv):
			out = append(out, argv[i+1])
			i++
		case strings.HasPrefix(a, "--run="), strings.HasPrefix(a, "--command="):
			out = append(out, a[strings.IndexByte(a, '=')+1:])
		}
	}
	return out
}

// parseAll returns src and every script nested in it (substitutions, `sh -c`, `eval`).
func parseAll(src string) []*script {
	var out []*script
	var walk func(s string, depth int)
	walk = func(s string, depth int) {
		if depth > maxNesting {
			return
		}
		sc, subs := parseScript(s)
		sc.depth = depth
		out = append(out, sc)
		for _, sub := range subs {
			walk(sub, depth+1)
		}
		for _, pl := range sc.pipelines {
			for _, c := range pl.cmds {
				argv := argvOf(c.words)
				if len(argv) == 0 {
					continue
				}
				switch name := strings.ToLower(base(argv[0])); {
				case name == "eval":
					rest := argv[1:]
					if len(rest) > 0 && rest[0] == "--" {
						rest = rest[1:] // `eval -- 'git push'`
					}
					walk(strings.Join(rest, " "), depth+1)
				case shells[name]:
					if code, ok := shellCode(argv); ok {
						walk(code, depth+1)
					}
				default:
					for _, code := range runnerCode(name, argv) {
						walk(code, depth+1)
					}
				}
			}
		}
	}
	walk(src, 0)
	return out
}

func base(s string) string { return filepath.Base(s) }

var (
	reAssign = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*=`)
	keywords = map[string]bool{
		"if": true, "then": true, "else": true, "elif": true, "do": true, "while": true,
		"until": true, "!": true, "{": true, "}": true, "time": true, "fi": true, "done": true,
	}
)

// wrapperValueFlags lists, per wrapper program, the short flags that take a value.
var wrapperValueFlags = map[string]string{
	"env": "uCS", "command": "", "builtin": "", "exec": "a", "nohup": "", "nice": "n",
	"timeout": "sk", "xargs": "IinPLdEsa", "sudo": "ugCDhpRrTU", "doas": "uC", "stdbuf": "ioe",
	"busybox": "", // `busybox sh -c …`, `busybox git …`: the applet is the real program
}

// cmdView is a simple command with keywords, assignments and wrapper programs stripped.
type cmdView struct {
	start       int      // index of the real program in words
	wrappers    []string // wrapper programs stripped (env, xargs, sudo, …)
	splitString bool     // `env -S` / `--split-string`: the program comes from a string the guard cannot see
}

// split strips keywords, assignments and wrapper programs: what remains is the real program and its args.
func split(words []string) cmdView {
	var v cmdView
	i := 0
	for i < len(words) && (keywords[words[i]] || reAssign.MatchString(words[i])) {
		if words[i] == "time" { // `time -p git push`: the options of time do not hide the command
			for i+1 < len(words) && strings.HasPrefix(words[i+1], "-") {
				i++
				if words[i] == "--" {
					break
				}
			}
		}
		i++
	}
	for i < len(words) {
		name := strings.ToLower(base(words[i]))
		vf, ok := wrapperValueFlags[name]
		if !ok {
			break
		}
		v.wrappers = append(v.wrappers, name)
		i++
		for i < len(words) {
			w := words[i]
			if w == "--" {
				i++
				break
			}
			if reAssign.MatchString(w) && name == "env" {
				i++
				continue
			}
			if strings.HasPrefix(w, "-") && len(w) > 1 {
				if name == "env" && (strings.HasPrefix(w, "--split-string") ||
					(!strings.HasPrefix(w, "--") && strings.ContainsRune(w, 'S'))) {
					v.splitString = true
				}
				i++
				if !strings.HasPrefix(w, "--") && strings.ContainsRune(vf, rune(w[len(w)-1])) {
					i++ // value of the last flag in the cluster
				}
				continue
			}
			break
		}
		if name == "timeout" && i < len(words) {
			i++ // duration
		}
	}
	v.start = min(i, len(words))
	return v
}

// argvOf returns the real program and its args.
func argvOf(words []string) []string { return words[split(words).start:] }
