package guard

import (
	"regexp"
	"slices"
	"strings"
)

// relaxable reports a command the merge reader may read relaxed (GhPrMerges): one whose words, commands
// and quoted here-document bodies the shell reader surely reads as bash does. It is read on its own text,
// apart from the reader: outside the bodies of here-documents with a quoted delimiter it holds none of the
// forms whose boundaries the reader may misplace (`${`, `$[`, `$'`, `$"`, `((`, `[[`, a backtick, `<(`, `>(`,
// an array `=(`, an unquoted `#`, `eval`, `source`, `.`, `case`, `coproc`, `alias`, `shopt`, `set` with
// an o option, a shell's -O / +O, an extglob-like `@( !( +( *( ?(`, a control character but tab and
// newline), its quotes and parentheses are balanced, and every here-document is `<<'WORD'` or `<<"WORD"`
// (WORD a plain name) whose closing line is exactly WORD, with no line of its body ending in a backslash.
// The bodies found must be the ones the reader recorded (scripts' literalBodies), byte for byte.
func relaxable(cmd string, scripts []*script) bool {
	code, bodies, ok := splitQuotedBodies(cmd)
	if !ok {
		return false
	}
	var read []string
	for _, sc := range scripts {
		for _, r := range sc.literalBodies {
			read = append(read, sc.src[r[0]:r[1]])
		}
	}
	slices.Sort(read)
	slices.Sort(bodies)
	if !slices.Equal(read, bodies) {
		return false
	}
	// Quotes and backslashes dropped, continuations joined: `e"v"al` and `ev\al` are eval.
	plain := strings.Map(func(r rune) rune {
		if r == '\'' || r == '"' || r == '\\' {
			return -1
		}
		return r
	}, strings.ReplaceAll(code, "\\\n", ""))
	if reUnmodeled.MatchString(code) || reUnmodeled.MatchString(plain) || reShellOptionO.MatchString(plain) {
		return false
	}
	return !reUnmodeledWord.MatchString(strings.ToLower(plain))
}

var (
	// reUnmodeled is a form whose word or command boundaries the shell reader may place elsewhere than
	// bash: a parameter expansion, $[ arithmetic, ANSI-C or locale quoting, arithmetic, a [[ test, a backtick, process
	// substitution, an array assignment, an extglob-like pattern, a here-string or <<-, a control character.
	reUnmodeled = regexp.MustCompile("\\$\\{|\\$\\[|\\$'|\\$\"|\\(\\(|\\[\\[|`|<\\(|>\\(|=\\(|[@?*+!]\\(|<<<|<<-|[\\x00-\\x08\\x0b-\\x1f\\x7f]")
	// reShellOptionO is a flag cluster with O (`bash -O extglob`, `+O`).
	reShellOptionO = regexp.MustCompile(`(^|\s)[-+][A-Za-z]*O`)
	// reUnmodeledWord is, case folded, a word that runs or reads code or changes how it is read (eval,
	// source, ., case, coproc, alias, shopt, set with an o option such as `set -o` or `set -euo`).
	reUnmodeledWord = regexp.MustCompile(`(^|[^a-z0-9_-])(eval|source|case|coproc|alias|shopt)([^a-z0-9_-]|$)` +
		`|(^|[\s;&|(){}])\.([\s;&|(){}]|$)|(^|[^a-z0-9_-])set(\s+\S+)*?\s+[-+][a-z]*o`)
	// reHeredocWord is the delimiter of a relaxable here-document: a plain name.
	reHeredocWord = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*`)
)

// splitQuotedBodies splits cmd into its code (every byte outside quoted here-document bodies, quoted
// strings included) and the bodies of its here-documents, each with its closing line, as the shell
// reader records them (script.literalBodies). ok is false for any form relaxable refuses: an unquoted #,
// unbalanced quotes or parentheses, a `$(` whose end the reader's matchParen (which knows no
// here-document) places elsewhere, a here-document operator other than <<'WORD' / <<"WORD" or a << that
// is none (in quotes), a body without its closing line or with a line ending in a backslash, and on a line
// whose here-documents are pending a newline inside quotes or at another nesting, a continuation or a
// closing quote or paren.
func splitQuotedBodies(cmd string) (code string, bodies []string, ok bool) {
	type ctx struct {
		double bool // inside "…"; else the command itself, a $( ) or a ( ) group in it
		sub    int  // the index of the ( of a $( ), else -1
	}
	var b strings.Builder
	stack := []ctx{{sub: -1}}
	var pending []string // delimiters of here-documents whose bodies start after the next newline
	pendDepth := 0       // the stack depth at the first pending operator
	operators := 0
	pop := func(i int) bool {
		if len(pending) > 0 && len(stack) <= pendDepth {
			return false
		}
		top := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		return top.sub < 0 || matchParen(cmd, top.sub) == i+1
	}
	for i := 0; i < len(cmd); {
		c := cmd[i]
		top := stack[len(stack)-1]
		if top.double {
			switch {
			case c == '\\':
				if i+1 >= len(cmd) || cmd[i+1] == '\n' && len(pending) > 0 {
					return "", nil, false
				}
				b.WriteString(cmd[i : i+2])
				i += 2
				continue
			case c == '"':
				if !pop(i) {
					return "", nil, false
				}
			case c == '$' && i+1 < len(cmd) && cmd[i+1] == '(':
				stack = append(stack, ctx{sub: i + 1})
				b.WriteString("$(")
				i += 2
				continue
			case c == '\n' && len(pending) > 0:
				return "", nil, false
			}
			b.WriteByte(c)
			i++
			continue
		}
		switch {
		case c == '\\':
			if i+1 >= len(cmd) || cmd[i+1] == '\n' && len(pending) > 0 {
				return "", nil, false
			}
			b.WriteString(cmd[i : i+2])
			i += 2
			continue
		case c == '\'':
			end := strings.IndexByte(cmd[i+1:], '\'')
			if end < 0 || len(pending) > 0 && strings.Contains(cmd[i+1:i+1+end], "\n") {
				return "", nil, false
			}
			b.WriteString(cmd[i : i+end+2])
			i += end + 2
			continue
		case c == '"':
			stack = append(stack, ctx{double: true, sub: -1})
		case c == '#':
			return "", nil, false
		case c == '(':
			open := -1
			if i > 0 && cmd[i-1] == '$' {
				open = i
			}
			stack = append(stack, ctx{sub: open})
		case c == ')':
			if len(stack) == 1 || !pop(i) {
				return "", nil, false
			}
		case c == '<' && strings.HasPrefix(cmd[i:], "<<"):
			j := i + 2
			if j >= len(cmd) || cmd[j] != '\'' && cmd[j] != '"' {
				return "", nil, false
			}
			word := reHeredocWord.FindString(cmd[j+1:])
			k := j + 1 + len(word)
			if word == "" || k >= len(cmd) || cmd[k] != cmd[j] || k+1 < len(cmd) && !strings.ContainsRune(" \t\n;&|)<>", rune(cmd[k+1])) {
				return "", nil, false
			}
			if len(pending) == 0 {
				pendDepth = len(stack)
			}
			pending = append(pending, word)
			operators++
			b.WriteString(cmd[i : k+1])
			i = k + 1
			continue
		case c == '\n' && len(pending) > 0:
			if len(stack) != pendDepth {
				return "", nil, false // the line ends at another nesting than its operators
			}
			b.WriteByte(c)
			i++
			for _, delim := range pending {
				start := i
				for {
					if i >= len(cmd) {
						return "", nil, false // no closing line
					}
					line, _, found := strings.Cut(cmd[i:], "\n")
					i += len(line)
					if found {
						i++
					}
					if line == delim {
						break
					}
					if strings.HasSuffix(line, "\\") || !found {
						return "", nil, false
					}
				}
				bodies = append(bodies, cmd[start:i])
			}
			pending = nil
			continue
		}
		b.WriteByte(c)
		i++
	}
	code = b.String()
	if len(stack) != 1 || len(pending) > 0 || strings.Count(code, "<<") != operators {
		return "", nil, false
	}
	return code, bodies, true
}
