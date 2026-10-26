package guard

import (
	"slices"
	"strings"
)

// Operands (issue #95): some programs only compile, check, list, read or compare the files and
// packages their operands name and never run them, so `go test ./cmd/agent-jobs/` or
// `sed -n 1p cmd/agent-jobs/x.go` is data even though a path element names agent-jobs. The scan in
// unverifiable skips such operands; option words and their values are still scanned, and every other
// program keeps the fail-closed scan (`node …/claude-code/cli.js` runs the agent).

// goDataSubcommands build, test, check, list, format or document the packages named by their
// operands; go run, generate, tool, install and the rest are not listed (they run or install them).
var goDataSubcommands = map[string]bool{"test": true, "build": true, "vet": true, "list": true, "fmt": true, "doc": true}

// goExecFlags name a program for go to run (-exec, -toolexec, -vettool), or an output (-o) that may be
// a renamed agent executable (as with cp, see copiers): with one present no operand is data.
var goExecFlags = map[string]bool{"exec": true, "toolexec": true, "vettool": true, "o": true}

// fileOperandPrograms take only files (and option values that are no command) as operands.
var fileOperandPrograms = map[string]bool{"gofmt": true, "diff": true, "cmp": true}

// plainPath reports a literal file or package word: only letters, digits and . / _ - @ ~ +, so no
// code (spaces, quotes, parentheses, `;`), no `=` (a `-ldflags all=-extld=claude` value) and no
// expansion.
func plainPath(w string) bool {
	if w == "" || w == "-" {
		return false
	}
	for _, r := range w {
		if !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || strings.ContainsRune("./_-@~+", r)) {
			return false
		}
	}
	return true
}

// staticArgv reports a command whose words the shell passes as written: no word holds an expansion
// ($var, ${…}, $(…), backticks) or a glob/brace, and xargs does not add or replace words. Otherwise
// one word can turn the role of the next (`"$opt" claude` with opt=-exec, `xargs -I OPT go test OPT
// claude`), so no operand is exempt from the scan. A GOFLAGS assignment with a goExecFlags flag
// before the command (`GOFLAGS=-o=x go build ./cmd/agent-jobs`) counts as that flag on the command.
func staticArgv(c *simpleCmd, v cmdView) bool {
	for _, w := range c.words[:v.start] {
		if val, ok := strings.CutPrefix(w, "GOFLAGS="); ok && goFlagsExec(val) {
			return false
		}
	}
	return !slices.Contains(c.dyn, true) && !slices.Contains(c.glob, true) && !slices.Contains(v.wrappers, "xargs")
}

// goEnvRuns are the environment variables through which go runs a program: GOFLAGS (-exec,
// -toolexec, -vettool) and the cgo tools.
var goEnvRuns = map[string]bool{
	"GOFLAGS": true, "CC": true, "CXX": true, "FC": true, "AR": true, "GCCGO": true, "PKG_CONFIG": true,
}

// goFlagsExec reports a GOFLAGS value carrying one of goExecFlags, or a quote or a backslash: go
// splits GOFLAGS into fields with its own quoting rules (`'-o=x'` is the field -o=x), which the guard
// does not follow, so such a value counts as one with an exec flag.
func goFlagsExec(val string) bool {
	if strings.ContainsAny(val, `'"\`) {
		return true
	}
	return slices.ContainsFunc(strings.Fields(val), func(f string) bool {
		flag, _, _ := strings.Cut(strings.TrimLeft(f, "-"), "=")
		return strings.HasPrefix(f, "-") && goExecFlags[flag]
	})
}

// goEnvSetters set an environment variable from a NAME=value argument; so does `go env -w`, which
// persists it for later calls (goEnv). Other go arguments such as `go test … -args GOFLAGS=x` are none.
var goEnvSetters = map[string]bool{"export": true, "declare": true, "typeset": true, "local": true, "readonly": true}

// goEnvChanged (issues #95, #96) reports a call that sets GOFLAGS or GOENV for its go commands in a
// form the guard does not read: a word naming GOENV (a file of go settings), a word naming GOFLAGS
// other than a literal `GOFLAGS=value` (an appended `GOFLAGS+=`, a dynamic value, a bare name given
// to read, printf -v or export), a dynamic word given to a variable setter (`export "GOFL$x=…"`), or
// `.`/`source` of a file or a declare -n name reference (localVars). The variable lives on for the
// later commands of the call and the next loop iterations, so no go operand of the call is data.
func goEnvChanged(scripts []*script) bool {
	if _, all := localVars(scripts, "", false); all {
		return true
	}
	for _, sc := range scripts {
		for _, pl := range sc.pipelines {
			for _, c := range pl.cmds {
				v := split(c.words)
				setter := false
				if v.start < len(c.words) {
					name, args := progName(c.words[v.start]), c.words[v.start+1:]
					setter = varSetters[name] || name == "printf" && (printfTarget(args) != "" || len(args) > 0 && c.dyn[v.start+1])
				}
				for i, w := range c.words {
					if setter && i > v.start && c.dyn[i] || strings.Contains(w, "GOENV") ||
						strings.Contains(w, "GOFLAGS") && (c.dyn[i] || !strings.HasPrefix(w, "GOFLAGS=")) {
						return true
					}
				}
			}
		}
	}
	return false
}

// goEnv (issue #96) rejects a goEnvRuns assignment (`=` or `+=`) whose value names git/gh, a shell or
// an agent (`GOFLAGS=-toolexec=agent-jobs go test ./...`, `CC=claude go build`), in front of a command,
// under env, on its own (exported later or with set -a) or as an argument of export/declare/go env -w.
// A GOFLAGS value with a goExecFlags flag is allowed only in front of go itself, where staticArgv sees
// it: set elsewhere it would turn a later `go build ./cmd/agent-jobs` into a renamed agent build. go
// env -w keeps GOFLAGS for later calls, so there a dynamic value is rejected too.
func goEnv(c *simpleCmd, v cmdView) string {
	isGo, setter := false, false
	if v.start < len(c.words) {
		name := progName(c.words[v.start])
		isGo = name == "go"
		setter = goEnvSetters[name] || isGo && v.start+1 < len(c.words) && c.words[v.start+1] == "env"
	}
	for i, w := range c.words {
		if i == v.start && !setter {
			break
		}
		key, val, ok := strings.Cut(w, "=")
		key = strings.TrimSuffix(key, "+")
		if !ok || !goEnvRuns[key] {
			continue
		}
		if isGo && i > v.start && key == "GOFLAGS" && c.dyn[i] {
			return "go env -w GOFLAGS from a variable or substitution"
		}
		if execName(progName(val)) || reMention.MatchString(val) || (c.dyn[i] && mentionsExec(val)) {
			return "Go environment " + key + " running git/gh, a shell or an agent"
		}
		if key == "GOFLAGS" && goFlagsExec(val) && (!isGo || i >= v.start) {
			return "GOFLAGS with -exec, -toolexec, -vettool or -o outside a go command; pass the flag to go"
		}
	}
	return ""
}

// operands reports, for each of args (after the program name) of name (progName), whether it is a
// literal file or package operand that name never runs; nil when name is not known to be such a
// program or its options are not all understood. The caller passes only a staticArgv command.
func operands(name string, args []string) []bool {
	var ops []bool
	switch {
	case name == "go":
		ops = goOperands(args)
	case name == "sed":
		ops = sedOperands(args)
	case fileOperandPrograms[name]:
		ops = make([]bool, len(args))
		for k, a := range args {
			ops[k] = !strings.HasPrefix(a, "-")
		}
	}
	for k := range ops {
		ops[k] = ops[k] && plainPath(args[k])
	}
	return ops
}

// goOperands: `go SUB [flags] operands…` with SUB in goDataSubcommands and no goExecFlags flag; a word
// without a leading dash is an operand (a package, a file, a value of a flag that runs nothing, or a
// test binary argument after -args). go build of explicit .go files names its output after the first
// file (`go build cmd/agent-jobs/main.go` writes ./main, a renamed agent-jobs), so it is no data.
func goOperands(args []string) []bool {
	if len(args) == 0 || !goDataSubcommands[args[0]] {
		return nil
	}
	ops := make([]bool, len(args))
	for k := 1; k < len(args); k++ {
		a := args[k]
		if strings.HasPrefix(a, "-") {
			flag, _, _ := strings.Cut(strings.TrimLeft(a, "-"), "=")
			if goExecFlags[flag] {
				return nil
			}
			continue
		}
		if args[0] == "build" && strings.HasSuffix(strings.ToLower(a), ".go") {
			return nil
		}
		ops[k] = true
	}
	return ops
}

// sedNoValue are the GNU sed long options without a value (or with an optional attached one).
var sedNoValue = map[string]bool{
	"quiet": true, "silent": true, "regexp-extended": true, "posix": true, "separate": true, "unbuffered": true,
	"null-data": true, "debug": true, "sandbox": true, "follow-symlinks": true, "in-place": true, "line-length": true,
}

// sedArgs parses the arguments of sed: ops marks its input files, scripts are its script texts (the
// -e/--expression values, and the first non-option word unless an -e option comes before it). With
// POSIXLY_CORRECT sed stops at the first non-option word (`sed SCRIPT -e x f` takes SCRIPT, not x), so
// that word stays a script. ok is false for an option sed is not known to take, a long option
// abbreviation or a script file (-f/--file), whose text the guard does not see.
func sedArgs(args []string) (ops []bool, scripts []string, ok bool) {
	ops = make([]bool, len(args))
	script, positional := -1, []int{} // script: index of the first -e option
	next := func(k int) {             // the script given as the word after option k
		if k+1 < len(args) {
			scripts = append(scripts, args[k+1])
		}
	}
	for k := 0; k < len(args); k++ {
		a := args[k]
		switch {
		case a == "--":
			for k++; k < len(args); k++ {
				positional = append(positional, k)
			}
		case strings.HasPrefix(a, "--"):
			opt, val, eq := strings.Cut(a[2:], "=")
			switch {
			case opt == "expression":
				if script < 0 {
					script = k
				}
				if eq {
					scripts = append(scripts, val)
				} else {
					next(k)
					k++
				}
			case opt == "file" || !sedNoValue[opt] || (eq && opt != "in-place" && opt != "line-length"):
				return nil, nil, false
			case opt == "line-length" && !eq:
				k++
			}
		case strings.HasPrefix(a, "-") && len(a) > 1:
		short:
			for i := 1; i < len(a); i++ {
				switch a[i] {
				case 'n', 'E', 'r', 's', 'u', 'z':
				case 'i':
					break short // the rest is the backup suffix
				case 'e':
					if script < 0 {
						script = k
					}
					if i == len(a)-1 {
						next(k)
						k++
					} else {
						scripts = append(scripts, a[i+1:])
					}
					break short
				case 'l':
					if i == len(a)-1 {
						k++ // the value is the next word
					}
					break short
				default: // -f: a script file
					return nil, nil, false
				}
			}
		default:
			positional = append(positional, k)
		}
	}
	if len(positional) > 0 && (script < 0 || positional[0] < script) {
		scripts = append(scripts, args[positional[0]])
		positional = positional[1:]
	}
	for _, k := range positional {
		ops[k] = true
	}
	return ops, scripts, true
}

// sedOperands: the input files of sed; nil when a script may hold an e command or an s///e flag
// (sedCommandEs), when a script comes from a file or when sed's options are not all understood. Both
// run the input lines as commands, so an input file named after an agent is no data then.
func sedOperands(args []string) []bool {
	ops, scripts, ok := sedArgs(args)
	if !ok || slices.ContainsFunc(scripts, func(sc string) bool { return len(sedCommandEs(sc, true)) > 0 }) {
		return nil
	}
	return ops
}

// sedExecMention (issue #96) reports a sed call whose e command may run git/gh, a shell or an agent:
// the text after an e that may be a command, to the end of its line, names one. The text may follow
// the e without a blank (`sed 1eagent-jobs f`), so reMention alone does not see it. For a static argv
// only the script texts count, and an e inside a regex or a replacement is no command (`sed
// 's/eclaude/ok/' f`); otherwise (a dynamic word, an unknown option, -f) every e of every word counts.
func sedExecMention(args []string, static bool) bool {
	words, parse := args, false
	if static {
		if _, scripts, ok := sedArgs(args); ok {
			words, parse = scripts, true
		}
	}
	for _, w := range words {
		for _, k := range sedCommandEs(w, parse) {
			cmd, _, _ := strings.Cut(strings.TrimLeft(w[k+1:], " \t"), "\n")
			if f := strings.Fields(cmd); len(f) > 0 && execName(progName(f[0])) || reMention.MatchString(cmd) {
				return true
			}
		}
	}
	return false
}

// sedCommandEs returns the indices of the e letters of a sed script that may be an e command or an
// s///e flag: every e outside sedDataSpans. An e in an address regex or a replacement is no command;
// one in a label, a file name, a comment or a/i/c text still counts (a false positive only drops the
// operand exemption or rejects a rare script). Without parse every e counts.
func sedCommandEs(script string, parse bool) []int {
	var spans [][2]int
	if parse {
		spans = sedDataSpans(script)
	}
	var out []int
	for k := 0; k < len(script); k++ {
		if script[k] == 'e' && !slices.ContainsFunc(spans, func(sp [2]int) bool { return k >= sp[0] && k < sp[1] }) {
			out = append(out, k)
		}
	}
	return out
}

// sedDataSpans returns the byte ranges [start, end) of a sed script that sed reads as a regex or a
// replacement (an address /re/ or \cREc, the two parts of s and y); nil when the reader does not
// understand the script (an unknown command, a/i/c text with a backslash, s///w, an unterminated
// regex, a regex with an unescaped [), so that every e counts. Text it skips (labels, comments, file
// names, a/i/c text, an e command) holds no span, and it only resynchronises at the end of a line, so
// a span never covers text that sed reads as commands.
func sedDataSpans(s string) [][2]int {
	var spans [][2]int
	i := 0
	blank := func() {
		for i < len(s) && (s[i] == ' ' || s[i] == '\t') {
			i++
		}
	}
	digits := func() {
		for i < len(s) && s[i] >= '0' && s[i] <= '9' {
			i++
		}
	}
	// delimited reads the text from i to the unescaped delimiter d, records it and steps past d. In a
	// regex, sed reads a delimiter inside a bracket expression as data (`s/[/]/x/`); the reader does
	// not follow bracket expressions, so a regex with an unescaped [ fails.
	delimited := func(d byte, regex bool) bool {
		start := i
		for ; i < len(s); i++ {
			switch s[i] {
			case '\\':
				i++ // the escaped character, a newline too
			case '\n':
				return false
			case d:
				spans = append(spans, [2]int{start, i})
				i++
				return true
			case '[':
				if regex {
					return false
				}
			}
		}
		return false
	}
	// address reads one address; present is false when none starts at i.
	address := func() (present, ok bool) {
		if i >= len(s) {
			return false, true
		}
		switch c := s[i]; {
		case c >= '0' && c <= '9':
			digits()
			if i < len(s) && s[i] == '~' {
				i++
				digits()
			}
		case c == '$':
			i++
		case c == '/' || c == '\\':
			i++
			d := byte('/')
			if c == '\\' {
				if i >= len(s) || s[i] == '\n' || s[i] == '\\' {
					return true, false
				}
				d = s[i]
				i++
			}
			if !delimited(d, true) {
				return true, false
			}
			for i < len(s) && (s[i] == 'I' || s[i] == 'M') {
				i++
			}
		default:
			return false, true
		}
		return true, true
	}
	for {
		for i < len(s) && strings.IndexByte(" \t\n;", s[i]) >= 0 {
			i++
		}
		if i >= len(s) {
			return spans
		}
		present, ok := address()
		if !ok {
			return nil
		}
		if blank(); present && i < len(s) && s[i] == ',' {
			i++
			blank()
			if i < len(s) && (s[i] == '+' || s[i] == '~') {
				i++
				digits()
			} else if present, ok := address(); !present || !ok {
				return nil
			}
		}
		blank()
		for i < len(s) && s[i] == '!' {
			i++
			blank()
		}
		if i >= len(s) {
			return nil
		}
		c := s[i]
		i++
		switch {
		case c == '{' || c == '}':
			continue
		case strings.IndexByte("=dDgGhHnNpPxzF", c) >= 0:
		case strings.IndexByte("lqQL", c) >= 0:
			blank()
			digits()
		case c == 's' || c == 'y':
			if i >= len(s) || s[i] == '\n' || s[i] == '\\' {
				return nil
			}
			d := s[i]
			i++
			if !delimited(d, c == 's') || !delimited(d, false) {
				return nil
			}
			for c == 's' && i < len(s) && strings.IndexByte("gpiImMe0123456789", s[i]) >= 0 {
				i++
			}
		case c == 'b' || c == 't' || c == 'T' || c == ':':
			blank()
			for i < len(s) && strings.IndexByte(" \t\n;", s[i]) < 0 {
				i++
			}
		case strings.IndexByte("#aicerRwWv", c) >= 0: // the rest of the line is text, a file or a command
			line, _, _ := strings.Cut(s[i:], "\n")
			if strings.IndexByte("aic", c) >= 0 && strings.Contains(line, `\`) {
				return nil
			}
			i += len(line)
		default:
			return nil
		}
		blank()
		if i < len(s) && strings.IndexByte("\n;}#", s[i]) < 0 {
			return nil
		}
	}
}
