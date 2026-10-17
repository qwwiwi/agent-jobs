package guard

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

func TestCheck(t *testing.T) {
	cfg := Config{OwnerNames: []string{"Alex"}, GitName: "Job Bot", GitEmail: "bot@example.com"}
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
		{"owner in commit", `git commit -m "fix by alex"`, true},
		{"owner in co-author", "git commit -m 'x' -m 'Co-Authored-By: Alex <d@x>'", true},
		{"commit without owner", `git commit -m "Исправил лимиты"`, false},
		{"owner outside commit", `echo alex`, false},
		{"foreign -c identity", `git -c user.name="Someone" commit -m x`, true},
		{"own -c identity", `git -c user.email=bot@example.com commit -m x`, true}, // identity comes from the wrapper's gitconfig
		{"foreign config", `git config user.email evil@example.com`, true},
		{"read config ok", `git config user.email`, false},
		{"foreign author", `git commit --author="X <x@y>" -m x`, true},
		{"own author", `git commit --author="Job Bot <bot@example.com>" -m x`, false},
		{"foreign env", `GIT_AUTHOR_EMAIL=x@y git commit -m x`, true},
		{"own env", `GIT_COMMITTER_NAME="Job Bot" git commit -m x`, true}, // no GIT_* assignment next to git
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
	cfg := Config{OwnerNames: []string{"Alex"}, GitName: "Job Bot", GitEmail: "bot@example.com"}
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
		{"owner split by quotes", `git commit -m 'Al''ex'`, true},
		{"owner in later statement", `git commit -m fix; echo Alex`, false},
		{"owner in --message=", `git commit --message="thanks alex"`, true},
		{"owner in -am", `git commit -am "by Alex"`, true},
		{"owner in -mText", `git commit -m"by Alex"`, true},
		{"owner in trailer", `git commit -m x --trailer "Co-authored-by: Alex <d@x>"`, true},
		{"owner in heredoc message", "git commit -F - <<'EOF'\nfix for Alex\nEOF", true},
		{"owner in -C dir commit", `git -C sub commit -m "alex"`, true},
		{"foreign -c mixed case", `git -c User.Name=Foreign commit -m x`, true},
		{"foreign -c USER.EMAIL", `git -c USER.EMAIL=x@y commit -m x`, true},
		{"own -c mixed case", `git -c User.Email=bot@example.com commit -m x`, true},
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
	if err := os.WriteFile(filepath.Join(dir, "msg.txt"), []byte("fix for Alex\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "ok.txt"), []byte("Исправил лимиты\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg := Config{OwnerNames: []string{"Alex"}, Dir: dir}
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
	if err := os.WriteFile(filepath.Join(dir, "m"), []byte("alex"), 0o600); err != nil {
		t.Fatal(err)
	}
	in := `{"tool_name":"Bash","cwd":` + strconv.Quote(dir) + `,"tool_input":{"command":"git commit -F m"}}`
	r, err := Decide(strings.NewReader(in), Config{OwnerNames: []string{"Alex"}})
	if err != nil || r == "" {
		t.Fatalf("want block via cwd-relative -F, got %q %v", r, err)
	}
}

// Fix-loop 2: fail closed on any form the guard cannot resolve statically (N1–N3, B7, B2/B8 rest).
func TestCheckFixLoop2(t *testing.T) {
	cfg := Config{OwnerNames: []string{"Alex"}, GitName: "Job Bot", GitEmail: "bot@example.com"}
	cases := []struct {
		name  string
		cmd   string
		block bool
	}{
		// N1: stdin / here-string / heredoc feeding git, identity inside substitutions.
		{"here-string message", "git commit -F - <<< 'Alex'", true},
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
		{"owner via variable", `M=Alex; git commit -m "$M"`, true},
		{"ansi-c quoted message", `git commit -m $'Al\x65x'`, true},
		{"static heredoc message", "git commit -m \"$(cat <<'EOF'\nИсправил лимиты\nEOF\n)\"", false},
		{"static heredoc message with owner", "git commit -m \"$(cat <<'EOF'\nfix for Alex\nEOF\n)\"", true},
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
	cfg := Config{OwnerNames: []string{"Alex"}, GitName: "Job Bot", GitEmail: "bot@example.com"}
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
	cfg := Config{OwnerNames: []string{"Alex"}, GitName: "Job Bot", GitEmail: "bot@example.com"}
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
		{"cp git", "cp git /tmp/x", true}, // a file named git copied elsewhere is a renamed git
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

// B1–B2 (conformance walk): no comments in any gh form; no commit message reused from another commit.
func TestCheckCommentsAndReusedMessages(t *testing.T) {
	cfg := Config{OwnerNames: []string{"Alex"}, GitName: "Job Bot", GitEmail: "bot@example.com"}
	cases := []struct {
		name  string
		cmd   string
		block bool
	}{
		{"gh pr close --comment", `gh pr close 123 --comment 'x'`, true},
		{"gh pr close -c", `gh pr close 123 -c x`, true},
		{"gh pr close plain", `gh pr close 123`, true},
		{"gh issue close --comment", `gh issue close 5 --comment x`, true},
		{"gh issue reopen -c", `gh issue reopen 5 -c x`, true},
		{"gh pr reopen --comment", `gh pr reopen 5 --comment x`, true},
		{"gh pr merge --body", `gh pr merge 5 --body x`, true},
		{"gh pr ready", `gh pr ready 5`, true},
		{"gh issue develop", `gh issue develop 5`, true},
		{"gh gist create", `gh gist create f.txt`, true},
		{"gh repo edit", `gh repo edit --description x`, true},
		{"gh label create", `gh label create bug`, true},
		{"gh workflow run", `gh workflow run ci.yml`, true},
		{"gh auth token", `gh auth token`, true},
		{"gh secret set", `gh secret set X`, true},
		{"gh unknown future", `gh newthing do`, true},
		{"gh -R close", `gh -R o/r issue close 1 --comment x`, true},
		{"gh pr view ok", `gh pr view 5 --comments`, false},
		{"gh pr list ok", `gh pr list --state open`, false},
		{"gh issue view ok", `gh issue view 5`, false},
		{"gh pr checks ok", `gh pr checks 5`, false},
		{"gh search ok", `gh search issues guard`, false},
		{"gh auth status (token printing forms)", `gh auth status`, true},
		{"gh version ok", `gh --version`, false},

		{"commit -C HEAD", `git commit -C HEAD`, true},
		{"commit -c HEAD", `git commit -c HEAD`, true},
		{"commit -aC", `git commit -aC HEAD`, true},
		{"commit -CHEAD", `git commit -CHEAD`, true},
		{"commit --reuse-message=", `git commit --reuse-message=HEAD`, true},
		{"commit --reuse-message sep", `git commit --reuse-message HEAD`, true},
		{"commit --reedit-message=", `git commit --reedit-message=HEAD~1`, true},
		{"commit --fixup=amend:", `git commit --fixup=amend:HEAD`, true},
		{"commit --fixup=reword:", `git commit --fixup=reword:HEAD`, true},
		{"commit --fixup plain", `git commit --fixup HEAD`, true},
		{"commit --squash", `git commit --squash=HEAD`, true},
		{"commit --squash sep", `git commit --squash HEAD`, true},
		{"commit abbreviated --reuse", `git commit --reuse=HEAD`, true},
		{"commit abbreviated --mess owner", `git commit --mess="by Alex"`, true},
		{"commit abbreviated --mess clean", `git commit --mess="Исправил"`, true},
		{"commit -m with C inside value", `git commit -mCleanup`, false},
		{"commit -am ok", `git commit -am "Исправил"`, false},
		{"commit --amend --no-edit ok", `git commit --amend --no-edit`, false},
		{"commit --allow-empty ok", `git commit --allow-empty -m x`, false},
		{"git -C dir commit ok", `git -C sub commit -m x`, false},
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

func TestCommitTemplateFileIsChecked(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "tpl"), []byte("fix for Alex\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg := Config{OwnerNames: []string{"Alex"}, Dir: dir}
	for _, cmd := range []string{"git commit -t tpl", "git commit --template=tpl", "git commit --template tpl"} {
		if Check(cmd, cfg) == "" {
			t.Fatalf("%s: owner name in the template must block", cmd)
		}
	}
}

// Security review (Fable) H1–H3: direct spellings of outward actions, git running programs from
// config/env, nested agents.
func TestCheckSecurityReview(t *testing.T) {
	cfg := Config{OwnerNames: []string{"Alex"}, GitName: "Job Bot", GitEmail: "bot@example.com"}
	cases := []struct {
		name  string
		cmd   string
		block bool
	}{
		// H1: glob/brace in the command name or git words.
		{"brace git push", "{git,push} origin HEAD", true},
		{"glob git path", "/usr/bin/g[i]t push", true},
		{"glob ? git", "/usr/bin/gi? push", true},
		{"glob * git", "/usr/bin/gi* push", true},
		{"brace in env", "env {git,push} origin", true},
		{"brace subcommand", "git {push,} origin", true},
		{"glob subcommand", "git pu?h origin", true},
		{"brace in -C value", "git -C {x,push}", true},
		{"glob gh subcommand", "gh pr cr*ate", true},
		{"test bracket ok", "[ -f go.mod ] && echo ok", false},
		{"double bracket ok", "[[ -f go.mod ]] && echo ok", false},
		{"glob after subcommand ok", "git add '*.go'", false},
		{"glob in ls ok", "ls internal/*.go", false},
		// H1: git-<sub> binaries.
		{"git-push binary", "/usr/lib/git-core/git-push origin HEAD", true},
		{"git-push bare", "git-push origin", true},
		{"git-tag binary", "/usr/lib/git-core/git-tag v1", true},
		{"git-send-pack", "/usr/lib/git-core/git-send-pack remote HEAD", true},
		{"git-http-push", "git-http-push https://x/y HEAD", true},
		{"git-commit owner", `/usr/lib/git-core/git-commit -m "for Alex"`, true},
		{"git-commit ok", `/usr/lib/git-core/git-commit -m "Исправил"`, false},
		{"git-status ok", "/usr/lib/git-core/git-status", false},
		{"find -exec git-push", `find . -exec /usr/lib/git-core/git-push \;`, true},
		{"git-push in substitution", "echo $(git-push)", true},
		// H1: send-pack / http-push subcommands.
		{"git send-pack", "git send-pack origin HEAD", true},
		{"git http-push", "git http-push https://x/y HEAD", true},
		{"git -C send-pack", "git -C . send-pack x HEAD", true},
		// H1: copying git/gh/a shell makes a renamed executable.
		{"cp git binary", "cp /usr/bin/git ./x", true},
		{"cp git-push", "cp /usr/lib/git-core/git-push ./x", true},
		{"mv gh", "mv /usr/bin/gh ./x", true},
		{"install git", "install -m 755 /usr/bin/git ./x", true},
		{"ln -s git", "ln -s /usr/bin/git ./x", true},
		{"ln git hard", "ln /usr/bin/git ./x", true},
		{"cp bash", "cp /bin/bash ./x", true},
		{"cp claude", "cp $(which claude) ./x", true},
		{"cp which git", "cp `which git` ./x", true},
		{"cp claude path", "cp /usr/local/bin/claude ./x", true},
		{"cp dynamic ok", `cp "$f" /tmp/x`, false},
		{"cp source file ok", "cp internal/git.go /tmp/x", false},
		{"cp gitignore ok", "cp .gitignore /tmp/x", false},
		{"mv ok", "mv a.txt b.txt", false},
		{"ln -s ok", "ln -s ../data data", false},

		// H2: git env assignments next to git.
		{"GIT_DIR prefix", "GIT_DIR=/tmp/x git status", true},
		{"GIT_EXEC_PATH prefix", "GIT_EXEC_PATH=/tmp/x git status", true},
		{"GIT_SSH_COMMAND", "GIT_SSH_COMMAND='sh -c x' git fetch", true},
		{"GIT_PAGER", "GIT_PAGER='sh x' git log", true},
		{"PAGER", "PAGER=less git log", true},
		{"EDITOR", "EDITOR=vim git commit", true},
		{"VISUAL", "VISUAL=vim git commit", true},
		{"GIT_CONFIG_GLOBAL", "GIT_CONFIG_GLOBAL=$HOME/.gitconfig git log", true},
		{"export GIT_CONFIG_GLOBAL", "export GIT_CONFIG_GLOBAL=~/.gitconfig; git log", true},
		{"export -n", "export -n GIT_CONFIG_NOSYSTEM; git log", true},
		{"declare", "declare -x PAGER=less; git log", true},
		{"printf -v", "printf -v GIT_PAGER %s less; git log", true},
		{"default expansion", ": ${GIT_PAGER:=less}; git log", true},
		{"env assign", "env GIT_CONFIG_COUNT=0 git status", true},
		{"append assign", "GIT_CONFIG_COUNT+=1 git status", true},
		{"git-commit with EDITOR", "EDITOR=vi /usr/lib/git-core/git-commit", true},
		{"git env in bash -c", `bash -c 'GIT_DIR=x git status'`, true},
		{"sudo git", "sudo git status", true},
		{"PAGER without git ok", "PAGER=cat man ls", false},
		{"GIT var without git ok", "GIT_TRACE=1 go test ./...", false},
		// H2: emptied or unset environment, with or without git.
		{"env -i", "env -i git status", true},
		{"env -i no git", "env -i ./run.sh", true},
		{"env - ", "env - ./run.sh", true},
		{"env --ignore-environment", "env --ignore-environment ./run.sh", true},
		{"env -iv", "env -iv ./run.sh", true},
		{"env -u GIT", "env -u GIT_CONFIG_GLOBAL ./run.sh", true},
		{"env -uGIT", "env -uGIT_CONFIG_NOSYSTEM ./run.sh", true},
		{"env --unset=", "env --unset=GIT_CONFIG_COUNT ./run.sh", true},
		{"env --unset sep", "env --unset PAGER ./run.sh", true},
		{"env -u other ok", "env -u LANG ./run.sh", false},
		{"unset GIT", "unset GIT_CONFIG_GLOBAL; ./run.sh", true},
		{"unset -v EDITOR", "unset -v EDITOR", true},
		{"unset other ok", "unset FOO", false},
		{"exec -c", "exec -c ./run.sh", true},
		// H2: git config / -c allowlist.
		{"-c core.hooksPath", "git -c core.hooksPath=/tmp/h commit -m x", true},
		{"-c core.pager", "git -c core.pager='sh x' log", true},
		{"-c core.sshCommand", "git -c core.sshCommand='sh x' fetch", true},
		{"-c credential.helper", "git -c credential.helper='!sh x' fetch", true},
		{"-c diff.external", "git -c diff.external=/tmp/x diff", true},
		{"-c core.fsmonitor", "git -c core.fsmonitor=/tmp/x status", true},
		{"-c alias", "git -c alias.x='!sh y' x", true},
		{"-c include.path", "git -c include.path=/tmp/c log", true},
		{"-c bare key", "git -c core.hooksPath log", true},
		{"-c gpgsign ok", "git -c commit.gpgsign=false commit -m x", false},
		{"-c color ok", "git -c color.ui=never log", false},
		{"config core.hooksPath", "git config core.hooksPath /tmp/h", true},
		{"config --local hooksPath", "git config --local core.hooksPath /tmp/h", true},
		{"config --file hooksPath", "git config --file .git/config core.hooksPath /tmp/h", true},
		{"config set pager", "git config set core.pager 'sh x'", true},
		{"config --add credential", "git config --add credential.helper '!sh x'", true},
		{"config diff.x.textconv", "git config diff.x.textconv /tmp/x", true},
		{"config filter", "git config filter.x.clean /tmp/x", true},
		{"config url insteadOf", "git config url.https://evil/.insteadOf https://github.com/", true},
		{"config rename-section", "git config --rename-section x core", true},
		{"config gpgsign ok", "git config commit.gpgsign false", false},
		{"config own identity ok", `git config user.email bot@example.com`, false},
		{"config foreign identity", `git config user.email evil@example.com`, true},
		{"config read hooksPath ok", "git config --get core.hooksPath", false},
		{"config list ok", "git config --list", false},

		// H3: nested agents.
		{"claude", `claude -p "do it"`, true},
		{"claude path", `/usr/local/bin/claude -p x`, true},
		{"claude upper", `CLAUDE -p x`, true},
		{"env claude", `env FOO=1 claude -p x`, true},
		{"command claude", `command claude -p x`, true},
		{"exec claude", `exec claude -p x`, true},
		{"nohup claude", `nohup claude -p x &`, true},
		{"codex", `codex exec "fix"`, true},
		{"gemini", `gemini -p x`, true},
		{"aider", `aider --yes`, true},
		{"cursor-agent", `cursor-agent -p x`, true},
		{"claude in bash -c", `bash -c 'claude -p x'`, true},
		{"claude in substitution", `echo $(claude -p x)`, true},
		{"npx claude", `npx claude -p x`, true},
		{"timeout codex", `timeout 60 codex exec x`, true},
		{"find -exec claude", `find . -exec claude -p {} \;`, true},
		{"which claude ok", `which claude`, false},
		{"grep claude ok", `grep -rn claude docs`, false},
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

// Automated security review of the fix-loop guard commit: env abbreviations, glob/brace in wrapper
// words, incomplete git denylist (unknown subcommands, command-running forms), writes into git files.
// Push review: gh flags before the subcommand fail closed; gh auth status (token printing) is gone.
func TestGhFlagsBeforeSubcommand(t *testing.T) {
	cfg := Config{GitName: "Job Bot", GitEmail: "bot@example.com"}
	for _, c := range []struct {
		cmd     string
		block   bool
		reasons string // substring the block reason must contain ("" = any)
	}{
		{"gh --hostname github.com pr create", true, ""},
		{"gh -X pr view create", true, "gh flag -X"},
		{"gh --foo pr view", true, "gh flag --foo"},
		{"gh pr --web view", true, "gh flag --web"},
		{"gh pr --jq x create", true, "gh flag --jq"},
		{"gh --help pr", true, "gh flag --help"},
		{"gh auth status", true, ""},
		{"gh auth status -t", true, ""},
		{"gh auth status --show-token", true, ""},
		{"gh -R o/r pr view 1", false, ""},
		{"gh --repo o/r pr view 1", false, ""},
		{"gh --repo=o/r pr list", false, ""},
		{"gh -Ro/r pr list", false, ""},
		{"gh pr -R o/r view 1", false, ""},
		{"gh --hostname=h.example pr list", false, ""},
		{"gh pr view 1 --web --json title", false, ""},
		{"gh pr list --state open", false, ""},
	} {
		got := Check(c.cmd, cfg)
		if (got != "") != c.block || !strings.Contains(got, c.reasons) {
			t.Errorf("Check(%q) = %q, want block=%v containing %q", c.cmd, got, c.block, c.reasons)
		}
	}
}

// Push review: Write/Edit/MultiEdit/NotebookEdit never write inside a .git directory, also through a
// relative path, `..`, or a symlink in the job dir that points into .git.
func TestFileToolsGuarded(t *testing.T) {
	repo := t.TempDir()
	if err := os.MkdirAll(filepath.Join(repo, ".git", "hooks"), 0o755); err != nil {
		t.Fatal(err)
	}
	work := t.TempDir()
	if err := os.Symlink(filepath.Join(repo, ".git"), filepath.Join(work, "g")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(repo, ".git", "config"), filepath.Join(work, "cfg")); err != nil {
		t.Fatal(err)
	}
	// hl -> .git/hooks: `hl/../config` is opened by the kernel as .git/config, though Clean says work/config.
	if err := os.Symlink(filepath.Join(repo, ".git", "hooks"), filepath.Join(work, "hl")); err != nil {
		t.Fatal(err)
	}
	// A real git dir not named .git (a separate git dir / worktree admin dir), told to the guard.
	sep := filepath.Join(t.TempDir(), "store")
	if err := os.MkdirAll(filepath.Join(sep, "hooks"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(sep, filepath.Join(work, "s")); err != nil {
		t.Fatal(err)
	}
	decide := func(tool, key, path string) string {
		in, _ := json.Marshal(map[string]any{"tool_name": tool, "cwd": work, "tool_input": map[string]string{key: path}})
		r, err := Decide(strings.NewReader(string(in)), Config{GitDirs: []string{sep}})
		if err != nil {
			t.Fatal(err)
		}
		return r
	}
	for _, c := range []struct {
		tool, key, path string
		block           bool
	}{
		{"Write", "file_path", filepath.Join(repo, ".git", "config"), true},
		{"Edit", "file_path", filepath.Join(repo, ".git", "hooks", "pre-commit"), true},
		{"MultiEdit", "file_path", filepath.Join(repo, ".git", "info", "attributes"), true},
		{"NotebookEdit", "notebook_path", filepath.Join(repo, ".git", "x.ipynb"), true},
		{"Write", "file_path", filepath.Join(repo, ".GIT", "config"), true},
		{"Write", "file_path", repo + "/sub/../.git/config", true}, // literal: Join would drop sub/..
		{"Write", "file_path", work + "/hl/../config", true},       // symlink then .. = .git/config
		{"Write", "file_path", "hl/../config", true},               // same, relative
		{"Write", "file_path", "notes/../x.md", true},              // any .. is rejected
		{"Write", "file_path", filepath.Join(sep, "config"), true}, // job git dir not named .git
		{"Edit", "file_path", filepath.Join(sep, "hooks", "pre-commit"), true},
		{"Write", "file_path", filepath.Join(work, "s", "config"), true}, // symlink into that git dir
		{"Write", "file_path", sep, true},
		{"Write", "file_path", filepath.Join(filepath.Dir(sep), "other"), false}, // next to it is fine
		{"Write", "file_path", ".git/config", true},
		{"Write", "file_path", filepath.Join(repo, ".git"), true},
		{"Write", "file_path", "g/hooks/pre-commit", true},                         // symlinked dir into .git
		{"Write", "file_path", filepath.Join(work, "g", "new", "deep", "f"), true}, // non-existing tail
		{"Edit", "file_path", filepath.Join(work, "cfg"), true},                    // symlinked file into .git
		{"Write", "file_path", filepath.Join(repo, "main.go"), false},
		{"Edit", "file_path", "notes/report.md", false},
		{"Write", "file_path", filepath.Join(repo, ".github", "x.yml"), false},
		{"NotebookEdit", "notebook_path", filepath.Join(repo, "a.ipynb"), false},
		{"Read", "file_path", filepath.Join(repo, ".git", "config"), false}, // reading is fine
	} {
		if got := decide(c.tool, c.key, c.path); (got != "") != c.block {
			t.Errorf("%s %s: %q, want block=%v", c.tool, c.path, got, c.block)
		}
	}
}

// Every subcommand denied by name is blocked by its own check (not only by the git-command allowlist),
// so its message names the real reason and dropping it from its deny list fails here.
func TestGitDeniedByName(t *testing.T) {
	cfg := Config{GitName: "Job Bot", GitEmail: "bot@example.com"}
	outsideMsg, runsMsg := "reaches outside the job", "runs commands or git subcommands"
	for _, c := range []struct{ sub, want string }{
		{"push", outsideMsg}, {"tag", outsideMsg}, {"send-pack", outsideMsg}, {"http-push", outsideMsg},
		{"send-email", outsideMsg}, {"imap-send", outsideMsg}, {"svn", outsideMsg}, {"p4", outsideMsg},
		{"daemon", outsideMsg}, {"request-pull", outsideMsg}, {"instaweb", outsideMsg}, {"web--browse", outsideMsg},
		{"cvsexportcommit", outsideMsg}, {"cvsimport", outsideMsg}, {"cvsserver", outsideMsg},
		{"archimport", outsideMsg}, {"http-backend", outsideMsg}, {"upload-pack", outsideMsg},
		{"receive-pack", outsideMsg}, {"upload-archive", outsideMsg}, {"shell", outsideMsg},
		{"filter-branch", runsMsg}, {"filter-repo", runsMsg}, {"difftool", runsMsg}, {"difftool--helper", runsMsg},
		{"mergetool", runsMsg}, {"for-each-repo", runsMsg}, {"maintenance", runsMsg}, {"hook", runsMsg},
		{"remote-ext", runsMsg}, {"remote-fd", runsMsg}, {"credential-cache--daemon", runsMsg},
		{"fsmonitor--daemon", runsMsg},
	} {
		if !gitKnown[c.sub] {
			t.Errorf("%s missing from gitKnown: its deny list would be hidden by the allowlist", c.sub)
		}
		if got := Check("git "+c.sub+" x", cfg); !strings.Contains(got, c.want) || !strings.Contains(got, "`git "+c.sub+"`") {
			t.Errorf("git %s: %q, want %q naming it", c.sub, got, c.want)
		}
	}
}

// Fable control round of PR #1 (fix loop 2): agents and shells inside interpreter code and package
// paths, clone/init --template, grep -O, `.git` respelled or as a directory target, merge strategies,
// fetch-pack/ls-remote --exec, clone -c values.
func TestCheckSecurityReview3(t *testing.T) {
	cfg := Config{OwnerNames: []string{"Alex"}, GitName: "Job Bot", GitEmail: "bot@example.com"}
	cases := []struct {
		name  string
		cmd   string
		block bool
	}{
		// 2: agents and shells as words inside code strings and paths.
		{"python -c subprocess claude", `python3 -c 'import subprocess; subprocess.run(["claude", "-p", "x"])'`, true},
		{"node -e codex", `node -e 'require("child_process").execSync("codex exec x")'`, true},
		{"npx claude-code", "npx @anthropic-ai/claude-code -p x", true},
		{"node claude-code cli", "node /usr/lib/node_modules/@anthropic-ai/claude-code/cli.js -p x", true},
		{"claude-code command", "claude-code -p x", true},
		{"python -c bash", `python3 -c 'import os; os.system("bash x")'`, true},
		{"perl -e sh", `perl -e 'system("sh", "-c", "x")'`, true},
		{"ruby -e zsh", `ruby -e 'exec "zsh"'`, true},
		{"node -e gemini", `node -e 'spawn("gemini")'`, true},
		{"python -c aider", `python3 -c 'run("aider --yes")'`, true},
		{"python -c cursor-agent", `python3 -c 'run("cursor-agent")'`, true},
		{"python -c dash fish", `python3 -c 'run(["dash"]); run(["fish"])'`, true},
		{"python -c ok", `python3 -c 'print(1+1)'`, false},
		// Agent names with a -suffix and package names.
		{"gemini-cli command", "gemini-cli -p x", true},
		{"aider-chat command", "aider-chat --yes", true},
		{"codex-cli path", "/usr/local/bin/codex-cli exec x", true},
		{"node google gemini-cli", "node /usr/lib/node_modules/@google/gemini-cli/dist/index.js", true},
		{"node openai codex", "node /usr/lib/node_modules/@openai/codex/bin/codex.js", true},
		{"python -c aider-chat", `python3 -c 'run("aider-chat")'`, true},
		{"find -exec gemini-cli", "find . -exec gemini-cli {} ;", true},
		{"claudette is not claude ok", "go run ./claudette", false},
		// Package runners.
		{"npx", "npx prettier --write x.ts", true},
		{"pnpx", "pnpx eslint .", true},
		{"bunx", "bunx tsc", true},
		{"uvx", "uvx ruff check", true},
		{"pnpm dlx", "pnpm dlx create-vite", true},
		{"yarn dlx", "yarn dlx @google/gemini-cli", true},
		{"pipx run", "pipx run black .", true},
		{"uv tool run", "uv tool run ruff", true},
		{"npm exec", "npm exec -- prettier x", true},
		{"npm x", "npm x prettier", true},
		{"npm --prefix exec", "npm --prefix d exec prettier", true},
		{"bun x", "bun x tsc", true},
		{"timeout npx", "timeout 60 npx tsc", true},
		{"find -exec npx", "find . -exec npx tsc {} ;", true},
		{"npm install ok", "npm install", false},
		{"npm test ok", "npm test", false},
		{"pnpm install ok", "pnpm install", false},
		{"uv sync ok", "uv sync", false},
		{"pipx list ok", "pipx list", false},
		{"script.sh arg ok", "chmod +x scripts/check.sh", false},
		{"node script ok", "node build.js --prod", false},
		{"go test ok", "go test -run TestBash ./...", false},
		// 3: template directories.
		{"clone --template", "git clone --template=/tmp/t https://x/y", true},
		{"clone --templ abbrev", "git clone --templ /tmp/t https://x/y", true},
		{"init --template", "git init --template /tmp/t", true},
		{"init --te abbrev", "git init --te=/tmp/t", true},
		{"init ok", "git init", false},
		{"clone ok", "git clone --depth 1 https://x/y", false},
		// 4: grep -O.
		{"grep -O", "git grep -O foo", true},
		{"grep -Ovim", "git grep -Ovim foo", true},
		{"grep -nO cluster", "git grep -nO foo", true},
		{"grep --open-files-in-pager", "git grep --open-files-in-pager=vim foo", true},
		{"grep --open abbrev", "git grep --open foo", true},
		{"grep ok", "git grep -n foo", false},
		// 5: `.git` respelled, as a directory target, through a glob.
		{"redirect .git//config", "echo x > .git//config", true},
		{"redirect .git/./config", "echo x >> .git/./config", true},
		{"redirect via ..", "echo x > sub/../.git/hooks/pre-commit", true},
		{"cd .git", "cd .git", true},
		{"cd abs .git", "cd /repo/.git", true},
		{"cd .git/", "cd .git/", true},
		{"ln -s .git", "ln -s ../repo/.git d", true},
		{"cp into .git", "cp hooks -r .git/", true},
		{"cp -t .git", "cp --target-directory=.git config", true},
		{"mv into .git", "mv evil .git", true},
		{"tee .git//config", "echo x | tee .git//config", true},
		{"redirect to .git", "echo x > .git", true},
		{"cp glob .gi?", "cp evil .gi?/config", true},
		{"cp glob .[g]it", "cp evil .[g]it/hooks/pre-commit", true},
		{"cp glob mentions .git", "cp evil .git/hook*", true},
		{"redirect glob", "echo x > .g*/config", true},
		{"ls .git ok", "ls .git", false},
		{"cp glob", "cp src/* dst/", true},
		// Any glob/brace in a write position fails closed (dotglob, braces, `*` can reach .git).
		{"cp brace alternative", "cp evil {.g,x}it/config", true},
		{"cp star segment", "cp evil */config", true},
		{"tar -C glob", "tar -xf evil.tar -C .g*", true},
		{"redirect brace", "echo x > {a,.git/config}", true},
		{"shopt dotglob", "shopt -s dotglob", true},
		{"setopt globdots", "setopt globdots", true},
		{"setopt GLOB_DOTS", "setopt GLOB_DOTS", true},
		{"set -o globdots", "set -o globdots", true},
		{"GLOBIGNORE", "GLOBIGNORE=x; cp evil * dir", true},
		{"export GLOBIGNORE", "export GLOBIGNORE=.", true},
		{"ls glob ok", "ls *.go", false},
		{"grep glob ok", "grep -n foo *.go", false},
		{"find -name glob ok", "find . -name '*.go'", false},
		{"pgrep bracket ok", "pgrep -f '[s]erver'", false},
		{"shopt other ok", "shopt -s nullglob", false},
		{"find filter ok", "find . -type f -not -path '*/.git/*'", false},
		{"find exec glob", "find . -path '*/.git/*' -exec rm {} +", true},
		{"cd elsewhere ok", "cd internal/guard", false},
		// 6: merge strategies.
		{"merge -s evil", "git merge -s evil main", true},
		{"merge -sevil", "git merge -sevil main", true},
		{"merge -ns cluster", "git merge -ns evil main", true},
		{"merge --strategy=evil", "git merge --strategy=evil main", true},
		{"merge --strat abbrev", "git merge --strat evil main", true},
		{"pull -s evil", "git pull -s evil origin main", true},
		{"rebase --strategy evil", "git rebase --strategy evil main", true},
		{"cherry-pick --strategy evil", "git cherry-pick --strategy=evil abc", true},
		{"revert --strategy evil", "git revert --strategy evil abc", true},
		{"merge -s ort ok", "git merge -s ort main", false},
		{"merge --strategy=ours ok", "git merge --strategy=ours main", false},
		{"merge -X ok", "git merge -X theirs main", false},
		{"merge --strategy-option ok", "git merge --strategy-option=theirs main", false},
		{"cherry-pick -s signoff ok", "git cherry-pick -s abc", false},
		{"revert -s signoff ok", "git revert -s abc", false},
		// 7: --exec is --upload-pack.
		{"fetch-pack --exec", "git fetch-pack --exec='sh x' origin", true},
		{"ls-remote --exec", "git ls-remote --exec 'sh x' origin", true},
		{"ls-remote --ex abbrev", "git ls-remote --ex='sh x' origin", true},
		{"fetch --exec", "git fetch --exec='sh x' origin", true},
		{"ls-remote ok", "git ls-remote origin", false},
		// 8: clone -c values go through the config allowlist.
		{"clone -c hooksPath", "git clone -c core.hooksPath=/tmp/h https://x/y", true},
		{"clone --config fsmonitor", "git clone --config core.fsmonitor=/tmp/x https://x/y", true},
		{"clone --conf= abbrev", "git clone --conf=core.sshCommand=x https://x/y", true},
		{"clone -ckv attached", "git clone -ccore.pager=x https://x/y", true},
		{"clone -qc cluster", "git clone -qc alias.st=!sh https://x/y", true},
		{"clone -c foreign identity", "git clone -c user.name=Other https://x/y", true},
		{"clone -c own identity ok", "git clone -c 'user.name=Job Bot' https://x/y", false},
		{"clone -c safe ok", "git clone -c core.autocrlf=false https://x/y", false},
		{"clone -b ok", "git clone -b cfg https://x/y", false},
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

func TestCheckSecurityReview2(t *testing.T) {
	cfg := Config{OwnerNames: []string{"Alex"}, GitName: "Job Bot", GitEmail: "bot@example.com"}
	cases := []struct {
		name  string
		cmd   string
		block bool
	}{
		// 1: env long options: GNU getopt abbreviations, two-word --unset; unknown long options fail closed.
		{"env --ign", "env --ign ./run.sh", true},
		{"env --ignore-env", "env --ignore-env git status", true},
		{"env --unse=", "env --unse=GIT_CONFIG_GLOBAL ./run.sh", true},
		{"env --uns sep", "env --uns GIT_DIR ./run.sh", true},
		{"env --unset sep GIT", "env --unset GIT_CONFIG_GLOBAL ./run.sh", true},
		{"env --spl", "env --spl 'git push'", true},
		{"env unknown long", "env --frobnicate ./run.sh", true},
		{"env --chdir ok", "env --chdir=/tmp ./run.sh", false},
		{"env --unset other ok", "env --unset LANG ./run.sh", false},
		{"env --null ok", "env --null", false},
		// 2: glob/brace in any word up to the program: wrapper option values, the timeout duration.
		{"timeout glob duration", "timeout 5* git push", true},
		{"timeout brace duration", "timeout {5,git} push", true},
		{"env -C brace", "env -C {x,git} push", true},
		{"nice -n brace", "nice -n {1,git} push", true},
		{"stdbuf brace value", "stdbuf -o {L,git} push", true},
		{"sudo -u glob", "sudo -u r* git status", true},
		{"brace keyword ok", "{ go test ./...; }", false},
		{"assignment glob ok", "FILES=*.go go vet ./...", false},
		{"timeout ok", "timeout 60 go test ./...", false},
		// 3: git subcommands: more outside verbs, command-running forms, unknown subcommands (aliases).
		{"git send-email", "git send-email x.patch", true},
		{"git imap-send", "git imap-send", true},
		{"git svn", "git svn dcommit", true},
		{"git p4", "git p4 submit", true},
		{"git daemon", "git daemon --export-all", true},
		{"git request-pull", "git request-pull v1 origin", true},
		{"git instaweb", "git instaweb", true},
		{"git-send-email binary", "/usr/lib/git-core/git-send-email x", true},
		{"rebase -x", "git rebase -x 'sh x' HEAD~1", true},
		{"rebase --exec", "git rebase --exec='sh x' HEAD~1", true},
		{"rebase --ex abbrev", "git rebase --ex 'sh x' HEAD~1", true},
		{"rebase -ix cluster", "git rebase -ix 'sh x' HEAD~1", true},
		{"rebase ok", "git rebase main", false},
		{"submodule foreach", "git submodule foreach 'sh x'", true},
		{"submodule --quiet foreach", "git submodule --quiet foreach 'sh x'", true},
		{"submodule status ok", "git submodule status", false},
		{"bisect run", "git bisect run ./test.sh", true},
		{"bisect start ok", "git bisect start", false},
		{"difftool", "git difftool -x 'sh x'", true},
		{"difftool --extcmd", "git difftool --extcmd='sh x'", true},
		{"mergetool", "git mergetool", true},
		{"filter-branch", "git filter-branch --tree-filter 'sh x'", true},
		{"filter-repo", "git filter-repo --path x", true},
		{"for-each-repo", "git for-each-repo --config=x -- push", true},
		{"maintenance start", "git maintenance start", true},
		{"hook run", "git hook run pre-commit", true},
		{"fetch --upload-pack", "git fetch --upload-pack='sh x' origin", true},
		{"ls-remote --upload-pack abbrev", "git ls-remote --upload='sh x' origin", true},
		{"clone -u", "git clone -u 'sh x' https://x/y", true},
		{"archive --exec", "git archive --exec='sh x' HEAD", true},
		{"unknown subcommand alias", "git p origin", true},
		{"unknown subcommand lfs", "git lfs push origin", true},
		{"git-lfs binary", "git-lfs push origin", true},
		{"git --version ok", "git --version", false},
		{"git help ok", "git help commit", false},
		{"git worktree list ok", "git worktree list", false},
		{"git fetch ok", "git fetch origin", false},
		{"git log ok", "git log --until=yesterday --reverse", false},
		{"git add -u ok", "git add -u", false},
		// 4: writes and redirects into git config, hooks, $GIT_DIR.
		{"redirect .git/config", "echo '[core]' >> .git/config", true},
		{"redirect hook", "printf 'x' > .git/hooks/pre-commit", true},
		{"redirect abs config", "echo x >> /repo/.git/config", true},
		{"redirect worktree config", "echo x > /repo/.git/worktrees/w/config.worktree", true},
		{"redirect GIT_DIR", `echo x > "$GIT_DIR/config"`, true},
		{"redirect GIT_DIR braces", "echo x > ${GIT_DIR}/hooks/post-commit", true},
		{"redirect &>", "make &> .git/hooks/pre-push", true},
		{"redirect <>", "cat <> .git/config", true},
		{"tee config", "echo x | tee -a .git/config", true},
		{"cp hook", "cp evil.sh .git/hooks/pre-commit", true},
		{"ln hook", "ln -s ../../evil.sh .git/hooks/pre-commit", true},
		{"sed -i config", "sed -i 's/x/y/' .git/config", true},
		{"chmod hook", "chmod +x .git/hooks/pre-commit", true},
		{"cat config ok", "cat .git/config", false},
		{"grep config ok", "grep -n hooksPath .git/config", false},
		{"redirect from config ok", "wc -l < .git/config", false},
		{"redirect elsewhere ok", "go test ./... > /tmp/out.txt 2>&1", false},
		{"index.lock ok", "rm -f .git/index.lock", false},
		{"git config --file ok", "git config --file .git/config commit.gpgsign false", false},
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

// No nested jobs (PR #4 review): agent-jobs is rejected as a command in every form except the job's
// own MCP server, and the AGENT_JOBS_* environment (the job marker) cannot be changed.
func TestCheckNoNestedJobs(t *testing.T) {
	cfg := Config{GitName: "Job Bot", GitEmail: "bot@example.com"}
	cases := []struct {
		name  string
		cmd   string
		block bool
	}{
		{"start", `agent-jobs start --agent agent1 --title t --model opus --brief x`, true},
		{"mcp orchestrator", `agent-jobs mcp --role orchestrator --agent agent1`, true},
		{"mcp role=orchestrator", `agent-jobs mcp --role=orchestrator`, true},
		{"mcp two roles", `agent-jobs mcp --role job --role orchestrator`, true},
		{"mcp no role", `agent-jobs mcp --agent agent1`, true},
		{"mcp extra flag", `agent-jobs mcp --role job --agent agent1 --x y`, true},
		{"mcp extra positional", `agent-jobs mcp --role job start`, true},
		{"cancel", `agent-jobs cancel --agent agent1 261017-1`, true},
		{"tick", `agent-jobs tick --agent agent1`, true},
		{"daemon", `agent-jobs daemon --agent agent1`, true},
		{"list", `agent-jobs list --agent agent1`, true},
		{"bare", `agent-jobs`, true},
		{"path", `/home/u/.local/bin/agent-jobs start --title t`, true},
		{"relative path", `./agent-jobs tick`, true},
		{"upper", `AGENT-JOBS tick`, true},
		{"suffix build", `./agent-jobs-linux tick`, true},
		{"env wrapper", `env FOO=1 agent-jobs tick`, true},
		{"command wrapper", `command agent-jobs tick`, true},
		{"nohup", `nohup agent-jobs daemon --agent agent1 &`, true},
		{"timeout", `timeout 5 agent-jobs tick`, true},
		{"bash -c", `bash -c 'agent-jobs tick'`, true},
		{"sh -c", `sh -c "agent-jobs start --title t"`, true},
		{"substitution", `echo $(agent-jobs list)`, true},
		{"backticks", "x=`agent-jobs list`", true},
		{"eval", `eval 'agent-jobs tick'`, true},
		{"after &&", `true && agent-jobs tick`, true},
		{"in pipeline", `echo x | agent-jobs start --title t`, true},
		{"find -exec", `find . -exec agent-jobs tick \;`, true},
		{"xargs", `echo tick | xargs agent-jobs`, true},
		{"python -c", `python3 -c 'import subprocess; subprocess.run(["agent-jobs", "tick"])'`, true},
		{"cp binary", `cp /home/u/.local/bin/agent-jobs ./x`, true},
		{"mcp job role ok", `agent-jobs mcp --role job --agent agent1`, false},
		{"mcp job role= ok", `agent-jobs mcp --agent=agent1 --role=job`, false},
		{"which ok", `which agent-jobs`, false},
		{"grep ok", `grep -rn agent-jobs docs`, false},
		{"echo ok", `echo agent-jobs`, false},
		// The job marker and the other AGENT_JOBS_* variables.
		{"unset marker", `unset AGENT_JOBS_JOB; ./run.sh`, true},
		{"env -u marker", `env -u AGENT_JOBS_JOB ./run.sh`, true},
		{"env -u attached", `env -uAGENT_JOBS_JOB ./run.sh`, true},
		{"env --unset", `env --unset AGENT_JOBS_JOB ./run.sh`, true},
		{"prefix empty", `AGENT_JOBS_JOB= ./run.sh`, true},
		{"prefix home", `AGENT_JOBS_HOME=/tmp/x ./run.sh`, true},
		{"export", `export AGENT_JOBS_JOB=`, true},
		{"export bare", `export -n AGENT_JOBS_JOB`, true},
		{"declare", `declare +x AGENT_JOBS_JOB`, true},
		{"read", `read AGENT_JOBS_JOB < /dev/null`, true},
		{"printf -v", `printf -v AGENT_JOBS_JOB ''`, true},
		{"expand assign", `echo ${AGENT_JOBS_JOB:=x}`, true},
		{"env assign", `env AGENT_JOBS_JOB= ./run.sh`, true},
		{"env -i", `env -i ./run.sh`, true},
		{"mapfile", `mapfile AGENT_JOBS_JOB < /dev/null`, true},
		{"mapfile -t", `mapfile -t AGENT_JOBS_JOB < /dev/null`, true},
		{"mapfile -d -u", `mapfile -d , -u 3 AGENT_JOBS_JOB 3< /dev/null`, true},
		{"mapfile attached", `mapfile -tAGENT_JOBS_JOB < /dev/null`, true},
		{"readarray", `readarray AGENT_JOBS_JOB < /dev/null`, true},
		{"readarray -t", `readarray -t AGENT_JOBS_HOME < /dev/null`, true},
		{"builtin unset", `builtin unset AGENT_JOBS_JOB`, true},
		{"command unset", `command unset AGENT_JOBS_JOB`, true},
		{"builtin declare", `builtin declare +x AGENT_JOBS_JOB`, true},
		{"builtin mapfile", `builtin mapfile AGENT_JOBS_JOB < /dev/null`, true},
		{"unset -v", `unset -v AGENT_JOBS_JOB`, true},
		{"local", `f() { local AGENT_JOBS_JOB; ./run.sh; }; f`, true},
		{"printf -v attached", `printf -vAGENT_JOBS_JOB ''`, true},
		{"printf -v after --", `printf -- -v AGENT_JOBS_JOB`, false},
		{"printf arg ok", `printf '%s\n' AGENT_JOBS_JOB`, false},
		{"printf -v other ok", `printf -v X '%s' AGENT_JOBS_JOB`, false},
		{"mapfile other ok", `mapfile -t lines < AGENT_JOBS_JOB.txt`, false},
		{"read marker ok", `echo "$AGENT_JOBS_JOB"`, false},
		{"grep marker ok", `grep -rn AGENT_JOBS_JOB docs`, false},
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

// The marker rule against a real bash: every command the guard lets through keeps AGENT_JOBS_JOB in a
// child's environment, and every one it rejects really removes it (so the rule targets real losses).
func TestJobMarkerSurvivesAllowedCommands(t *testing.T) {
	bash, err := exec.LookPath("bash")
	if err != nil {
		t.Skip("no bash")
	}
	cfg := Config{}
	child := `; printenv AGENT_JOBS_JOB || echo GONE`
	for _, c := range []struct {
		cmd     string
		allowed bool
	}{
		{`printf '%s\n' AGENT_JOBS_JOB`, true},
		{`printf -v X '%s' AGENT_JOBS_JOB`, true},
		{`mapfile -t lines < /dev/null`, true},
		{`echo "$AGENT_JOBS_JOB"`, true},
		{`mapfile AGENT_JOBS_JOB < /dev/null`, false},
		{`readarray -t AGENT_JOBS_JOB < /dev/null`, false},
		{`unset AGENT_JOBS_JOB`, false},
		{`builtin unset AGENT_JOBS_JOB`, false},
		{`command unset AGENT_JOBS_JOB`, false},
		{`declare +x AGENT_JOBS_JOB`, false},
		{`export -n AGENT_JOBS_JOB`, false},
		{`printf -v AGENT_JOBS_JOB ''`, false},
	} {
		if got := Check(c.cmd, cfg) == ""; got != c.allowed {
			t.Fatalf("Check(%q) allowed=%v, want %v", c.cmd, got, c.allowed)
		}
		cmd := exec.Command(bash, "-c", c.cmd+child)
		cmd.Env = append(os.Environ(), "AGENT_JOBS_JOB=job-1")
		out, err := cmd.Output()
		if err != nil {
			t.Fatalf("%q: %v", c.cmd, err)
		}
		kept := strings.Contains(string(out), "job-1\n") && !strings.Contains(string(out), "GONE")
		if c.allowed && !kept {
			t.Fatalf("allowed %q dropped the marker: %q", c.cmd, out)
		}
		if !c.allowed && kept {
			t.Fatalf("rejected %q does not drop the marker in bash (rule needless?): %q", c.cmd, out)
		}
	}
}

// A job stays in its cgroup scope: no new units through the user's service manager or bus, no writes
// into the cgroup file system (docs/ARCHITECTURE.md, «Cgroup containment»).
func TestCheckUserManagerAndCgroupFS(t *testing.T) {
	cfg := Config{}
	cases := []struct {
		name  string
		cmd   string
		block bool
	}{
		{"systemd-run scope", `systemd-run --user --scope sleep 600`, true},
		{"systemd-run path", `/usr/bin/systemd-run --user -t bash`, true},
		{"systemd-run behind nohup", `nohup systemd-run --user sleep 9 &`, true},
		{"systemd-run in sh -c", `sh -c 'systemd-run --user --scope sleep 9'`, true},
		{"systemd-run find -exec", `find . -exec systemd-run --user {} \;`, true},
		{"busctl", `busctl --user call org.freedesktop.systemd1 /org/freedesktop/systemd1 x y`, true},
		{"dbus-send", `dbus-send --session --dest=org.freedesktop.systemd1 /x y`, true},
		{"gdbus", `gdbus call --session -d org.freedesktop.systemd1 -o / -m x`, true},
		{"dbus-run-session", `dbus-run-session -- ./run.sh`, true},
		{"systemctl --user start", `systemctl --user start x.service`, true},
		{"systemctl --user after verb", `systemctl start x.service --user`, true},
		{"systemctl abbreviated", `systemctl --us start x.service`, true},
		{"systemctl --user=", `systemctl --user=yes stop x`, true},
		{"echo into cgroup.procs", `echo $$ > /sys/fs/cgroup/user.slice/cgroup.procs`, true},
		{"tee into cgroup", `echo 1 | tee /sys/fs/cgroup/x/cgroup.procs`, true},
		{"cd into cgroupfs", `cd /sys/fs/cgroup/user.slice`, true},
		{"systemctl status ok", `systemctl status nginx`, false},
		{"systemctl --runtime ok", `systemctl --runtime status nginx`, false},
		{"cat cgroup ok", `cat /sys/fs/cgroup/cgroup.controllers`, false},
		{"grep systemd-run ok", `grep -rn systemd-run docs`, false},
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
