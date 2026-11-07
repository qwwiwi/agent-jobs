package guard

import "strings"

// The runner chain of a command (issue #179): which program each runner in front of rsync really
// runs, read per runner syntax. `flock parallel rsync a b` runs rsync (parallel is the lock file);
// `flock rsync parallel rsync a` runs parallel (rsync is the lock file), which runs rsync with
// arguments from stdin; `find . -exec rsync --exclude parallel a b \;` runs one rsync whose
// --exclude value is data. A runner shape the walk does not model is handed to the legacy
// position heuristic (legacyRemoteCopyArg) or, where its words cannot be read, refuses every rsync
// word after it: the walk fails closed.

// chainSpan is a range [at, end) of argv: a command word and its arguments.
type chainSpan struct {
	at, end int
	stdin   bool // the command's argv gets extra words from stdin (an outer xargs)
}

// runnerChain is the result of walking one argv.
type runnerChain struct {
	rsyncs []chainSpan  // the rsync commands the chain runs; stdin = argv not fully visible
	tails  []chainSpan  // spans the walk cannot read: any rsync word in them is refused
	legacy []chainSpan  // programs not modeled here, checked by legacyRemoteCopyArg
	blind  []int        // a runner whose program the guard cannot see (a variable, xargs input)
	data   map[int]bool // runner option words, their values and operands (a lock file): never a program
	// ambiguous: a word was walked in several readings (flockFrom); data then holds the words of
	// every reading, so no word may be skipped as data (chainData).
	ambiguous bool
	dyn       []bool
	bare      []bool
}

func (ch *runnerChain) isDyn(i int) bool  { return i < len(ch.dyn) && ch.dyn[i] }
func (ch *runnerChain) isBare(i int) bool { return i < len(ch.bare) && ch.bare[i] }

// walkChain walks argv (argv[0] is the program) from its first word. stdin marks an argv that an
// outer xargs extends with words from its input.
func walkChain(argv []string, dyn, bare []bool, stdin bool) *runnerChain {
	ch := &runnerChain{data: map[int]bool{}, dyn: dyn, bare: bare}
	ch.command(argv, chainSpan{0, len(argv), stdin})
	return ch
}

// command classifies the program at s.at and walks into it when it is a modeled runner.
func (ch *runnerChain) command(argv []string, s chainSpan) {
	if s.at >= s.end {
		if s.stdin && s.at > 0 { // `xargs setsid`: the program itself comes from input
			ch.blind = append(ch.blind, s.at-1)
		}
		return
	}
	if s.at > 0 && (ch.isDyn(s.at) || ch.isBare(s.at)) { // a program from a variable: any program
		ch.blind = append(ch.blind, s.at)
		return
	}
	name := progName(argv[s.at])
	switch {
	case name == "rsync":
		ch.rsyncs = append(ch.rsyncs, s)
	case stdinRunners[name]: // parallel, entr, xargs: the command's arguments come from input
		ch.tails = append(ch.tails, s)
	case name == "find":
		ch.find(argv, s)
	case name == "flock":
		ch.flock(argv, s)
	case name == "setsid":
		ch.options(argv, s, "cfwhV", "", map[string]bool{"--ctty": false, "--fork": false, "--wait": false,
			"--help": false, "--version": false})
	case isWrapper(name):
		ch.wrapper(argv, s)
	default:
		ch.legacy = append(ch.legacy, s)
	}
}

// isWrapper reports a program that split strips as a wrapper (wrapperValueFlags).
func isWrapper(name string) bool { _, ok := wrapperValueFlags[name]; return ok }

// options walks a runner of the form `runner [options] command [args]` (getopt with '+': options end
// at the first operand or `--`). shortFlags take no value, shortValues take the rest of the word or
// the next word, longs maps every long option to whether it takes a value.
func (ch *runnerChain) options(argv []string, s chainSpan, shortFlags, shortValues string, longs map[string]bool) {
	i, st := ch.runnerOptions(argv, s, s.at+1, shortFlags, shortValues, longs)
	if st != optsDone {
		ch.tails = append(ch.tails, s)
		return
	}
	ch.command(argv, chainSpan{i, s.end, s.stdin})
}

// optsState is how runnerOptions ended.
type optsState int

const (
	optsDone      optsState = iota // at the first operand (or the end)
	optsUnknown                    // an unknown or ambiguous option, or an unquoted expansion
	optsAmbiguous                  // a quoted expansion that may be an option or the first operand
)

// runnerOptions consumes the options of the runner at s.at, from index from, and returns the index
// of its first operand. A separate option value may be a quoted expansion (`-w "$T"`): it is one
// word, data whatever it holds.
func (ch *runnerChain) runnerOptions(argv []string, s chainSpan, from int, shortFlags, shortValues string,
	longs map[string]bool) (int, optsState) {
	i := from
	for i < s.end {
		w := argv[i]
		if ch.isBare(i) { // an unquoted expansion may split into options and operands
			return i, optsUnknown
		}
		if ch.isDyn(i) {
			// A quoted expansion is one word: an operand when its static text already shows it
			// (`"./locks/$L"`), else it may spell an option too (`"$LOCK"`).
			if w != "" && !strings.ContainsAny(w[:1], "-$`") {
				return i, optsDone
			}
			return i, optsAmbiguous
		}
		if w == "--" {
			ch.data[i] = true
			return i + 1, optsDone
		}
		if !strings.HasPrefix(w, "-") || w == "-" {
			return i, optsDone
		}
		next, ok := runnerOption(argv, i, s.end, shortFlags, shortValues, longs)
		if !ok || next > s.end {
			return i, optsUnknown
		}
		for j := i; j < next; j++ {
			if ch.isBare(j) {
				return i, optsUnknown
			}
			ch.data[j] = true
		}
		i = next
	}
	return i, optsDone
}

// runnerOption reads the option word argv[i] and returns the index after it and its value.
// Long options follow getopt_long: an exact spelling or a unique prefix.
func runnerOption(argv []string, i, end int, shortFlags, shortValues string, longs map[string]bool) (int, bool) {
	w := argv[i]
	if strings.HasPrefix(w, "--") {
		key, _, eq := strings.Cut(w, "=")
		full := ""
		if _, exact := longs[key]; exact {
			full = key
		} else {
			for opt := range longs {
				if strings.HasPrefix(opt, key) {
					if full != "" {
						return i, false // ambiguous
					}
					full = opt
				}
			}
		}
		if full == "" {
			return i, false
		}
		switch takesValue := longs[full]; {
		case takesValue && !eq:
			return i + 2, i+1 < end
		case !takesValue && eq:
			return i, false
		}
		return i + 1, true
	}
	for k := 1; k < len(w); k++ {
		switch c := w[k]; {
		case strings.IndexByte(shortFlags, c) >= 0:
		case strings.IndexByte(shortValues, c) >= 0:
			if k < len(w)-1 {
				return i + 1, true
			}
			return i + 2, i+1 < end
		default:
			return i, false
		}
	}
	return i + 1, true
}

// flock: `flock [options] file|dir command [args]`, `flock [options] file|dir -c command`,
// `flock [options] fd` (util-linux, getopt "+sexnoFuw:E:hV?"). The first operand is the lock
// file, never a program.
func (ch *runnerChain) flock(argv []string, s chainSpan) { ch.flockFrom(argv, s, s.at+1, 0) }

// flockOpts are flock's options: short flags, short options with a value, long options.
var flockOpts = struct {
	flags, values string
	longs         map[string]bool
}{"sexnoFuhV?", "wE", map[string]bool{
	"--shared": false, "--exclusive": false, "--unlock": false, "--nonblock": false, "--nb": false,
	"--timeout": true, "--wait": true, "--conflict-exit-code": true, "--close": false,
	"--no-fork": false, "--verbose": false, "--help": false, "--version": false,
}}

// flockFrom walks flock's options from index from. A quoted expansion where an option or the lock
// file may stand (`flock "$LOCK" rsync …`) is walked as each: the lock file, a one-word option,
// or -w/-E with the next word as its (numeric) value; every reading's rsyncs, tails and legacy
// programs count (ambiguous: no word is taken as data, see chainData). Past maxFlockForks such
// words the argv is unreadable.
func (ch *runnerChain) flockFrom(argv []string, s chainSpan, from, forks int) {
	i, st := ch.runnerOptions(argv, s, from, flockOpts.flags, flockOpts.values, flockOpts.longs)
	switch st {
	case optsUnknown:
		ch.tails = append(ch.tails, s)
		return
	case optsAmbiguous:
		if forks >= maxFlockForks {
			ch.tails = append(ch.tails, s)
			return
		}
		ch.ambiguous = true
		ch.flockOperand(argv, s, i)
		ch.flockFrom(argv, s, i+1, forks+1)
		if i+1 < s.end && numeric(argv[i+1]) && !ch.isDyn(i+1) {
			ch.flockFrom(argv, s, i+2, forks+1)
		}
		return
	}
	ch.flockOperand(argv, s, i)
}

// maxFlockForks bounds the readings flockFrom walks (3^n).
const maxFlockForks = 3

// numeric reports a word that flock -w/-E could take as its value (seconds or an exit code).
func numeric(w string) bool {
	return w != "" && strings.Trim(w, "0123456789.") == ""
}

// flockOperand walks flock from its lock file at index i: then the command or `-c` string.
func (ch *runnerChain) flockOperand(argv []string, s chainSpan, i int) {
	if i >= s.end {
		if s.stdin { // `xargs flock`: the lock file and the program come from input
			ch.blind = append(ch.blind, s.at)
		}
		return
	}
	ch.data[i] = true
	i++
	if i < s.end && (argv[i] == "-c" || argv[i] == "--command") {
		// The command string runs in a shell: it stays subject to the code checks (rsyncInCode).
		ch.tails = append(ch.tails, chainSpan{i + 1, s.end, s.stdin})
		return
	}
	ch.command(argv, chainSpan{i, s.end, s.stdin})
}

// find: each -exec/-execdir/-ok/-okdir action runs the command from the word after it up to its
// terminator (findExecEnd); every other word belongs to find's expression. Words appended by an
// outer xargs, or an unquoted expansion in find's expression, may add actions of their own.
func (ch *runnerChain) find(argv []string, s chainSpan) {
	if s.stdin {
		ch.tails = append(ch.tails, s)
		return
	}
	args := argv[s.at+1 : s.end]
	off := s.at + 1
	for i := 0; i < len(args); i++ {
		if ch.isBare(off + i) {
			ch.tails = append(ch.tails, chainSpan{off + i, s.end, false})
			return
		}
		if !findExec[args[i]] || i+1 >= len(args) {
			continue
		}
		end := findExecEnd(args, i+1)
		ch.command(argv, chainSpan{off + i + 1, off + end, false})
		i = end
	}
}

// wrapper walks a wrapper that split strips (env, nice, timeout, xargs, sudo, stdbuf, nohup, …)
// with split's own option rules; an option split cannot resolve, or an unreadable word among the
// wrapper's words, makes the rest unreadable.
func (ch *runnerChain) wrapper(argv []string, s chainSpan) {
	v := split(argv[s.at:s.end])
	if v.splitString || v.wrapperUnknown != "" || v.envUnknown != "" || v.lead != 0 {
		ch.tails = append(ch.tails, s)
		return
	}
	start := s.at + v.start
	for j := s.at + 1; j < start; j++ {
		if ch.isDyn(j) || ch.isBare(j) {
			ch.tails = append(ch.tails, s)
			return
		}
		ch.data[j] = true
	}
	stdin := s.stdin
	for _, w := range v.wrappers {
		stdin = stdin || w == "xargs"
	}
	ch.command(argv, chainSpan{start, s.end, stdin})
}
