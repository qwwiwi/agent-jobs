package guard

import "testing"

// Issue #96: go install of an agent package, Go environment assignments that make go run a program
// (GOFLAGS=-toolexec=…, CC=…) and sed's e command attached to an agent name are blocked.
func TestCheckGoInstallEnvSedExec(t *testing.T) {
	cfg := Config{GitName: "Job Bot", GitEmail: "bot@example.com"}
	cases := []struct {
		name  string
		cmd   string
		block bool
	}{
		{"go install agent pkg", `go install ./cmd/agent-jobs`, true},
		{"go install agent import path", `go install github.com/qwwiwi/agent-jobs/cmd/agent-jobs@latest`, true},
		{"go install claude pkg", `go install ./cmd/claude`, true},
		{"go install tool", `go install golang.org/x/tools/cmd/goimports@latest`, false},
		{"GOFLAGS toolexec agent", `GOFLAGS=-toolexec=agent-jobs go test ./...`, true},
		{"GOFLAGS exec agent path", `GOFLAGS=-exec=/usr/bin/claude go test ./...`, true},
		{"GOFLAGS quoted fields", `GOFLAGS='-count=1 -toolexec=codex' go test ./...`, true},
		{"env GOFLAGS", `env GOFLAGS=-toolexec=claude go test ./...`, true},
		{"CC agent", `CC=claude go build ./cmd/x`, true},
		{"GOFLAGS standalone then export", `GOFLAGS=-toolexec=claude; export GOFLAGS; go test ./...`, true},
		{"GOFLAGS set -a", `set -a; GOFLAGS=-toolexec=claude; go test ./...`, true},
		{"GOFLAGS exec drops exemption", `GOFLAGS=-exec=./x go test ./cmd/agent-jobs`, true},
		{"GOFLAGS dynamic drops exemption", `GOFLAGS="$F" go test ./cmd/agent-jobs`, true},
		{"export GOFLAGS -o", `export GOFLAGS=-o=x; go build ./cmd/agent-jobs; ./x tick`, true},
		{"declare GOFLAGS exec", `declare -x GOFLAGS=-toolexec=./t; go build ./cmd/agent-jobs`, true},
		{"go env -w GOFLAGS -o", `go env -w GOFLAGS=-o=x`, true},
		{"go env -w CC agent", `go env -w CC=claude`, true},
		{"export GOFLAGS harmless", `export GOFLAGS=-count=1; go test ./cmd/agent-jobs`, false},
		{"GOFLAGS harmless", `GOFLAGS=-count=1 go test ./cmd/agent-jobs`, false},
		{"GOFLAGS exec no agent", `GOFLAGS=-exec=./x go test ./internal/x`, false},
		{"CGO off", `CGO_ENABLED=0 GOOS=linux go build ./cmd/agent-jobs`, false},
		{"GOCACHE path", `GOCACHE=/tmp/cache go vet ./cmd/agent-jobs/`, false},
		{"sed e no separator", `sed 1eagent-jobs f`, true},
		{"sed e claude", `sed -n 1eclaude f`, true},
		{"sed e last line", `sed '$eclaude list' f`, true},
		{"sed e regex address", `sed '/x/ecodex' f`, true},
		{"sed e after address flag", `sed '/x/Ieclaude' f`, true},
		{"sed e after separator", `sed 'p;eagent-jobs' f`, true},
		{"sed e in -e", `sed -n -e 1eagent-jobs cmd/agent-jobs/x.go`, true},
		{"sed e in --expression", `sed --expression=1eclaude f`, true},
		{"sed e block", `sed '1{eclaude' -e '}' f`, true},
		{"sed e negated", `sed '1!eclaude' f`, true},
		{"sed words with e", `sed -n 's/refresh/legit/p' cmd/agent-jobs/x.go`, false},
		{"sed e other command", `sed 1edate f`, false},
		{"sed delete", `sed -i -e '/^$/d' cmd/agent-jobs/x.go`, false},
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
