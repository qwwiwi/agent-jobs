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
	words []string
	dyn   []bool // dyn[i]: words[i] contains an expansion ($, `, substitution) the guard cannot resolve
	// bare[i]: words[i] has an expansion outside double quotes (the shell may split or glob it into
	// several words); a dyn word that is not bare stays one word.
	bare    []bool
	raw     string // source text with single-quoted content removed (for `$?` detection)
	heredoc string // bodies of the here-documents and words of the here-strings attached to this command
	stdin   bool   // input redirected: `<file`, `<&fd`, here-doc or here-string
	// outRedirs are the targets of output redirections (`>f`, `>>f`, `>|f`, `&>f`, `<>f`, `2>&1` gives "1").
	outRedirs []string
	outDyn    []bool // outDyn[i]: outRedirs[i] contains an expansion
	// inRedirs are the sources of input redirections and here-strings (`<f`, `<>f`, `<<<w`).
	inRedirs []string
	// feeds retain the commands of a compound pipeline stage; inputSources retain
	// process substitutions supplying an argument or redirected stdin.
	feeds        []*simpleCmd
	inputSources []string
	incoming     []*simpleCmd // pipeline stages supplying an enclosing compound command
	inputGroups  []*simpleCmd // enclosing stages carrying trailing input redirections
	subSources   []string     // substitutions executed while expanding this command
	subScripts   []*script
	nested       []*script // shell/eval/runner bodies executed by this command
}

// pipeline is one or more commands joined by `|` / `|&`.
type pipeline struct {
	cmds   []*simpleCmd
	piped  bool   // a pipe operator occurred, including before a continuation newline
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
	// comments and literalBodies are byte ranges [start, end) of src the shell neither runs nor expands:
	// its `#` comments and the bodies of here-documents with a quoted delimiter (<<'E', <<"E", <<\E).
	comments      [][2]int
	literalBodies [][2]int
}

type redirKind int

const (
	redirNone redirKind = iota
	redirFile
	redirHeredoc
	redirHeredocStrip
	redirHereString
)

type pendingHeredoc struct {
	delim  string
	strip  bool
	quoted bool // quoted delimiter: the body is literal; otherwise substitutions in it run
	// exact: the delimiter word holds no $ (in `<<$'EOF'` the reader's delimiter is not bash's, so the
	// body's end is not known and no literal-body range is recorded).
	exact bool
	cmd   *simpleCmd
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
	bare     bool // current word contains an expansion outside double quotes
	quoted   bool // current word contains quoting
	redir    redirKind
	redirOut bool // the pending redirFile target is written to
	redirIn  bool // the pending redirFile source is read (`<`, `<>`, `<<<`)
	heredocs []pendingHeredoc
	paren    int             // current `( … )` depth
	sep      string          // separator that ended the previous pipeline
	groups   []compoundGroup // open compound commands
}

type compoundGroup struct {
	start int
	outer *pipeline
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
		p.cmd.bare = append(p.cmd.bare, p.bare)
	case redirFile:
		if p.redirOut {
			p.cmd.outRedirs = append(p.cmd.outRedirs, w)
			p.cmd.outDyn = append(p.cmd.outDyn, p.dyn)
		}
		if p.redirIn {
			p.cmd.inRedirs = append(p.cmd.inRedirs, w)
		}
	case redirHereString: // its word is checked like a here-doc body
		p.cmd.heredoc += w + "\n"
	case redirHeredoc, redirHeredocStrip:
		p.heredocs = append(p.heredocs, pendingHeredoc{delim: w, strip: p.redir == redirHeredocStrip, quoted: p.quoted, exact: !p.dyn, cmd: p.cmd})
	}
	p.started, p.dyn, p.bare, p.quoted = false, false, false, false
	p.redir, p.redirOut, p.redirIn = redirNone, false, false
}

func (p *parser) endCmd() {
	p.endWord()
	// A bare redirection is a command too: `> f` truncates f, `< f` opens it.
	if len(p.cmd.words) > 0 || p.cmd.heredoc != "" || len(p.heredocs) > 0 || len(p.cmd.outRedirs) > 0 || len(p.cmd.inRedirs) > 0 || len(p.cmd.feeds) > 0 {
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
	var quotes []bool
	double := false
	for i := open; i < len(s); i++ {
		switch s[i] {
		case '\\':
			i++
		case '\'':
			if !double {
				if j := strings.IndexByte(s[i+1:], '\''); j >= 0 {
					i += j + 1
				}
			}
		case '"':
			double = !double
		case '`':
			i = stringEnd(s, i) - 1
		case '(':
			if !double || i > 0 && s[i-1] == '$' {
				quotes = append(quotes, double)
				double = false
			}
		case ')':
			if !double && len(quotes) > 0 {
				double = quotes[len(quotes)-1]
				quotes = quotes[:len(quotes)-1]
				if len(quotes) == 0 {
					return i + 1
				}
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
		p.cmd.subSources = append(p.cmd.subSources, inner)
		p.i = min(end+1, len(p.src))
		return p.src[start:p.i]
	}
	end := matchParen(p.src, p.i+1)
	innerEnd := end - 1
	if innerEnd < p.i+2 {
		innerEnd = p.i + 2
	}
	inner := p.src[p.i+2 : min(innerEnd, len(p.src))]
	p.subs = append(p.subs, inner)
	p.cmd.subSources = append(p.cmd.subSources, inner)
	if p.src[start] == '<' {
		p.cmd.inputSources = append(p.cmd.inputSources, inner)
	}
	p.i = end
	return p.src[start:end]
}

func (p *parser) readHeredocs() {
	for _, h := range p.heredocs {
		var body strings.Builder
		start := p.i
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
		switch {
		case h.quoted && h.exact:
			p.out.literalBodies = append(p.out.literalBodies, [2]int{start, p.i})
		case !h.quoted: // `$(…)` and backticks in an unquoted here-doc body are executed
			subs := bodySubstitutions(body.String())
			p.subs = append(p.subs, subs...)
			h.cmd.subSources = append(h.cmd.subSources, subs...)
		}
	}
	p.heredocs = nil
}

// bodySubstitutions returns the command substitutions of an unquoted here-doc body.
func bodySubstitutions(body string) []string {
	q := &parser{src: body, cmd: &simpleCmd{}}
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
			p.started, p.dyn, p.bare = true, true, true
		case c == '#' && !p.started:
			start := p.i
			nl := strings.IndexByte(p.src[p.i:], '\n')
			if nl < 0 {
				p.i = len(p.src)
			} else {
				p.i += nl
			}
			if exactComment(p.src[:start]) {
				p.out.comments = append(p.out.comments, [2]int{start, p.i})
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
			p.redir, p.redirOut = redirFile, true
		case c == '&':
			p.endPipeline("&")
			p.i++
		case c == '|' && p.peek(1) == '|':
			p.endPipeline("||")
			p.i += 2
		case c == '|':
			p.endCmd()
			p.pipe.piped = true
			p.i++
			if p.peek(0) == '&' {
				p.i++
			}
		case c == '(':
			p.openGroup("(")
			p.paren++
			p.i++
		case c == ')':
			p.closeGroup(")")
			p.paren--
			p.i++
		case (c == '{' || c == '}') && !p.started && (p.peek(1) == 0 || strings.ContainsRune(" \t\r\n;|&", rune(p.peek(1)))):
			if c == '{' {
				p.openGroup("{")
				p.paren++
			} else {
				p.closeGroup("}")
				p.paren--
			}
			p.i++
		case c == '<' || c == '>':
			p.redirect()
		default:
			if c == '$' {
				p.dyn, p.bare = true, true
			}
			p.word.WriteByte(c)
			p.cmd.raw += string(c)
			p.started = true
			p.i++
		}
	}
}

// exactComment reports a # the reader takes as a comment where bash surely does too: before is the
// source ahead of it. After ${, $'…' or (( the reader's word boundaries may not be bash's (`${x:- #}`,
// `$'a\' #'`, `(( y= # ))` hold no comment), so no comment range is recorded there.
func exactComment(before string) bool {
	return !strings.Contains(before, "${") && !strings.Contains(before, "$'") &&
		!strings.Contains(strings.ReplaceAll(before, "$((", ""), "((")
}

func (p *parser) openGroup(sep string) {
	p.endCmd()
	p.groups = append(p.groups, compoundGroup{len(p.out.pipelines), p.pipe})
	p.pipe = &pipeline{}
	p.sep = sep
}

func (p *parser) closeGroup(sep string) {
	p.endPipeline(sep)
	if len(p.groups) == 0 {
		return
	}
	group := p.groups[len(p.groups)-1]
	p.groups = p.groups[:len(p.groups)-1]
	stage := p.cmd
	for _, pl := range p.out.pipelines[group.start:] {
		for _, c := range pl.cmds {
			// Bodies are flattened once; synthetic stages must not duplicate nested bodies.
			if len(c.feeds) == 0 {
				stage.feeds = append(stage.feeds, c)
				c.incoming = append(c.incoming, group.outer.cmds...)
				c.inputGroups = append(c.inputGroups, stage)
			}
		}
	}
	p.pipe = group.outer
}

// commandInputWords keeps input relationships through compound consumers. Only
// redirections of enclosing stages are read, avoiding cycles through their bodies.
func commandInputWords(c *simpleCmd, previous []*simpleCmd, over func() bool) (words []string, fed bool) {
	for _, stages := range [][]*simpleCmd{previous, c.incoming} {
		for _, prev := range stages {
			if over() {
				return words, fed
			}
			words = append(words, feederWords(prev, over)...)
			fed = true
		}
	}
	for _, input := range append([]*simpleCmd{c}, c.inputGroups...) {
		if over() {
			return words, fed
		}
		fed = fed || input.stdin || len(input.inputSources) > 0
		words = append(words, input.heredoc)
		words = append(words, input.inRedirs...)
		for _, src := range input.inputSources {
			words = append(words, sourceFeederWords(src, over)...)
		}
	}
	return words, fed
}

// feederWords includes every command of a compound stage and input substitution,
// rather than just the final command before `|`.
func feederWords(c *simpleCmd, over func() bool) (words []string) {
	var walk func(*simpleCmd)
	walk = func(c *simpleCmd) {
		if over() {
			return
		}
		words = append(words, c.words...)
		words = append(words, c.heredoc)
		for _, src := range c.inputSources {
			words = append(words, sourceFeederWords(src, over)...)
		}
		for _, prev := range c.feeds {
			if over() {
				return
			}
			walk(prev)
		}
	}
	walk(c)
	return words
}

func sourceFeederWords(src string, over func() bool) (words []string) {
	if over() {
		return nil
	}
	// parseAll already includes nested substitutions and compound bodies. Do not
	// recursively expand their feeder metadata and duplicate those commands.
	for _, sc := range parseAll(src) {
		for _, pl := range sc.pipelines {
			for _, c := range pl.cmds {
				if over() {
					return words
				}
				words = append(words, c.words...)
				words = append(words, c.heredoc)
			}
		}
	}
	return words
}

func (p *parser) redirect() {
	if p.peek(1) == '(' { // process substitution <(...) / >(...)
		lit := p.substitution()
		p.word.WriteString(lit)
		p.started, p.dyn, p.bare = true, true, true
		return
	}
	if p.started && isDigits(p.word.String()) { // fd number: 2>&1
		p.word.Reset()
		p.started = false
	}
	p.endWord()
	op := p.src[p.i]
	p.i++
	out := op == '>'
	if op == '<' {
		p.cmd.stdin = true
	}
	kind := redirFile
	if op == '<' && p.peek(0) == '<' {
		p.i++
		switch {
		case p.peek(0) == '<': // here-string <<<
			p.i++
			kind = redirHereString
		case p.peek(0) == '-':
			p.i++
			kind = redirHeredocStrip
		default:
			kind = redirHeredoc
		}
	} else if p.peek(0) == '>' || p.peek(0) == '&' || p.peek(0) == '|' {
		out = out || p.peek(0) == '>' // `<>file` opens it for writing too
		p.i++
	}
	p.redir, p.redirOut, p.redirIn = kind, out, op == '<' && kind == redirFile
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
				if splitsQuoted(p.src[p.i+1:]) {
					p.bare = true
				}
			}
			p.word.WriteByte(c)
			p.cmd.raw += string(c)
			p.i++
		}
	}
}

// splitsQuoted reports an expansion that gives several words even inside double quotes ("$@",
// "${a[@]}", "${!p@}"); rest is the text after its $.
func splitsQuoted(rest string) bool {
	if strings.HasPrefix(rest, "@") {
		return true
	}
	if !strings.HasPrefix(rest, "{") {
		return false
	}
	if end := strings.IndexByte(rest, '}'); end >= 0 {
		rest = rest[:end]
	}
	return strings.Contains(rest, "@")
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
	if name == "script" || name == "watch" || name == "vim" || name == "vi" || name == "nvim" || name == "ex" {
		editor := name != "script" && name != "watch"
		vals, longs := runnerOptions(name)
		var out []string
		for i := 1; i < len(argv); {
			a := argv[i]
			if a == "--" {
				i++
				if name == "watch" {
					out = append(out, strings.Join(argv[i:], " "))
				}
				break
			}
			if editor && strings.HasPrefix(a, "+") {
				out = append(out, exCommands(a[1:])...)
				i++
				continue
			}
			if !strings.HasPrefix(a, "-") || a == "-" {
				if name == "watch" {
					out = append(out, strings.Join(argv[i:], " "))
					break
				}
				i++
				continue
			}
			opts, next := utilityOptions(argv, i, vals, longs)
			i = next
			for _, opt := range opts {
				if editor && (opt.name == "c" || opt.name == "--cmd" || opt.name == "-cmd") {
					out = append(out, exCommands(opt.value)...)
				}
				if name == "script" && (opt.name == "c" || opt.name == "--command") {
					out = append(out, opt.value)
				}
			}
		}
		return out
	}
	if name == "perl" || awks[name] {
		var out, programs []string
		if name == "perl" {
			programs = perlPrograms(argv[1:])
		} else {
			for k := 1; k < len(argv); k++ {
				if inlineCode(name, argv, k) {
					code := argv[k]
					if strings.HasPrefix(code, "--source=") {
						code = strings.TrimPrefix(code, "--source=")
					} else if strings.HasPrefix(code, "-e") && len(code) > 2 {
						code = code[2:]
					}
					programs = append(programs, code)
				}
			}
		}
		for _, code := range programs {
			out = append(out, systemCommands(code, name == "perl")...)
		}
		return out
	}
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

// Only command positions count: printing a word such as "screen" stays data.
var reExShell = regexp.MustCompile(`(?m)(^|[|:])\s*(silent!?\s+)?[0-9,$%]*!`)

func runnerOptions(name string) (string, map[string]bool) {
	switch name {
	case "script":
		return "cIOTBmo", map[string]bool{"--command": true, "--log-in": true, "--log-out": true, "--log-io": true, "--log-timing": true, "--logging-format": true, "--output-limit": true}
	case "watch":
		return "n", map[string]bool{"--interval": true}
	default:
		return "cuUwWT", map[string]bool{"--cmd": true, "-cmd": true}
	}
}

// runnerDataValues marks option values that are paths, timings or other data.
// Command-bearing values remain subject to the nested command checks.
func runnerDataValues(name string, args []string) map[int]bool {
	data := map[int]bool{}
	if name != "script" && name != "watch" {
		return data
	}
	vals, longs := runnerOptions(name)
	for i := 0; i < len(args); {
		a := args[i]
		if a == "--" {
			break
		}
		if !strings.HasPrefix(a, "-") || a == "-" {
			if name == "watch" {
				break
			}
			i++
			continue
		}
		opts, next := utilityOptions(args, i, vals, longs)
		command := false
		for _, opt := range opts {
			command = command || opt.name == "c" || opt.name == "--command"
		}
		if !command {
			for j := i; j < next; j++ {
				data[j] = true
			}
		}
		i = next
	}
	return data
}

type utilityOption struct{ name, value string }

// utilityOptions reads one option word. A value option ends a short cluster,
// consuming either its attached suffix or the next argument, never both.
func utilityOptions(argv []string, i int, shortValues string, longValues map[string]bool, optionalShort ...string) (out []utilityOption, next int) {
	a := argv[i]
	next = i + 1
	value := func(rest string) string {
		if rest == "" && next < len(argv) {
			rest = argv[next]
			next++
		}
		return rest
	}
	key, val, eq := strings.Cut(a, "=")
	if strings.HasPrefix(a, "--") || longValues[key] {
		if longValues[key] && !eq {
			val = value("")
		}
		return []utilityOption{{key, val}}, next
	}
	for j := 1; j < len(a); j++ {
		opt := utilityOption{name: string(a[j])}
		if strings.IndexByte(shortValues, a[j]) >= 0 {
			opt.value = a[j+1:]
			optional := len(optionalShort) > 0 && strings.IndexByte(optionalShort[0], a[j]) >= 0
			if !optional {
				opt.value = value(opt.value)
			}
			return append(out, opt), next
		}
		out = append(out, opt)
	}
	return out, next
}

// exCommands scans command syntax with strings masked; :execute explicitly
// evaluates its string argument, while a quoted value assigned by :let is data.
var reExExecute = regexp.MustCompile(`\b(?:execute|exe)\s*(?:\(\s*)?`)

func exCommands(code string) []string { return nestedExCommands(code, 0) }

func nestedExCommands(code string, depth int) (out []string) {
	masked := []byte(code)
	for i := 0; i < len(code); {
		if code[i] == '\'' || code[i] == '"' {
			end := stringEnd(code, i)
			for j := i; j < end; j++ {
				masked[j] = 0
			}
			i = end
		} else {
			i++
		}
	}
	for _, loc := range reExShell.FindAllStringIndex(string(masked), -1) {
		out = append(out, code[loc[1]:])
	}
	if depth >= maxNesting {
		return out
	}
	for _, loc := range reExExecute.FindAllStringIndex(string(masked), -1) {
		start := loc[1]
		if start < len(code) && (code[start] == '\'' || code[start] == '"') {
			end := stringEnd(code, start)
			if end > start+1 && code[end-1] == code[start] {
				out = append(out, nestedExCommands(decodeLiteral(code[start+1:end-1]), depth+1)...)
			}
		}
	}
	return out
}

func perlPrograms(args []string) (out []string) {
	spec := interpSpecs["perl"]
	for i := 0; i < len(args); i++ {
		a := args[i]
		if !strings.HasPrefix(a, "-") || a == "--" || a == "-" {
			break
		}
		for j := 1; j < len(a); j++ {
			c := a[j]
			if c == 'e' || c == 'E' {
				if j+1 < len(a) {
					out = append(out, a[j+1:])
				} else if i+1 < len(args) {
					i++
					out = append(out, args[i])
				}
				break
			}
			if strings.IndexByte(spec.valueShort, c) >= 0 {
				if j+1 == len(a) {
					i++
				}
				break
			}
			if strings.IndexByte(spec.attachShort, c) >= 0 {
				break
			}
		}
	}
	return out
}

// systemCommands extracts literal command arguments, skipping strings and
// comments so printing "system('screen')" is still ordinary data.
func systemCommands(code string, bare bool) (out []string) {
	for i := 0; i < len(code); {
		if code[i] == '#' {
			if end := strings.IndexByte(code[i:], '\n'); end >= 0 {
				i += end + 1
				continue
			}
			break
		}
		if code[i] == '\'' || code[i] == '"' {
			i = stringEnd(code, i)
			continue
		}
		if !bare && code[i] == '/' && awkRegexStart(code, i) {
			i = stringEnd(code, i)
			continue
		}
		if !identifierByte(code[i]) {
			i++
			continue
		}
		start := i
		for i < len(code) && identifierByte(code[i]) {
			i++
		}
		word := code[start:i]
		if bare && (word == "q" || word == "qq" || word == "qw" || word == "qx") {
			if lit, end, ok := perlQuote(code, start); ok {
				if word == "qx" {
					out = append(out, lit)
				}
				i = end
				continue
			}
		}
		if word != "system" && word != "exec" {
			continue
		}
		for i < len(code) && strings.ContainsRune(" \t\r\n", rune(code[i])) {
			i++
		}
		if i < len(code) && code[i] == '(' {
			i++
		} else if !bare {
			continue
		}
		for i < len(code) && strings.ContainsRune(" \t\r\n", rune(code[i])) {
			i++
		}
		if bare && i < len(code) && code[i] == '{' {
			i++
			for i < len(code) && code[i] == ' ' {
				i++
			}
		}
		if i < len(code) && (code[i] == '\'' || code[i] == '"') {
			end := stringEnd(code, i)
			if end > i+1 && code[end-1] == code[i] {
				out = append(out, decodeLiteral(code[i+1:end-1]))
			}
			i = end
		} else if bare {
			if lit, end, ok := perlQuote(code, i); ok {
				out = append(out, lit)
				i = end
			}
		}
	}
	return out
}

func awkRegexStart(code string, i int) bool {
	for i--; i >= 0; i-- {
		if strings.ContainsRune(" \t\r\n", rune(code[i])) {
			continue
		}
		return strings.ContainsRune("(~=!{;,", rune(code[i]))
	}
	return true
}

// perlQuote reads q/qq/qw/qx literals with paired, nested delimiters.
func perlQuote(code string, start int) (string, int, bool) {
	i := start
	if i >= len(code) || code[i] != 'q' {
		return "", start, false
	}
	i++
	if i < len(code) && strings.ContainsRune("qwx", rune(code[i])) {
		i++
	}
	for i < len(code) && (code[i] == ' ' || code[i] == '\t') {
		i++
	}
	if i >= len(code) || identifierByte(code[i]) {
		return "", start, false
	}
	open, close := code[i], code[i]
	switch open {
	case '(':
		close = ')'
	case '[':
		close = ']'
	case '{':
		close = '}'
	case '<':
		close = '>'
	}
	begin, depth := i+1, 1
	for i++; i < len(code); i++ {
		if code[i] == '\\' {
			i++
			continue
		}
		if open != close && code[i] == open {
			depth++
		}
		if code[i] == close {
			depth--
			if depth == 0 {
				return decodeLiteral(code[begin:i]), i + 1, true
			}
		}
	}
	return "", start, false
}

func identifierByte(c byte) bool {
	return c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '_'
}

// stringEnd returns the byte after a quoted literal, keeping escapes as written.
func stringEnd(s string, start int) int {
	for i := start + 1; i < len(s); i++ {
		if s[i] == '\\' {
			i++
		} else if s[i] == s[start] {
			return i + 1
		}
	}
	return len(s)
}

// parseAll returns src and every script nested in it (substitutions, `sh -c`, `eval`).
func parseAll(src string) []*script {
	var out []*script
	var walk func(s string, depth int) *script
	walk = func(s string, depth int) *script {
		if depth > maxNesting {
			return nil
		}
		sc, _ := parseScript(s)
		sc.depth = depth
		out = append(out, sc)
		for _, pl := range sc.pipelines {
			for _, c := range pl.cmds {
				for _, sub := range c.subSources {
					if child := walk(sub, depth+1); child != nil {
						c.subScripts = append(c.subScripts, child)
					}
				}
				add := func(code string) {
					if child := walk(code, depth+1); child != nil {
						c.nested = append(c.nested, child)
					}
				}
				argv := argvOf(c.words)
				if len(argv) == 0 {
					continue
				}
				switch name := progName(argv[0]); {
				case name == "eval":
					rest := argv[1:]
					if len(rest) > 0 && rest[0] == "--" {
						rest = rest[1:] // `eval -- 'git push'`
					}
					add(strings.Join(rest, " "))
				case shells[name]:
					if code, ok := shellCode(argv); ok {
						add(code)
					}
				case name == "trap":
					// `trap 'code' SIG…`: the code runs later in this shell (also after the last command).
					for _, a := range argv[1:] {
						if !strings.HasPrefix(a, "-") {
							add(a)
							break
						}
					}
				default:
					for _, code := range runnerCode(name, argv) {
						add(code)
					}
				}
			}
		}
		return sc
	}
	walk(src, 0)
	return out
}

func base(s string) string { return filepath.Base(s) }

var (
	reAssign = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*=`)
	keywords = map[string]bool{
		"if": true, "then": true, "else": true, "elif": true, "do": true, "while": true,
		"until": true, "!": true, "coproc": true, "{": true, "}": true, "time": true, "fi": true, "done": true,
	}
)

// wrapperValueFlags lists, per wrapper program, the short flags that take a value.
var wrapperValueFlags = map[string]string{
	"env": "uCS", "command": "", "builtin": "", "exec": "a", "nohup": "", "nice": "n",
	"timeout": "sk", "xargs": "IinLPldeEsa", "sudo": "ugCDhpRrTU", "doas": "uC", "stdbuf": "ioe",
	"busybox": "", // `busybox sh -c …`, `busybox git …`: the applet is the real program
}

// Values are required unless listed separately as optional GNU long arguments.
var wrapperLongValues = map[string]map[string]bool{
	"nice": {"--adjustment": true}, "timeout": {"--signal": true, "--kill-after": true},
	"xargs": {"--max-args": true, "--max-procs": true, "--delimiter": true, "--arg-file": true,
		"--max-chars": true, "--max-lines": true, "--process-slot-var": true},
	"stdbuf": {"--input": true, "--output": true, "--error": true},
	"sudo": {"--user": true, "--group": true, "--chdir": true, "--close-from": true, "--host": true,
		"--prompt": true, "--chroot": true, "--role": true, "--type": true, "--command-timeout": true,
		"--other-user": true},
}

var wrapperLongFlags = map[string][]string{
	"nice":    {"--help", "--version"},
	"timeout": {"--foreground", "--preserve-status", "--verbose", "--help", "--version"},
	"xargs": {"--null", "--no-run-if-empty", "--interactive", "--verbose", "--exit", "--show-limits",
		"--open-tty", "--help", "--version", "--replace", "--eof"},
	"stdbuf": {"--help", "--version"},
	"sudo": {"--askpass", "--background", "--bell", "--edit", "--help", "--login", "--list",
		"--non-interactive", "--preserve-env", "--remove-timestamp", "--reset-timestamp", "--set-home",
		"--shell", "--stdin", "--validate", "--version"},
	"command": {}, "builtin": {}, "exec": {}, "nohup": {"--help", "--version"},
	"doas": {}, "busybox": {"--help"},
}

// resolveWrapperLong follows getopt_long: exact spellings win, otherwise only
// one matching prefix is accepted. Optional values consume only an attached '='.
func resolveWrapperLong(name, key string) (full string, required bool) {
	options := map[string]bool{}
	for opt := range wrapperLongValues[name] {
		options[opt] = true
	}
	for _, opt := range wrapperLongFlags[name] {
		options[opt] = false
	}
	if required, ok := options[key]; ok {
		return key, required
	}
	for opt, value := range options {
		if strings.HasPrefix(opt, key) {
			if full != "" {
				return "", false
			}
			full, required = opt, value
		}
	}
	return
}

// cmdView is a simple command with keywords, assignments and wrapper programs stripped.
type cmdView struct {
	start          int      // index of the real program in words
	wrappers       []string // wrapper programs stripped (env, xargs, sudo, …)
	splitString    bool     // `env -S` / `--split-string`: the program comes from a string the guard cannot see
	envClear       bool     // `env -i` / `env -` / `--ignore-environment` / `exec -c`: the program runs with an emptied environment
	envUnset       []string // variables removed by `env -u NAME` / `-uNAME` / `--unset[=]NAME`
	wrapperUnknown string   // first unknown or ambiguous wrapper long option
	envUnknown     string   // first env long option that is not a full known spelling (abbreviation, unknown)
	lead           int      // number of leading keywords and shell assignments (before any program word)
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
	v.lead = i
	for i < len(words) {
		name := progName(words[i])
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
			if w == "-" && name == "env" { // `env -` is `env -i`
				v.envClear = true
				i++
				continue
			}
			if strings.HasPrefix(w, "-") && len(w) > 1 {
				if name != "env" && strings.HasPrefix(w, "--") {
					key, _, eq := strings.Cut(w, "=")
					full, required := resolveWrapperLong(name, key)
					if full == "" && v.wrapperUnknown == "" {
						v.wrapperUnknown = name + " " + w
					}
					i++
					if required && !eq && i < len(words) {
						i++
					}
					continue
				}
				longs := wrapperLongValues[name]
				if name == "env" {
					longs = map[string]bool{"--unset": true, "--chdir": true, "--split-string": true}
				}
				optional := ""
				if name == "xargs" {
					optional = "iel"
				}
				opts, next := utilityOptions(words, i, vf, longs, optional)
				for _, opt := range opts {
					if name == "exec" && opt.name == "c" {
						v.envClear = true
					}
					if name != "env" {
						continue
					}
					switch opt.name {
					case "i", "--ignore-environment":
						v.envClear = true
					case "S", "--split-string":
						v.splitString = true
					case "u", "--unset":
						v.envUnset = append(v.envUnset, opt.value)
					case "C", "--chdir", "v", "0", "--null", "--debug", "--help", "--version", "--list-signal-handling", "--block-signal", "--default-signal", "--ignore-signal":
					default:
						if strings.HasPrefix(opt.name, "--") && v.envUnknown == "" {
							v.envUnknown = w
						}
					}
				}
				i = next
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
