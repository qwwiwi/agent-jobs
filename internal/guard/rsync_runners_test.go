package guard

import "testing"

// Issue #179: the runner chain in front of rsync is parsed per runner (its own options and
// operands), so an rsync option value or a runner operand is never read as the executed program,
// while a real rsync reached through flock/parallel/find/xargs keeps its remote checks.
func TestRsyncRunnerChains(t *testing.T) {
	t.Setenv("HOME", "/home/u")
	t.Setenv("CDPATH", "")
	cfg := Config{Dir: "/home/u/work"}
	cases := []struct {
		name  string
		cmd   string
		block bool
	}{
		// False refusals from the issue: local copies stay allowed.
		{"setsid exclude value names rsync", `setsid rsync --exclude 'rsync cache' ./src/ ./dst/`, false},
		{"exclude value with variable names rsync", `rsync --exclude "rsync $P" ./src/ ./dst/`, false},
		{"find exec exclude values name runners", `find . -exec rsync --exclude parallel --exclude rsync ./src/ ./dst/ \;`, false},
		{"flock lock file named parallel", `flock parallel rsync ./src/ ./dst/`, false},
		// More local shapes.
		{"flock with options", `flock -w 5 -x ./lock rsync -a ./src/ ./dst/`, false},
		{"setsid flock chain", `setsid -f flock ./lock rsync ./src/ ./dst/`, false},
		{"nice timeout rsync", `nice -n 5 timeout 30 rsync -a ./src/ ./dst/`, false},
		{"find exec local", `find . -maxdepth 0 -exec rsync -a ./src/ ./dst/ \;`, false},
		// The bypass from loop 2 and its variants stay refused.
		{"flock rsync lock then parallel rsync", `printf '%s\n' host:/dst/ | flock rsync parallel -j 2 rsync ./src/`, true},
		{"flock parallel rsync remote", `flock ./lock parallel rsync ./src/ ::: host:/dst/`, true},
		{"parallel rsync remote args", `parallel rsync ./src/ ::: host:/dst/`, true},
		{"parallel options then rsync", `parallel -j 2 --halt now,fail=1 rsync ./src/ ::: host:/dst/`, true},
		{"parallel rsync stdin", `echo host:/dst/ | parallel rsync ./src/`, true},
		{"setsid parallel rsync", `echo h:/x | setsid parallel rsync ./src/`, true},
		{"xargs rsync stdin", `echo host:/dst/ | xargs rsync ./src/`, true},
		{"xargs options rsync stdin", `echo host:/dst/ | xargs -n 1 -P 2 rsync ./src/`, true},
		{"find exec xargs rsync", `find . -exec xargs rsync ./src/ \;`, true},
		{"find exec parallel rsync", `find . -exec parallel rsync ./src/ ::: h:/x \;`, true},
		{"flock lock rsync remote", `flock ./lock rsync ./src/ host:/dst/`, true},
		{"flock lock named rsync then rsync remote", `flock rsync rsync ./src/ host:/dst/`, true},
		{"setsid rsync remote", `setsid rsync ./src/ host:/dst/`, true},
		{"find exec remote", `find . -exec rsync --exclude parallel ./src/ host:/dst/ \;`, true},
		{"flock -c rsync remote", `flock ./lock -c 'rsync ./src/ host:/dst/'`, true},
		{"unknown runner option fails closed", `flock --zzz ./lock rsync ./src/ host:/dst/`, true},
		// An outer xargs extends the argv of the command a runner runs.
		{"xargs flock rsync stdin", `echo h:/x | xargs flock ./l rsync ./src/`, true},
		{"xargs setsid rsync stdin", `echo h:/x | xargs setsid rsync ./src/`, true},
		// An unquoted lock file may split into a lock file and a stdin runner.
		{"bare lock file", `echo h:/x | flock $L rsync ./src/ ./dst/`, true},
		// time is not modeled: its -o value is not a program, parallel behind it still is.
		{"flock time -o rsync parallel", `printf '%s\n' h:/x | flock ./l time -o rsync parallel rsync ./src/`, true},
		// Unmodeled runners and exec options inside a chain keep the position checks.
		{"setsid ionice remote", `setsid ionice -c 3 rsync ./src/ h:/x`, true},
		{"setsid ionice local", `setsid ionice -c 3 rsync ./src/ ./dst/`, false},
		{"setsid fd -x remote", `setsid fd -x rsync ./src/ h:/x`, true},
		// The program behind a runner comes from xargs input or a variable: it may be rsync.
		{"xargs setsid program from input", `echo rsync ./a h:/x | xargs setsid`, true},
		{"xargs flock program from input", `echo rsync ./a h:/x | xargs flock ./l`, true},
		{"setsid quoted variable program", `setsid "rs$Y" ./a h:/x`, true},
		{"flock quoted variable program", `flock ./l "rs$Y" ./a h:/x`, true},
		{"find exec quoted variable program", `find . -exec "rs$Y" ./a h:/x \;`, true},
		// A quoted lock file variable: the lock file, or an option that makes rsync the lock file.
		{"quoted lock file variable", `flock "$LOCK" rsync -a ./a/ ./b/`, false},
		{"quoted lock file under home", `flock -n "$HOME/.cache/x.lock" rsync -a ./a/ ./b/`, false},
		{"quoted timeout value", `flock -w "$T" ./l rsync ./a ./b`, false},
		{"quoted lock variable as option", `L=-n; printf '%s\n' h:/x | flock "$L" rsync parallel rsync ./src/`, true},
		{"quoted lock variable remote", `flock "$LOCK" rsync ./a h:/x`, true},
		// `"$X"` may be -w/-E: its value is any word strtod/strtol reads, then the lock file.
		{"quoted option exponent value", `flock "$X" 1e3 L rsync host:/x /y`, true},
		{"quoted option signed value", `flock "$X" +5 L rsync host:/x /y`, true},
		{"quoted option hex value", `flock "$X" 0x10 L rsync host:/x /y`, true},
		{"quoted option after flag", `flock -s "$X" 1e3 ./l rsync -a ./a host:/x`, true},
		{"quoted option inf value", `flock "$X" inf L rsync host:/x /y`, true},
		// A runner operand is data, not a program.
		{"lock file with space named rsync", `flock 'rsync lock' rsync ./src/ ./dst/`, false},
		{"setsid flock options and excludes", `setsid -f flock -n -w 3 ./l rsync -a --exclude 'rsync x' ./src/ ./dst/`, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := Check(c.cmd, cfg)
			if (got != "") != c.block {
				t.Fatalf("Check(%q) = %q, want block=%v", c.cmd, got, c.block)
			}
		})
	}
}
