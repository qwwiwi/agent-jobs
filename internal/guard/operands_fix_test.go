package guard

import "testing"

// Issue #95 fix loop 1: operand exemptions only hold for a statically known argv. A dynamic or
// glob-expanded word can shift the roles of the words after it, xargs supplies or replaces words, and
// go build of explicit .go files names its output after the first file.
func TestCheckOperandsStaticOnly(t *testing.T) {
	cfg := Config{GitName: "Job Bot", GitEmail: "bot@example.com"}
	cases := []struct {
		name  string
		cmd   string
		block bool
	}{
		{"go test dynamic option", `opt=-exec; go test "$opt" claude ./internal/guard`, true},
		{"go test unset dynamic option", `go test "$opt" claude ./internal/guard`, true},
		{"go test substitution option", `go test "$(printf -- -exec)" claude ./internal/guard`, true},
		{"go test backtick option", "go test \"`printf -- -exec`\" claude ./internal/guard", true},
		{"go test dynamic operand later", `go test -run "$T" ./cmd/agent-jobs`, true},
		{"go test glob before operand", `go test -run Test* ./cmd/agent-jobs`, true},
		{"sed dynamic option", `printf 'x\n' | sed -e p "$q" s/x/claude/e`, true},
		{"sed dynamic option brace", `printf 'x\n' | sed -e p "${q95_opt:0:0}-e" s/x/claude/e`, true},
		{"sed dynamic before file", `sed -n "$q" 1p cmd/agent-jobs/x.go`, true},
		{"diff dynamic option", `diff "$o" cmd/agent-jobs/a.go cmd/agent-jobs/b.go`, true},
		{"xargs replacement", `printf '%s\n' -exec | xargs -I OPT go test OPT claude ./internal/guard`, true},
		{"xargs appended", `printf '%s\n' -exec | xargs go test claude ./internal/guard`, true},
		{"xargs sed", `printf '%s\n' -e | xargs sed -e p s/x/claude/e`, true},
		{"go build go files then run", `go build cmd/agent-jobs/main.go cmd/agent-jobs/milestones.go && ./main tick`, true},
		{"go build go file", `go build cmd/agent-jobs/main.go`, true},
		{"go build flags go file", `go build -v -trimpath ./cmd/agent-jobs/main.go`, true},
		// Static forms stay data (#95).
		{"go test static", `go test -run TestX -count=1 ./cmd/agent-jobs`, false},
		{"go build pkg", `go build ./cmd/agent-jobs`, false},
		{"go vet go file", `go vet cmd/agent-jobs/main.go`, false},
		{"go build other go file", `go build ./internal/x/main.go`, false},
		{"sed static", `sed -n -e 1,50p cmd/agent-jobs/milestones.go`, false},
		{"dynamic word no agent", `go test -run "$T" ./internal/x`, false},
		{"xargs no agent", `echo ./internal/x | xargs go test`, false},
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
