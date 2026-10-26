package guard

import "testing"

// Issue #95: a path or package whose elements name agent-jobs, an agent, git or a shell is data when
// it is a file/package operand of a program that never runs its operands; running the agent stays
// blocked in every form.
func TestCheckOperandsAreData(t *testing.T) {
	cfg := Config{GitName: "Job Bot", GitEmail: "bot@example.com"}
	cases := []struct {
		name  string
		cmd   string
		block bool
	}{
		// False positives from #95 and their neighbours.
		{"go test pkg dir", `go test ./cmd/agent-jobs/`, false},
		{"go test pkg", `go test -run TestX -count=1 ./cmd/agent-jobs`, false},
		{"go test import path", `go test github.com/qwwiwi/agent-jobs/cmd/agent-jobs`, false},
		{"go build pkg", `go build ./cmd/agent-jobs`, false},
		{"go vet pkg", `go vet ./cmd/agent-jobs/ ./internal/guard/...`, false},
		{"go list pkg", `go list ./cmd/agent-jobs/...`, false},
		{"go test claude pkg", `go test ./internal/claude/`, false},
		{"gofmt dir", `gofmt -l cmd/agent-jobs`, false},
		{"sed file", `sed -n 1,50p cmd/agent-jobs/milestones.go`, false},
		{"sed -e file", `sed -n -e 1,50p cmd/agent-jobs/milestones.go`, false},
		{"sed -ne file", `sed -ne 1,50p cmd/agent-jobs/milestones.go`, false},
		{"sed git dir", `sed -n 1p internal/git/x.go`, false},
		{"diff files", `diff cmd/agent-jobs/a.go cmd/codex/b.go`, false},
		// Running the agent stays blocked.
		{"bin path", `./bin/agent-jobs start`, true},
		{"go run", `go run ./cmd/agent-jobs start`, true},
		{"go run bare", `go run ./cmd/agent-jobs`, true},
		{"go run import path", `go run github.com/qwwiwi/agent-jobs/cmd/agent-jobs@latest list`, true},
		{"go run claude", `go run ./cmd/claude`, true},
		{"bash -c", `bash -c 'agent-jobs list'`, true},
		{"xargs", `echo list | xargs agent-jobs`, true},
		{"env", `env agent-jobs list`, true},
		{"exec", `exec agent-jobs list`, true},
		{"go test -exec", `go test -exec agent-jobs ./...`, true},
		{"go test -exec=", `go test -exec=./bin/claude ./cmd/agent-jobs/`, true},
		{"go build -toolexec", `go build -toolexec codex ./cmd/agent-jobs`, true},
		{"go vet -vettool", `go vet -vettool=/usr/bin/claude ./...`, true},
		{"go build -ldflags extld", `go build -ldflags=all=-extld=claude ./cmd/x`, true},
		{"go build -ldflags pattern", `go build -ldflags all=-extld=claude ./cmd/x`, true},
		// A renamed output of the agent-jobs package could be run under another name.
		{"go build -o", `go build -o bin/x ./cmd/agent-jobs`, true},
		{"go build then run", `go build ./cmd/agent-jobs && ./agent-jobs list`, true},
		{"go generate", `go generate ./cmd/agent-jobs/`, true},
		{"go tool", `go tool claude`, true},
		{"go global flag", `go -C cmd/agent-jobs run .`, true},
		{"sed script runs agent", `sed s/x/agent-jobs/e f`, true},
		{"sed -e script runs agent", `sed -n -e s/x/claude/e f`, true},
		{"sed --expression", `sed --expression=s/x/codex/e f`, true},
		{"sed --expression next word", `sed --expression s/x/codex/e f`, true},
		{"sed -ei attached script", `sed -es/x/claude/e f`, true},
		{"sed posix script before -e", `POSIXLY_CORRECT=1 sed s/x/claude/e -e p f`, true},
		{"sed abbreviated option", `sed --expr=s/x/claude/e f`, true},
		{"sed -- script", `sed -n -- s/x/claude/e f`, true},
		{"sed -f file", `sed -f prog.sed cmd/agent-jobs/x.go`, false},
		{"sed -i suffix", `sed -i.bak -e s/a/b/ cmd/agent-jobs/x.go`, false},
		{"sed unknown option", `sed --unknown cmd/agent-jobs/x.go`, true},
		{"sed operand from variable", `sed -n 1p "$F/agent-jobs"`, true},
		{"sed code operand", `sed -n 1p 'x; claude'`, true},
		{"node agent path", `node /x/claude-code/cli.js`, true},
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
