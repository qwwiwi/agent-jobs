package guard

import "slices"

// Relaxations of false refusals measured in docs/BENCHMARK.md («Guard refusals»): forms the guard
// refused although they reach nothing a script file with the same text would not reach. Each one is
// narrow: the surrounding checks still run on everything else the command does.

// gitSubstReads are git subcommands that only print repository data: inside a command substitution
// (`n=$(git rev-list --count HEAD)`) they are checked like the same call written as a plain command.
var gitSubstReads = map[string]bool{
	"log": true, "diff": true, "rev-list": true, "show": true, "rev-parse": true, "status": true,
	"ls-files": true, "ls-tree": true, "merge-base": true, "describe": true, "shortlog": true,
	"blame": true, "diff-tree": true,
}

// substGitRead reports a read-only git call whose script is reached from the command through command
// or process substitutions only (no eval, `sh -c` or runner body), with no `-c` configuration: the
// substitution prints what the plain call prints, and every other rule still checks the nested call.
func substGitRead(sc *script, argv []string) bool {
	if !sc.subst || len(argv) == 0 || progName(argv[0]) != "git" {
		return false
	}
	g, _ := parseGit(argv)
	return len(g.configs) == 0 && g.attached == "" && g.subIdx < len(argv) && gitSubstReads[g.sub]
}

// dataSinks copy their standard input to standard output or to the files they name: a here-doc they
// read is file content, not code.
var dataSinks = map[string]bool{"cat": true, "tee": true}

// dataHeredoc reports a here-doc that is only written out as data (`cat > notes.md <<'EOF'`): the
// command is cat or tee with no wrapper, alone in its pipeline at the top level (nothing reads its
// output as code), with no substitution, process substitution or expanding redirection, and every
// here-doc of it literal (quoted delimiter, or a body without $ and backticks) and no here-string.
// Its body is then not scanned for paths: mentioning a path in a file's text does not open it.
func dataHeredoc(sc *script, pl *pipeline, c *simpleCmd) bool {
	if sc.depth != 0 || c.heredoc == "" || c.expandingStdin || len(pl.cmds) != 1 {
		return false
	}
	if len(c.feeds) > 0 || len(c.incoming) > 0 || len(c.subSources) > 0 || len(c.inputSources) > 0 ||
		slices.Contains(c.outDyn, true) || slices.Contains(c.dyn, true) {
		return false
	}
	v := split(c.words)
	if len(v.wrappers) > 0 || v.start >= len(c.words) {
		return false
	}
	return dataSinks[progName(c.words[v.start])]
}
