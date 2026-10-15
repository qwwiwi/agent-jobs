package guard

import (
	"os"
	"path/filepath"
	"strconv"
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

// Fix-loop 1 regressions (Codex review B2, B7, B8, B9).
func TestCheckFixLoop1(t *testing.T) {
	cfg := Config{OwnerNames: []string{"Dashi"}, GitName: "Job Bot", GitEmail: "bot@example.com"}
	cases := []struct {
		name  string
		cmd   string
		block bool
	}{
		// B2: nothing goes out, whatever the spelling.
		{"git push", "git push origin main", true},
		{"git -C push", "git -C . push", true},
		{"git -c push", "git -c core.x=y push", true},
		{"git quoted push", "git 'push'", true},
		{"git split quotes", `g'it' pu"sh"`, true},
		{"git abs path", "/usr/bin/git push", true},
		{"git after and", "go test ./... && git push", true},
		{"git after newline", "make\ngit push --force", true},
		{"git env prefix", "GIT_TRACE=1 git push", true},
		{"git env wrapper", "env -i GIT_X=1 git push", true},
		{"git in subst", "echo $(git push)", true},
		{"git in backticks", "echo `git push`", true},
		{"git in bash -c", `bash -c "git push"`, true},
		{"git in if", "if git push; then echo ok; fi", true},
		{"git in subshell", "(cd x && git push)", true},
		{"git --git-dir push", "git --git-dir=.git --work-tree . push", true},
		{"git tag", "git tag v1", true},
		{"git alias via -c", "git -c alias.p=push p", true},
		{"git config alias", "git config alias.p push", true},
		{"git status ok", "git status && git log --oneline -3", false},
		{"git commit ok", `git commit -m "Исправил тег"`, false},
		{"word push not git", "echo git push", false},
		{"git push in quoted grep", `grep "git push" README.md`, false},
		{"gh release", "gh release create v1", true},
		{"gh api", "gh api repos/x/y", true},
		{"gh pr create", "gh pr create --fill", true},
		{"gh pr -R create", "gh pr -R owner/repo create", true},
		{"gh pr --repo create", "gh pr --repo=owner/repo create", true},
		{"gh pr new", "gh pr new", true},
		{"gh pr edit", "gh pr edit 3 --title x", true},
		{"gh pr merge", "gh pr merge 3", true},
		{"gh pr review", "gh pr review 3 --approve", true},
		{"gh pr comment", "gh pr comment 3 -b hi", true},
		{"gh issue new", "gh issue new", true},
		{"gh issue comment", "gh -R o/r issue comment 1 -b x", true},
		{"gh quoted", "gh 'pr' \"create\"", true},
		{"gh alias set", "gh alias set pc 'pr create'", true},
		{"gh pr view ok", "gh pr view 3 --json title", false},
		{"gh pr diff ok", "gh pr -R o/r diff 3", false},
		{"redirect amp is not a separator", "git push >/dev/null 2>&1", true},
		{"comment is not a command", "go test ./... # then git push", false},
		{"heredoc body is data", "cat <<EOF\ngit push\nEOF\necho done", false},

		// B7: pipefail must really be in effect.
		{"pipefail word only", "echo pipefail; false | true; echo $?", true},
		{"pipefail disabled", "set -o pipefail; set +o pipefail; a | b; echo $?", true},
		{"pipefail cluster", "set -euo pipefail; a | b; echo $?", false},
		{"pipefail after use", "a | b; echo $?; set -o pipefail", true},
		{"pipestatus word elsewhere", "echo PIPESTATUS; a | b; echo $?", true},
		{"status in single quotes", "a | b; echo '$?'", false},

		// B8: the real commit message and identity, any spelling.
		{"owner split by quotes", `git commit -m 'Da''shi'`, true},
		{"owner in later statement", `git commit -m fix; echo Dashi`, false},
		{"owner in --message=", `git commit --message="thanks dashi"`, true},
		{"owner in -am", `git commit -am "by Dashi"`, true},
		{"owner in -mText", `git commit -m"by Dashi"`, true},
		{"owner in trailer", `git commit -m x --trailer "Co-authored-by: Dashi <d@x>"`, true},
		{"owner in heredoc message", "git commit -F - <<'EOF'\nfix for Dashi\nEOF", true},
		{"owner in -C dir commit", `git -C sub commit -m "dashi"`, true},
		{"foreign -c mixed case", `git -c User.Name=Foreign commit -m x`, true},
		{"foreign -c USER.EMAIL", `git -c USER.EMAIL=x@y commit -m x`, true},
		{"own -c mixed case", `git -c User.Email=bot@example.com commit -m x`, false},
		{"foreign config mixed case", `git config User.Name Foreign`, true},
		{"foreign config --global", `git config --global user.email x@y`, true},
		{"foreign config set", `git config set user.name Foreign`, true},
		{"read config --get", `git config --get user.name`, false},
		{"foreign author.name", `git config author.name Foreign`, true},
		{"foreign export env", `export GIT_COMMITTER_EMAIL=x@y; git commit -m x`, true},
		{"foreign env via env", `env GIT_AUTHOR_NAME=X git commit -m x`, true},
		{"foreign config env", `GIT_CONFIG_COUNT=1 GIT_CONFIG_KEY_0=user.name GIT_CONFIG_VALUE_0=X git commit -m x`, true},
		{"foreign author separate", `git commit --author "X <x@y>" -m x`, true},

		// B9: only a pattern that can match its own loop is blocked.
		{"bracket trick", "while pgrep -f '[s]erver'; do sleep 1; done", false},
		{"self matching", "while pgrep -f 'npm run dev' >/dev/null; do sleep 1; done", true},
		{"self matching long flag", "until ! pgrep --full server; do sleep 2; done", true},
		{"pgrep -f no loop", "pgrep -f server", false},
		{"pgrep without -f in loop", "while pgrep server; do sleep 1; done", false},
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

func TestOwnerInCommitMessageFile(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "msg.txt"), []byte("fix for Dashi\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "ok.txt"), []byte("Исправил лимиты\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg := Config{OwnerNames: []string{"Dashi"}, Dir: dir}
	if Check("git commit -F msg.txt", cfg) == "" {
		t.Fatal("owner name in -F file must block")
	}
	if Check("git commit --file=ok.txt", cfg) != "" {
		t.Fatal("clean -F file must pass")
	}
	if Check("git commit -F missing.txt", cfg) == "" {
		t.Fatal("unreadable message file must block (fail closed)")
	}
}

func TestDecideUsesCwd(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "m"), []byte("dashi"), 0o600); err != nil {
		t.Fatal(err)
	}
	in := `{"tool_name":"Bash","cwd":` + strconv.Quote(dir) + `,"tool_input":{"command":"git commit -F m"}}`
	r, err := Decide(strings.NewReader(in), Config{OwnerNames: []string{"Dashi"}})
	if err != nil || r == "" {
		t.Fatalf("want block via cwd-relative -F, got %q %v", r, err)
	}
}
