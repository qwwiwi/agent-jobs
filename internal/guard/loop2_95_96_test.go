package guard

import "testing"

// Issues #95/#96 fix loop 2: GOFLAGS that Go reads with its quoting rules or that a command sets
// dynamically, by appending or through a builtin for a later go call; sed scripts whose e command
// hides behind an option prefix or a custom regex delimiter; Go test arguments that look like
// assignments.
func TestCheckGoFlagsSedLoop2(t *testing.T) {
	cfg := Config{GitName: "Job Bot", GitEmail: "bot@example.com"}
	cases := []struct {
		name  string
		cmd   string
		block bool
	}{
		// GOFLAGS read with Go's quoted-field rules.
		{"GOFLAGS single-quoted field", `GOFLAGS="'-o=x'" go build ./cmd/agent-jobs && ./x tick`, true},
		{"GOFLAGS double-quoted field", `GOFLAGS='"-o=x"' go build ./cmd/agent-jobs && ./x tick`, true},
		{"GOFLAGS quoted exec field", `GOFLAGS="'-toolexec=./t'" go test ./cmd/agent-jobs`, true},
		{"GOFLAGS backslash", `GOFLAGS='\-o=x' go build ./cmd/agent-jobs`, true},
		// GOFLAGS set for a later go call.
		{"export GOFLAGS dynamic", `F=-o=x; export GOFLAGS="$F"; go build ./cmd/agent-jobs && ./x tick`, true},
		{"export GOFLAGS append", `GOFLAGS=; export GOFLAGS+=-o=x; go build ./cmd/agent-jobs && ./x tick`, true},
		{"GOFLAGS append then export", `GOFLAGS+=-o=x; export GOFLAGS; go build ./cmd/agent-jobs`, true},
		{"set -a GOFLAGS append", `set -a; GOFLAGS+=-o=x; go build ./cmd/agent-jobs`, true},
		{"declare GOFLAGS dynamic", `declare -x GOFLAGS="$F"; go build ./cmd/agent-jobs`, true},
		{"readonly GOFLAGS dynamic", `readonly GOFLAGS="$F"; go build ./cmd/agent-jobs`, true},
		{"typeset GOFLAGS dynamic", `typeset -x GOFLAGS="$F"; go build ./cmd/agent-jobs`, true},
		{"standalone GOFLAGS dynamic", `GOFLAGS="$F"; go build ./cmd/agent-jobs`, true},
		{"GOFLAGS set after go in loop", `for i in 1 2; do go build ./cmd/agent-jobs; export GOFLAGS="$F"; done`, true},
		{"read GOFLAGS", `read GOFLAGS <<< -o=x; export GOFLAGS; go build ./cmd/agent-jobs`, true},
		{"printf -v GOFLAGS", `printf -v GOFLAGS -- -o=x; export GOFLAGS; go build ./cmd/agent-jobs`, true},
		{"export dynamic name", `x=AGS; export "GOFL$x=-o=x"; go build ./cmd/agent-jobs`, true},
		{"go env -w GOFLAGS dynamic", `go env -w GOFLAGS="$F"`, true},
		{"go env -w GOFLAGS quoted", `go env -w GOFLAGS="'-o=x'"`, true},
		{"GOENV file", `GOENV=./e go build ./cmd/agent-jobs && ./x tick`, true},
		{"export GOENV", `export GOENV=./e; go build ./cmd/agent-jobs`, true},
		// Harmless GOFLAGS forms and Go test arguments stay allowed.
		{"GOFLAGS shell-quoted fields", `GOFLAGS='-count=1 -run=X' go test ./cmd/agent-jobs`, false},
		{"export GOFLAGS dynamic no agent", `export GOFLAGS="$F"; go test ./internal/x`, false},
		{"printf dynamic then go test", `printf '%s\n' "$X"; go test ./cmd/agent-jobs`, false},
		{"test args GOFLAGS", `go test ./internal/guard -args GOFLAGS=-o=x`, false},
		{"test args CC", `go test ./internal/guard -args CC=cc`, false},
		// sed scripts.
		{"sed --expression= e", `sed --expression=eclaude f`, true},
		{"sed --expression e", `sed --expression eclaude f`, true},
		{"sed -e attached e", `sed -eeclaude f`, true},
		{"sed -ne attached e", `sed -n -eeclaude cmd/agent-jobs/x.go`, true},
		{"sed custom delimiter address", `sed '\#x#eclaude' f`, true},
		{"sed custom delimiter address flag", `sed '\#x#Ieclaude' f`, true},
		{"sed s inside address", `sed '/s/eclaude' f`, true},
		{"sed s after blank inside address", `sed '/x s/eclaude' f`, true},
		{"sed e after s", `sed 's/x/y/;eclaude' f`, true},
		{"sed e after y", `sed 'y/abc/xyz/;eclaude' f`, true},
		{"sed e on next line", "sed '1{\neclaude\n}' f", true},
		{"sed e after a text", "sed '1a x\neclaude' f", true},
		{"sed e after label", `sed ':a;eclaude' f`, true},
		{"sed s///e on agent file", `sed 's/x/y/e' cmd/agent-jobs/run.sh`, true},
		{"sed e pattern space on agent file", `sed e cmd/agent-jobs/run.sh`, true},
		{"sed -f on agent file", `sed -f prog.sed cmd/agent-jobs/x.go`, true},
		{"sed --file on agent file", `sed --file=prog.sed cmd/agent-jobs/x.go`, true},
		{"sed e in s pattern", `sed 's/eclaude/ok/' f`, false},
		{"sed e in s pattern on agent file", `sed -n 's/eclaude/ok/p' cmd/agent-jobs/x.go`, false},
		{"sed e in address", `sed -n '/refresh/p' cmd/agent-jobs/x.go`, false},
		{"sed range of func", `sed -n '/^func /,/^}/p' cmd/agent-jobs/main.go`, false},
		{"sed e date", `sed -n '1edate' cmd/x.go`, false},
		{"sed -f other file", `sed -f prog.sed internal/x/x.go`, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			r := Check(c.cmd, cfg)
			if c.block && r == "" {
				t.Errorf("%q allowed, want blocked", c.cmd)
			}
			if !c.block && r != "" {
				t.Errorf("%q blocked: %s", c.cmd, r)
			}
		})
	}
}
