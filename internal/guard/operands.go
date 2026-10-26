package guard

import (
	"regexp"
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

// goFlagsExec reports a GOFLAGS value carrying one of goExecFlags.
func goFlagsExec(val string) bool {
	return slices.ContainsFunc(strings.Fields(val), func(f string) bool {
		flag, _, _ := strings.Cut(strings.TrimLeft(f, "-"), "=")
		return strings.HasPrefix(f, "-") && goExecFlags[flag]
	})
}

// goEnvSetters set an environment variable from a NAME=value argument (go via `go env -w`, which
// persists it for later calls).
var goEnvSetters = map[string]bool{"export": true, "declare": true, "typeset": true, "local": true, "readonly": true, "go": true}

// goEnv (issue #96) rejects a goEnvRuns assignment whose value names git/gh, a shell or an agent
// (`GOFLAGS=-toolexec=agent-jobs go test ./...`, `CC=claude go build`), in front of a command, under
// env, on its own (exported later or with set -a) or as an argument of export/declare/go env -w. A
// GOFLAGS value with a goExecFlags flag is allowed only in front of go itself, where staticArgv sees
// it: set elsewhere it would turn a later `go build ./cmd/agent-jobs` into a renamed agent build.
func goEnv(c *simpleCmd, v cmdView) string {
	isGo, setter := false, false
	if v.start < len(c.words) {
		name := progName(c.words[v.start])
		isGo, setter = name == "go", goEnvSetters[name]
	}
	for i, w := range c.words {
		if i == v.start && !setter {
			break
		}
		key, val, ok := strings.Cut(w, "=")
		if !ok || !goEnvRuns[key] {
			continue
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

// sedOperands: the input files of sed. The script (an -e/--expression value, or the first non-option
// word unless an -e/-f option comes before it) is no operand: `s/x/agent-jobs/e` runs a command. With
// POSIXLY_CORRECT sed stops at the first non-option word (`sed SCRIPT -e x f` takes SCRIPT, not x),
// so that word stays the script. An option sed is not known to take, or a long option abbreviation,
// fails closed (nil).
func sedOperands(args []string) []bool {
	ops := make([]bool, len(args))
	script, positional := -1, []int{} // script: index of the first -e/-f option
	for k := 0; k < len(args); k++ {
		a := args[k]
		switch {
		case a == "--":
			for k++; k < len(args); k++ {
				positional = append(positional, k)
			}
		case strings.HasPrefix(a, "--"):
			opt, _, eq := strings.Cut(a[2:], "=")
			switch {
			case opt == "expression" || opt == "file":
				if script < 0 {
					script = k
				}
				if !eq {
					k++ // the value is the next word
				}
			case !sedNoValue[opt] || (eq && opt != "in-place" && opt != "line-length"):
				return nil
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
				case 'e', 'f', 'l':
					if script < 0 && a[i] != 'l' {
						script = k
					}
					if i == len(a)-1 {
						k++ // the value is the next word
					}
					break short
				default:
					return nil
				}
			}
		default:
			positional = append(positional, k)
		}
	}
	if len(positional) > 0 && (script < 0 || positional[0] < script) {
		positional = positional[1:] // the script
	}
	for _, k := range positional {
		ops[k] = true
	}
	return ops
}

// reSedExec finds sed's e command or s///e flag: an `e` after an address (or its I/M flag), a `!`, a
// separator, a closing delimiter or a blank. The text after it, to the end of the line, is a shell
// command; it may follow without a blank (`sed 1eagent-jobs f`), so reMention alone does not see it.
var reSedExec = regexp.MustCompile(`(^|[0-9$/,!~;{}IM\s])e`)

// sedExecMention reports a sed script word in which an e command runs git/gh, a shell or an agent.
func sedExecMention(w string) bool {
	for _, m := range reSedExec.FindAllStringIndex(w, -1) {
		cmd, _, _ := strings.Cut(strings.TrimLeft(w[m[1]:], " \t"), "\n")
		if f := strings.Fields(cmd); len(f) > 0 && execName(progName(f[0])) || reMention.MatchString(cmd) {
			return true
		}
	}
	return false
}
