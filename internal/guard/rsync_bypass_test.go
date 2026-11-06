package guard

import "testing"

// Issue #155: remaining rsync bypasses after #116 (find terminator in context, inline interpreter
// code, dynamic operands) and two false refusals (--log-file-format value, -f filter data).
func TestRsyncRemoteBypassesRefused(t *testing.T) {
	t.Setenv("HOME", "/home/u")
	t.Setenv("CDPATH", "")
	cfg := Config{Dir: "/home/u/work"}
	cases := []struct {
		name  string
		cmd   string
		block bool
	}{
		// High: a literal `+` argument is not find's terminator.
		{"find -exec literal plus arg", `find . -maxdepth 0 -exec rsync --exclude + ./src/ host:/dst/ \;`, true},
		{"find -exec plus terminator then remote action", `find . -exec rsync a b {} + -exec rsync a host:/x \;`, true},
		// High: inline interpreter code naming rsync.
		{"python -c os.system rsync", `python3 -c 'import os; os.system("rsync ./src/ host:/dst/")'`, true},
		{"node -e rsync", `node -e 'require("child_process").execSync("rsync a h:/b")'`, true},
		{"perl -e rsync", `perl -e 'system("rsync", "a", "h:/b")'`, true},
		// High: dynamic operands.
		{"variable operand", `DST=host:/dst/; setsid rsync ./src/ "$DST"`, true},
		{"substitution operand", `rsync ./src/ "$(cat dst.txt)"`, true},
		{"find -exec dynamic operand", `D=host:/x; find . -maxdepth 0 -exec rsync a "$D" \;`, true},
		{"direct variable operand", `D=h:/x; rsync a $D`, true},
		// Medium: false refusals stay allowed.
		{"log-file-format value", `rsync --log-file-format '%f:%l' ./src/ ./dst/`, false},
		{"log-file-format attached", `rsync --log-file-format='%f:%l' ./src/ ./dst/`, false},
		{"filter data glob", `rsync -f'- *.cache' ./src/ ./dst/`, false},
		{"filter long data glob", `rsync --filter='- *.cache' ./src/ ./dst/`, false},
		// Local rsync with static operands and unrelated python stays allowed.
		{"local static", `rsync -a src/ dst/`, false},
		{"python without rsync", `python3 -c 'print(1)'`, false},
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

// Unit tests of the helpers behind #155 (the end-to-end guard has other rules that refuse a
// computed write target or a `{}` word, which would hide what these check).
func TestRsyncRemoteDynamism(t *testing.T) {
	cases := []struct {
		name string
		args []string
		dyn  []bool
		bare []bool // an expansion outside double quotes: may split into several words
		want bool
	}{
		{"dynamic bare operand", []string{"./src/", "$D"}, []bool{false, true}, nil, true},
		{"dynamic with local prefix", []string{"./src/", "./out/$D"}, []bool{false, true}, nil, false},
		{"dynamic with host prefix", []string{"./src/", "h:$D"}, []bool{false, true}, nil, true},
		{"dynamic filter value is data", []string{"--exclude", "$P", "./src/", "./dst/"}, []bool{false, true, false, false}, nil, false},
		{"dynamic attached filter is data", []string{"--exclude=$P", "./src/", "./dst/"}, []bool{true, false, false}, nil, false},
		{"dynamic option name", []string{"--$O", "a", "b"}, []bool{true, false, false}, nil, true},
		{"dynamic short cluster", []string{"-a$O", "a", "b"}, []bool{true, false, false}, nil, true},
		{"static dyn shorter than args", []string{"./a", "./b"}, nil, nil, false},
		// An unquoted expansion may split: neither an option's data value nor a local prefix.
		{"bare filter value splits", []string{"--exclude", "$P", "./src/", "./dst/"}, []bool{false, true, false, false}, []bool{false, true, false, false}, true},
		{"bare attached filter splits", []string{"--exclude=$P", "./src/", "./dst/"}, []bool{true, false, false}, []bool{true, false, false}, true},
		{"bare local prefix splits", []string{"./src/", "./out/$D"}, []bool{false, true}, []bool{false, true}, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := rsyncRemote(c.args, c.dyn, c.bare); got != c.want {
				t.Fatalf("rsyncRemote(%q, %v, %v) = %v, want %v", c.args, c.dyn, c.bare, got, c.want)
			}
		})
	}
}

func TestFindExecEnd(t *testing.T) {
	cases := []struct {
		name string
		args []string
		k    int
		want int
	}{
		{"semicolon ends", []string{"-exec", "rsync", "a", ";", "-printf", "x"}, 1, 3},
		{"plus after braces ends", []string{"-exec", "rsync", "a", "{}", "+", "-printf", "x"}, 1, 4},
		{"literal plus does not end", []string{"-exec", "rsync", "+", "h:/x", ";"}, 1, 4},
		{"plus after braces for -ok is an argument", []string{"-ok", "rsync", "{}", "+", "h:/x", ";"}, 1, 5},
		{"second action", []string{"-exec", "a", ";", "-exec", "rsync", "h:/x", ";"}, 4, 6},
		{"outside any action", []string{"rsync", "h:/x", ";"}, 0, 3},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := findExecEnd(c.args, c.k); got != c.want {
				t.Fatalf("findExecEnd(%q, %d) = %d, want %d", c.args, c.k, got, c.want)
			}
		})
	}
}
