package guard

import (
	"strings"
	"testing"
)

func TestCheck(t *testing.T) {
	cfg := Config{OwnerNames: []string{"Dashi"}, GitName: "Job Bot", GitEmail: "bot@example.com"}
	cases := []struct {
		name  string
		cmd   string
		block bool
	}{
		{"plain", "go test ./...", false},
		{"status after pipe", "go test ./... | tail -5; echo $?", true},
		{"status after pipe newline", "make | tee log\nif [ $? -ne 0 ]; then exit 1; fi", true},
		{"status after pipe and", "a | b && echo \"rc=$?\"", true},
		{"status with pipefail", "set -o pipefail; a | b; echo $?", false},
		{"pipestatus", "a | b; echo ${PIPESTATUS[0]}", false},
		{"status no pipe", "go build; echo $?", false},
		{"or is not a pipe", "a || b; echo $?", false},
		{"pipe inside quotes", `grep "a|b" f; echo $?`, false},
		{"pipe inside single quotes", `grep -E 'x|y' f; echo $?`, false},
		{"wait loop pgrep -f", "while pgrep -f 'npm run dev' >/dev/null; do sleep 1; done", true},
		{"until loop pgrep -af", "until ! pgrep -af server; do sleep 2; done", true},
		{"pgrep without loop", "pgrep -f server", false},
		{"loop without pgrep -f", "while kill -0 $pid; do sleep 1; done", false},
		{"owner in commit", `git commit -m "fix by dashi"`, true},
		{"owner in co-author", "git commit -m 'x' -m 'Co-Authored-By: Dashi <d@x>'", true},
		{"commit without owner", `git commit -m "Исправил лимиты"`, false},
		{"owner outside commit", `echo dashi`, false},
		{"foreign -c identity", `git -c user.name="Someone" commit -m x`, true},
		{"own -c identity", `git -c user.email=bot@example.com commit -m x`, false},
		{"foreign config", `git config user.email evil@example.com`, true},
		{"read config ok", `git config user.email`, false},
		{"foreign author", `git commit --author="X <x@y>" -m x`, true},
		{"own author", `git commit --author="Job Bot <bot@example.com>" -m x`, false},
		{"foreign env", `GIT_AUTHOR_EMAIL=x@y git commit -m x`, true},
		{"own env", `GIT_COMMITTER_NAME="Job Bot" git commit -m x`, false},
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

func TestNoIdentityConfiguredRejectsAnyOverride(t *testing.T) {
	if Check(`git config user.name "Anyone"`, Config{}) == "" {
		t.Fatal("override must be rejected when no identity is configured")
	}
}

func TestDecide(t *testing.T) {
	in := `{"tool_name":"Bash","tool_input":{"command":"x | y; echo $?"}}`
	r, err := Decide(strings.NewReader(in), Config{})
	if err != nil || r == "" {
		t.Fatalf("want block, got %q %v", r, err)
	}
	r, err = Decide(strings.NewReader(`{"tool_name":"Read","tool_input":{"file_path":"/x"}}`), Config{})
	if err != nil || r != "" {
		t.Fatalf("non-Bash must pass, got %q %v", r, err)
	}
	if _, err := Decide(strings.NewReader(`not json`), Config{}); err == nil {
		t.Fatal("invalid JSON must error (caller fails closed)")
	}
}
