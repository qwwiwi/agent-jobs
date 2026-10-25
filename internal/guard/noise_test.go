package guard

import (
	"path/filepath"
	"strings"
	"testing"
)

// noiseSetup is the state layout of stateReadSetup with a git identity, and the job's work dir
// without the symlink to jobs.db (a real worktree has none: `*` there names worktree files only).
func noiseSetup(t *testing.T) (state, own string, cfg Config) {
	t.Helper()
	state, own, cfg = stateReadSetup(t)
	cfg.GitName, cfg.GitEmail = "Job Bot", "bot@example.com"
	cfg.OwnerNames = []string{"Alex"}
	return state, own, cfg
}

// The false refusals measured in docs/BENCHMARK.md («Guard refusals», audit job 261025-193139-b348):
// each form was refused before the relaxation in noise.go and is allowed now.
func TestNoiseRelaxedFormsAllowed(t *testing.T) {
	state, own, cfg := noiseSetup(t)
	db := filepath.Join(state, "jobs.db")
	other := filepath.Join(state, "jobs", "k", "report.md")
	for _, c := range []string{
		// A state-dir path or a glob mentioned in the text of a here-doc written to a file.
		"cat > notes.md <<'EOF'\nthe database is " + db + "\nEOF",
		"cat > notes.md <<'EOF'\nreports: ~/.agent-jobs/thrall/jobs/*/report.md\nEOF",
		"cat > notes.md <<EOF\nsee " + other + "\nEOF",
		"cat <<'EOF' > notes.md\nglobs /** and src/**/*.go\nEOF",
		"tee notes.md <<'EOF'\n" + state + "/jobs/*/stream.jsonl\nEOF",
		"cat >> notes.md <<-'EOF'\n\t" + db + "\n\tEOF",
		// Read-only git in a command substitution.
		`echo "$(git log --oneline -3)"`,
		`x=$(git diff --stat HEAD~1); echo "$x"`,
		`n=$(git rev-list --count HEAD); echo "$n"`,
		`echo $(git show -s --format=%s HEAD)`,
		"echo `git rev-parse --short HEAD`",
		`base=$(git merge-base HEAD main) && git diff "$base" --stat`,
		`echo "$(git -C . log --all --oneline -- internal/guard | head -3)"`,
		// A glob-like string in inline code is code, not a write target.
		`python3 -c "import glob; print(glob.glob('internal/*.go'))"`,
		`python3 -c "open('out.txt','w').write('a*b')"`,
		`python3 -I -c "print(2*3)"`,
		`node -e "console.log([1,2].map(x => x*2))"`,
		`perl -0pi -e 's/(a)/{$1}/' notes.md`,
		`awk '{print $1*2}' data.txt`,
		// Forms of the audit that pass already; kept so they stay allowed.
		"python3 -I script.py " + own,
		"python3 -I script.py " + filepath.Join(own, "stream.jsonl"),
		"git log --all -- internal/guard/guard.go",
		"git log --all --oneline -- internal/guard",
	} {
		if r := Check(c, cfg); r != "" {
			t.Errorf("%q refused: %s", c, r)
		}
	}
}

// The real policies still refuse, also in the forms next to the relaxed ones.
func TestNoiseRealPoliciesStillRefused(t *testing.T) {
	state, own, cfg := noiseSetup(t)
	db := filepath.Join(state, "jobs.db")
	other := filepath.Join(state, "jobs", "k", "report.md")
	cases := []struct{ name, cmd, want string }{
		// Reads of the state dir and other jobs' folders: the here-doc is code, or is fed to one.
		{"heredoc into python", "python3 - <<'EOF'\nprint(open('" + db + "').read())\nEOF", "agent-jobs state"},
		{"heredoc into sh", "sh <<'EOF'\ncat " + other + "\nEOF", ""},
		{"cat heredoc piped to python", "cat <<'EOF' | python3 -\nprint(open('" + db + "').read())\nEOF", ""},
		{"tee heredoc piped to sh", "tee x.sh <<'EOF' | sh\ncat " + other + "\nEOF", ""},
		{"xargs cat heredoc", "xargs cat <<'EOF'\n" + db + "\nEOF", ""},
		{"here-string", "cat <<< " + db, ""},
		{"substitution in unquoted body", "cat > n.md <<EOF\n$(cat " + other + ")\nEOF", "agent-jobs state"},
		{"variable in unquoted body", "cat > n.md <<EOF\n$HOME/.agent-jobs/thrall/jobs.db\nEOF", ""},
		{"cat reads state with heredoc", "cat " + db + " <<'EOF'\nx\nEOF", "agent-jobs state"},
		{"heredoc written into other job", "cat > " + other + " <<'EOF'\nx\nEOF", ""},
		{"heredoc into process substitution", "cat <<'EOF' > >(sh)\ncat " + db + "\nEOF", ""},
		{"nested cat heredoc", "python3 -c \"$(cat <<'EOF'\nprint(open('" + db + "').read())\nEOF\n)\"", ""},
		{"plain state read", "cat " + other, "agent-jobs state"},
		{"glob over other jobs", "cat " + state + "/jobs/*/report.md", "agent-jobs state"},
		{"inline code reads state", "python3 -c \"print(open('" + db + "').read())\"", "agent-jobs state"},
		{"script arg in other job", "python3 -I script.py " + filepath.Join(own, "..", "k", "report.md"), "agent-jobs state"},
		{"git -C other job", "git -C " + filepath.Join(state, "jobs", "k") + " log", "agent-jobs state"},
		{"git log state path in subst", "echo $(git log --all -- " + other + ")", ""},
		// Outward actions, in a substitution too.
		{"push", "git push origin main", ""},
		{"push in subst", `echo "$(git push origin main)"`, ""},
		{"tag in subst", `x=$(git tag v1)`, ""},
		{"gh pr create", "gh pr create --fill", ""},
		{"gh pr merge", "gh pr merge 3", ""},
		{"gh in subst", `echo "$(gh pr list)"`, ""},
		{"gh api in subst", `x=$(gh api repos/x/y)`, ""},
		// Git identity and config injection: no relaxation reaches them.
		{"git -c in subst", `echo "$(git -c core.pager=sh log)"`, ""},
		{"hooksPath in subst", `echo "$(git -c core.hooksPath=/tmp/h log)"`, ""},
		{"exec-path in subst", `echo "$(git --exec-path=/tmp/x log)"`, ""},
		{"config write in subst", `echo "$(git config core.hooksPath /tmp/h)"`, ""},
		{"identity in subst", `echo "$(git commit --author='X <x@y>' -m x)"`, ""},
		{"commit in subst", `echo "$(git commit -m x)"`, ""},
		{"git via sh -c in subst", `echo "$(sh -c 'git log')"`, ""},
		{"git via eval in subst", `echo "$(eval git log)"`, ""},
		{"git from stdin in subst", `echo "$(git log < f)"`, ""},
		{"xargs git in subst", `echo "$(echo HEAD | xargs git log)"`, ""},
		{"git -c plain", "git -c core.hooksPath=/tmp/h status", ""},
		{"git -c identity", `git -c user.name=Someone commit -m x`, ""},
		// Package runners and nested agents in a here-doc job or substitution.
		{"npx", "npx tsx x.ts", ""},
		{"npx in subst", `echo "$(npx tsx x.ts)"`, ""},
		{"nested agent in subst", `echo "$(claude -p hi)"`, ""},
		// Writes into .git, by glob or literally, also from inline code.
		{"glob into git dir", "cp x .gi?/hooks/pre-commit", "glob/brace"},
		{"glob operand of sed -i", "sed -i 's/a/b/' *.go", "glob/brace"},
		{"heredoc to git hooks", "cat > .git/hooks/pre-commit <<'EOF'\nx\nEOF", ""},
		{"dotglob", "shopt -s dotglob", ""},
		// Secrets and protected paths.
		{"protected db write", "cat > " + db + " <<'EOF'\nx\nEOF", ""},
		{"subagent log", "cat > " + filepath.Join(state, "subagents", "j.log") + " <<'EOF'\nx\nEOF", ""},
		{"job env", "AGENT_JOBS_JOB= cat > n.md <<'EOF'\nx\nEOF", ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			r := Check(c.cmd, cfg)
			if r == "" {
				t.Fatalf("%q allowed", c.cmd)
			}
			if c.want != "" && !strings.Contains(r, c.want) {
				t.Fatalf("%q refused for another reason: %s", c.cmd, r)
			}
		})
	}
}

// Messaging and other tools that reach a person stay outside the job allowlist.
func TestNoiseOutwardToolsStillRefused(t *testing.T) {
	for _, tool := range []string{"SendMessage", "PushNotification", "CronCreate", "mcp__telegram__send_message", "Monitor"} {
		if ToolAllowed(tool) {
			t.Errorf("tool %s allowed", tool)
		}
	}
}
