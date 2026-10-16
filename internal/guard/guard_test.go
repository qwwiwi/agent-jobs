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

// Fix-loop 2: fail closed on any form the guard cannot resolve statically (N1–N3, B7, B2/B8 rest).
func TestCheckFixLoop2(t *testing.T) {
	cfg := Config{OwnerNames: []string{"Dashi"}, GitName: "Job Bot", GitEmail: "bot@example.com"}
	cases := []struct {
		name  string
		cmd   string
		block bool
	}{
		// N1: stdin / here-string / heredoc feeding git, identity inside substitutions.
		{"here-string message", "git commit -F - <<< 'Dashi'", true},
		{"here-string clean message", "git commit -F - <<< 'Исправил'", true},
		{"heredoc clean message", "git commit -F - <<'EOF'\nИсправил\nEOF", true},
		{"stdin file into git", "git commit -F - < msg.txt", true},
		{"pipe into commit -F -", "echo x | git commit -F -", true},
		{"commit -F /dev/stdin", "git commit -F /dev/stdin", true},
		{"identity in heredoc substitution", "cat <<EOF\n$(git -c user.name=Foreign commit -m x)\nEOF", true},
		{"quoted heredoc body is data", "cat <<'EOF'\n$(git push)\nEOF", false},
		{"git in substitution", `echo "$(git rev-parse HEAD)"`, true},
		{"gh in backticks", "echo `gh pr create`", true},
		{"git in process substitution", "diff <(git show HEAD:a) a", true},
		{"eval with git", `eval "git status"`, true},
		{"eval dynamic", `eval "$X"`, true},
		{"eval plain", `eval echo hi`, false},
		{"bash -c git", `bash -c "git status"`, true},
		{"bash -c plain", `bash -c "go test ./..."`, false},
		{"bash -c dynamic", `bash -c "$CMD"`, true},
		{"bash heredoc", "bash <<EOF\ngit push\nEOF", true},
		{"pipe into sh", "echo 'git push' | sh", true},
		{"bash script file", "bash scripts/check.sh", false},
		{"env -S", "env -S 'git push'", true},
		{"env --split-string", "env --split-string='git push'", true},
		{"xargs git", "echo push | xargs git", true},
		{"find -exec git", `find . -name x -exec git add {} \;`, true},
		{"alias git", `alias p='git push'; p`, true},
		{"attached -c identity", "git -cuser.name=Foreign commit -m x", true},
		{"attached -C", "git -C. status", true},
		{"config-env", "git --config-env=user.name=N commit -m x", true},
		{"dynamic command name", `G=git; $G push`, true},
		{"dynamic quoted command name", `"$G" status`, true},
		{"dynamic git subcommand", `S=push; git $S`, true},
		{"dynamic gh subcommand", `gh pr "$X"`, true},
		{"upper-case git", "GIT push", true},
		{"dynamic commit message", `git commit -m "$MSG"`, true},
		{"owner via variable", `M=Dashi; git commit -m "$M"`, true},
		{"ansi-c quoted message", `git commit -m $'Da\x73hi'`, true},
		{"static heredoc message", "git commit -m \"$(cat <<'EOF'\nИсправил лимиты\nEOF\n)\"", false},
		{"static heredoc message with owner", "git commit -m \"$(cat <<'EOF'\nfix for Dashi\nEOF\n)\"", true},
		{"dynamic -C dir ok", `git -C "$dir" status`, false},
		{"echo git ok", "echo git push", false},
		{"grep git ok", `grep -rn git .`, false},
		{"which git ok", "which git", false},
		{"trap string", `trap 'git push' EXIT`, true},
		{"python -c", `python3 -c "import os; os.system('git push')"`, true},
		{"watch string", `watch -n1 "gh pr create"`, true},
		{"github url ok", `curl -s https://github.com/qwwiwi/x`, false},
		{"dot git path ok", `rm -f .git/index.lock`, false},

		// N2: wait-loop nested in control structure.
		{"loop after then", "if true; then while pgrep -f server; do sleep 1; done; fi", true},
		{"loop in braces", "{ while pgrep -f server; do sleep 1; done; }", true},
		{"loop in subshell", "(until ! pgrep -f server; do sleep 1; done)", true},
		{"loop after do", "for i in 1 2; do while pgrep -f server; do sleep 1; done; done", true},

		// N3: an escaped `$?` is a literal, not a status read.
		{"escaped status in dquotes", `false | true; echo "\$?"`, false},
		{"escaped status bare", `false | true; echo \$?`, false},
		{"single-quoted status", `false | true; echo '$?'`, false},
		{"double backslash is real read", `false | true; echo \\$?`, true},

		// B7: pipefail counts only as a top-level unconditional simple command.
		{"pipefail inside if", "if x; then set -o pipefail; fi; a | b; echo $?", true},
		{"pipefail in subshell", "(set -o pipefail); a | b; echo $?", true},
		{"pipefail conditional", "false && set -o pipefail; a | b; echo $?", true},
		{"pipefail after or", "x || set -o pipefail; a | b; echo $?", true},
		{"pipefail backgrounded", "set -o pipefail & a | b; echo $?", true},
		{"pipefail -eo", "set -eo pipefail; a | b; echo $?", false},
		{"pipefail newline", "set -o pipefail\na | b\necho $?", false},
		{"pipefail disabled in function", "set -o pipefail; f() { set +o pipefail; }; a | b; echo $?", true},
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

// N6: arguments of an ordinary program are data; only indirect executors have their args scanned.
func TestCheckArgsAreData(t *testing.T) {
	cfg := Config{OwnerNames: []string{"Dashi"}, GitName: "Job Bot", GitEmail: "bot@example.com"}
	cases := []struct {
		name  string
		cmd   string
		block bool
	}{
		{"brew install gh", "brew install gh", false},
		{"brew install git", "brew install git", false},
		{"mkdir git", "mkdir git", false},
		{"go get git path", "go get github.com/x/git", false},
		{"ls git", "ls git", false},
		{"apt-get install git", "apt-get install git", false},
		{"python script with git arg", "python3 tool.py git", false},
		{"xargs git push", "xargs git push", true},
		{"timeout git push", "timeout 5 git push", true},
		{"nohup gh pr create", "nohup gh pr create", true},
		{"python -c git push", `python3 -c "import os; os.system('git push')"`, true},
		{"find -exec git push", `find . -exec git push \;`, true},
		{"find -execdir git", `find . -execdir git push \;`, true},
		{"flock git", "flock /tmp/l git push", true},
		{"parallel gh", "parallel gh pr create ::: a", true},
		{"ssh git", "ssh host git push", true},
		{"perl -e git", `perl -e 'system("git push")'`, true},
		{"node -e gh", `node -e 'require("child_process").execSync("gh pr create")'`, true},
		{"fish -c git", `fish -c 'git push'`, true},
		{"script -c git", `script -q -c 'git push' /dev/null`, true},
		{"watch bash", "watch bash x.sh", true},
		{"stdbuf git", "stdbuf -oL git push", true},
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

// Fix-loop 2, round 2: fail closed again. A program not positively known as data-only that has a
// git/gh word among its arguments is rejected; N6's false positives stay allowed via the allowlist.
func TestCheckFailClosedAllowlist(t *testing.T) {
	cfg := Config{OwnerNames: []string{"Dashi"}, GitName: "Job Bot", GitEmail: "bot@example.com"}
	cases := []struct {
		name  string
		cmd   string
		block bool
	}{
		// Bypasses of the N6 denylist: all must block.
		{"awk system", `awk 'BEGIN{system("git push")}'`, true},
		{"csh -c", `csh -c 'git push'`, true},
		{"tcsh -c", `tcsh -c 'git push'`, true},
		{"abs csh -c", `/bin/csh -c 'git push'`, true},
		{"ksh -c", `ksh -c 'git push'`, true},
		{"dash -c", `dash -c 'git push'`, true},
		{"ash -c", `ash -c 'git push'`, true},
		{"mksh -c", `mksh -c 'git push'`, true},
		{"fish --command", `fish --command 'git push'`, true},
		{"fish --command=", `fish --command='git push'`, true},
		{"vim -c bang", `vim -c ':!git push' -c ':q' f`, true},
		{"vim -es bang gh", `vim -Nu NONE -es -c '!gh pr merge 1 --merge' -c 'qa!'`, true},
		{"ex -c bang", `ex -c '!git push|q' f`, true},
		{"busybox sh -c", `busybox sh -c 'git push'`, true},
		{"busybox git", `busybox git push`, true},
		{"nix-shell --run", `nix-shell --run 'git push'`, true},
		{"nix-shell --run=", `nix-shell --run='git push'`, true},
		{"python -W value then -c", `python3 -W ignore -c 'import os; os.system("git push")'`, true},
		{"python -m runner", `python3 -m tool git push`, true},
		{"python stdin", `python3 - git`, true},
		{"eval --", `eval -- 'git push'`, true},
		{"time -p", `time -p git push`, true},
		{"time -p --", `time -p -- git push`, true},
		{"docker exec", `docker exec c git push`, true},
		{"Rscript -e", `Rscript -e 'system("git push")'`, true},
		{"deno eval", `deno eval 'Deno.run({cmd:["git","push"]})'`, true},
		{"bun -e", `bun -e 'Bun.spawn(["gh","pr","create"])'`, true},
		{"expect -c", `expect -c 'spawn git push'`, true},
		{"pwsh -c", `pwsh -c 'git push'`, true},
		{"gdb -ex shell", `gdb -ex 'shell git push'`, true},
		{"sed e flag", `sed 's/.*/git push/e' f`, true},
		{"unknown runner", `somerunner git push`, true},
		{"rg --pre", `rg --pre 'git push' x .`, true},
		{"man -P", `man -P 'git push' ls`, true},
		{"brew other subcommand", `brew bundle exec git push`, true},
		{"go run is not data", `go run ./x git push`, true},
		{"upper BASH -c", `BASH -c 'git push'`, true},
		{"upper ZSH -c", `ZSH -c 'git push'`, true},
		{"upper SH -c", `SH -c 'git push'`, true},
		{"upper abs BASH -c", `/bin/BASH -c 'git push'`, true},
		{"upper EVAL", `EVAL 'git push'`, true},
		{"upper ENV wrapper", `ENV git push`, true},

		// N6 false positives: data-only programs, must pass.
		{"brew install git", "brew install git", false},
		{"brew info gh", "brew info gh", false},
		{"mkdir gh", "mkdir gh", false},
		{"ls git", "ls git", false},
		{"echo git", "echo git", false},
		{"grep -r gh", "grep -r gh .", false},
		{"rg git", "rg -n git .", false},
		{"cat git file", "cat docs/git.md", false},
		{"cp git", "cp git /tmp/x", false},
		{"rm gh", "rm -rf gh", false},
		{"touch git", "touch git", false},
		{"apt-get install git", "apt-get install git", false},
		{"go get git", "go get github.com/x/git", false},
		{"go install gh", "go install github.com/cli/cli/v2/cmd/gh@latest", false},
		{"pip install git", "pip install gitpython git", false},
		{"npm install gh", "npm install gh", false},
		{"python script file", "python3 tool.py git", false},
		{"python -W script file", "python3 -W ignore tool.py git", false},
		{"python -Wvalue script file", "python3 -Wignore -u tool.py gh", false},
		{"find name git", "find . -name git", false},
		{"man git", "man git", false},
		{"time -p plain", "time -p go test ./...", false},
		{"eval -- plain", "eval -- echo hi", false},
		{"csh plain", `csh -c 'make'`, false},
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
