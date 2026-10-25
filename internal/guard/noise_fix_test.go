package guard

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Fix loop 1 of the noise relaxations (noise.go): each case was allowed before the fix and reaches
// state, git admin files or an outward action. The bash behaviour each case relies on was checked in
// a scratch shell (bash 5): see the comments.
type refusalCase struct{ name, cmd, want string }

func runRefused(t *testing.T, cfg Config, cases []refusalCase) {
	t.Helper()
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

func runAllowed(t *testing.T, cfg Config, cmds []string) {
	t.Helper()
	for _, c := range cmds {
		if r := Check(c, cfg); r != "" {
			t.Errorf("%q refused: %s", c, r)
		}
	}
}

// F1/F4: in an unquoted here-doc bash joins backslash-newline before it compares a line with the
// delimiter (`EO\` + `F` ends `<<EOF`; `EOF\` + an empty line too), so the lines after it run as
// commands. A quoted delimiter joins nothing.
func TestNoiseFixHeredocContinuation(t *testing.T) {
	state, _, cfg := noiseSetup(t)
	db := filepath.Join(state, "jobs.db")
	other := filepath.Join(state, "jobs", "k", "report.md")
	runRefused(t, cfg, []refusalCase{
		{"H1 state read after joined end", "cat > notes.md <<EOF\nx\nEO\\\nF\ncat " + db + "\nEOF", "agent-jobs state"},
		{"H2 other job read", "cat > notes.md <<EOF\nx\nEO\\\nF\ncat " + other + "\nEOF", "agent-jobs state"},
		{"H3 push after joined end", "cat > notes.md <<EOF\nx\nEO\\\nF\ngit push origin main\nEOF", ""},
		{"H4 protected write", "cat > notes.md <<EOF\nx\nEO\\\nF\necho x > " + db + "\nEOF", ""},
		{"H5 tee", "tee notes.md <<EOF\nx\nEO\\\nF\ncat " + other + "\nEOF", "agent-jobs state"},
		{"H15 strip tabs", "cat > notes.md <<-EOF\n\tx\n\tEO\\\nF\ncat " + db + "\nEOF", "agent-jobs state"},
		{"H16 empty continuation", "cat > notes.md <<EOF\nx\nEOF\\\n\ncat " + other + "\nEOF", "agent-jobs state"},
		{"H16 push", "cat > notes.md <<EOF\nx\nEOF\\\n\ngit push origin main\nEOF", ""},
		// `$\` + `(…)` is `$(…)` once joined: the substitution runs.
		{"joined substitution", "cat > notes.md <<EOF\n$\\\n(cat " + other + ")\nEOF", "agent-jobs state"},
		{"joined substitution push", "cat > notes.md <<EOF\n$\\\n(git push origin main)\nEOF", ""},
		// A backslash in an unquoted body is processed by bash: the body is not literal data.
		{"backslash body", "cat > notes.md <<EOF\n" + other + "\\\n\nEOF", "agent-jobs state"},
	})
	runAllowed(t, cfg, []string{
		// Quoted delimiter: no join, the body (with the fake end) is data.
		"cat > notes.md <<'EOF'\nx\nEO\\\nF\ncat " + db + "\nEOF",
		"cat > notes.md <<'EOF'\nline with a \\\ncontinuation\nEOF",
		// `\\` + newline is an escaped backslash, not a join: EOF on the next line ends the body.
		"cat > notes.md <<EOF\nx\\\\\nEOF\necho done",
		"cat > notes.md <<EOF\nplain text\nEOF",
	})
	// The join keeps the body read to the real end: a fake end inside it is not one.
	runRefused(t, cfg, []refusalCase{
		{"H15 tab continuation is body", "cat > notes.md <<-EOF\n\tx\n\tEO\\\n\tF\ncat " + db + "\nEOF\ncat " + other, "agent-jobs state"},
	})
}

// F2: inside backticks bash removes the backslash of \\, \` and \$ (and of \" inside double quotes)
// before it runs the text, so an escaped backtick there opens a nested substitution.
func TestNoiseFixNestedBackticks(t *testing.T) {
	state, _, cfg := noiseSetup(t)
	other := filepath.Join(state, "jobs", "k", "report.md")
	runRefused(t, cfg, []refusalCase{
		{"push nested in git log", "echo `git log \\`git push origin main\\``", ""},
		{"gh merge nested in rev-parse", "x=`git rev-parse \\`gh pr merge 1\\``", ""},
		{"push nested in echo", "echo `echo a \\`git push\\``", ""},
		{"nested in unquoted here-doc", "cat > n.md <<EOF\n`git log \\`git push origin main\\``\nEOF", ""},
		{"nested in double quotes", "echo \"`echo a \\`git push origin main\\``\"", ""},
		{"escaped quote in double quotes", "echo \"`echo \\\"'\\\"; git push origin main; \\\"'\\\"`\"", ""},
		{"nested state read", "echo `echo \\`cat " + other + "\\``", "agent-jobs state"},
		{"escaped dollar substitution", "echo `echo \\$(git push origin main)`", ""},
	})
	runAllowed(t, cfg, []string{
		"echo `git log -1 \\`git rev-parse HEAD\\``",
		"echo `echo a \\\\ b`",
		"echo `git rev-parse --short HEAD`",
	})
}

// F3: an unquoted brace or glob in an inline-code word is expanded by the shell into more operands
// (a file name after the code); the relaxation holds only for glob characters that were quoted.
func TestNoiseFixInlineCodeUnquotedGlob(t *testing.T) {
	_, _, cfg := noiseSetup(t)
	runRefused(t, cfg, []refusalCase{
		{"python brace", `python3 -c {'import sys;open(sys.argv[1],"w").write("x")',.git/hooks/pre-commit}`, ""},
		{"perl -pi brace glob", `perl -pi -e {'s/a/b/',*.go}`, ""},
		{"perl -i brace git", `perl -i -pe {'s/^/x/',.gi?/config}`, ""},
		{"node brace", `node -e {'require("fs").writeFileSync(process.argv[1],"x")',.git/hooks/pre-commit}`, ""},
		{"perl bare glob", `perl -e .gi?/config`, ""},
		{"python unquoted star", `python3 -c print\(2*3\)`, ""},
	})
	runAllowed(t, cfg, []string{
		`python3 -c "print(2*3)"`,
		`perl -e 'print 2*3'`,
		`python3 -c print\(2\)`,
	})
}

// S1: a quoted glob in inline code is resolved by the language (perl glob, python glob.glob): a
// pattern that can name git admin files, the job's git dirs, protected paths or the agent state
// outside the job's own folder is refused; arithmetic and patterns over worktree files stay allowed.
func TestNoiseFixInlineCodeLanguageGlob(t *testing.T) {
	state, own, cfg := noiseSetup(t)
	// A real worktree has no link to jobs.db: the state check expands `*` of `a * b` on disk.
	if err := os.Remove(filepath.Join(own, "work", "lnk")); err != nil {
		t.Fatal(err)
	}
	home := filepath.Dir(filepath.Dir(state))
	gitDir := filepath.Join(home, "repo", ".git", "worktrees", "j")
	cfg.GitDirs = []string{gitDir}
	runRefused(t, cfg, []refusalCase{
		{"perl glob git config", `perl -e 'open my $f, ">", scalar glob(".gi?/config") or die $!'`, ""},
		{"perl glob star git", `perl -e 'unlink glob(".g*t/hooks/*")'`, ""},
		{"perl angle glob", `perl -e 'for (<.gi[t]/config>) { open my $f, ">", $_ }'`, ""},
		{"perl brace glob", `perl -e 'unlink glob(".{g,x}it/config")'`, ""},
		{"python glob git", `python3 -c "import glob; open(glob.glob('.gi?/config')[0],'w').write('x')"`, ""},
		{"python glob hooks", `python3 -c "import glob; [open(p,'w') for p in glob.glob('.g*/hooks/*')]"`, ""},
		{"python hidden star hooks", `python3 -c "import glob; print(glob.glob('*/hooks/*', include_hidden=True))"`, ""},
		{"python dot star config", `python3 -c "import glob; print(glob.glob('.*/config'))"`, ""},
		{"python git dir glob", `python3 -c "import glob; print(glob.glob('` + filepath.Join(home, "repo", ".g?t", "worktrees", "*", "config") + `'))"`, ""},
		{"python worktree git dir glob", `python3 -c "import glob; print(glob.glob('` + filepath.Join(home, "re?o", "*", "worktrees", "j", "*") + `'))"`, ""},
		{"python state glob", `python3 -c "import glob; print(open(glob.glob('` + home + `/.agent-j*/thrall/jobs.db')[0]).read())"`, ""},
		{"python other jobs glob", `python3 -c "import glob; print(glob.glob('` + home + `/.agent-jo?s/thrall/jobs/*/report.md'))"`, ""},
		{"node glob protected", `node -e "require('fs').globSync('` + home + `/.agent-*/*/subagents/*').forEach(f => require('fs').writeFileSync(f, ''))"`, ""},
		{"brace lists past the bound", `perl -e 'print glob("{a,b}{a,b}{a,b}{a,b}{a,b}{a,b}{a,b}")'`, ""},
		{"concatenated git glob", `python3 -c "import glob; open(glob.glob('.g' 'i?/config')[0],'w')"`, ""},
		{"concatenated hooks glob", `perl -e 'unlink glob(".git/" . "hoo*/pre-commit")'`, ""},
		{"tilde state glob", `python3 -c "import glob,os; print(glob.glob(os.path.expanduser('~/.agent-*/thrall/jobs/*/report.md')))"`, ""},
	})
	runAllowed(t, cfg, []string{
		`python3 -c "print(2*3)"`,
		`python3 -c "a, b = 2, 3; print(a * b)"`,
		`python3 -c "import glob; print(glob.glob('internal/*.go'))"`,
		`python3 -c "import glob; print(glob.glob('**/*.go', recursive=True))"`,
		`python3 -c "open('out.txt','w').write('a*b')"`,
		`perl -e 'print join(",", glob("*.go"))'`,
		`perl -pe 's/.*/x/' notes.md`,
		`python3 -c "import re; print(re.sub(r'.*', 'x', 'abc'))"`,
		`python3 -c "import glob; print(glob.glob('internal/*/x.go'))"`,
		`node -e "console.log(2 * 3)"`,
		`awk '$1*2 > 3' data.txt`,
		`python3 -c "import glob; print(glob.glob('` + own + `/*.md'))"`,
	})
}

// S2/F5: git's --output writes the diff to a file. In a substitution it is no read; as a plain call
// its value is a write target checked like a redirection.
func TestNoiseFixGitOutput(t *testing.T) {
	state, _, cfg := noiseSetup(t)
	db := filepath.Join(state, "jobs.db")
	runRefused(t, cfg, []refusalCase{
		{"subst output hooks", `echo "$(git diff --output=.git/hooks/pre-commit HEAD)"`, ""},
		{"subst output file", `echo "$(git show -s --format=x --output=README.md HEAD)"`, ""},
		{"subst output abbrev", `echo "$(git diff --outp=README.md HEAD)"`, ""},
		{"subst output separate", `x=$(git log -p --output README.md -1)`, ""},
		{"backtick output", "echo `git diff --output=README.md`", ""},
		{"plain output hooks", "git diff --output=.git/hooks/pre-commit HEAD", ""},
		{"plain output separate hooks", "git diff --output .git/hooks/pre-commit HEAD", ""},
		{"plain abbrev hooks", "git log -p --outpu=.git/config", ""},
		{"plain output git dir", "git -C . show --output=.git/config HEAD", ""},
		{"plain output protected", "git diff --output=" + db, ""},
		{"plain output glob", "git diff --output=.gi?/config", ""},
	})
	runAllowed(t, cfg, []string{
		"git diff --output=out.diff HEAD",
		"git diff --stat HEAD",
		"git log -- --output=x",
		`echo "$(git diff --output-indicator-new=+ HEAD)"`,
		`echo "$(git log --oneline -3)"`,
	})
	// A review job: --output into the worktree is a write there.
	jobDir, review, _ := readOnlySetup(t)
	runRefused(t, review, []refusalCase{
		{"read-only output", "git diff --output=notes.md", "review job, read-only"},
		{"read-only output separate", "git diff --output notes.md HEAD", "review job, read-only"},
		{"read-only output abbrev", "git show --outp=main.go HEAD", "review job, read-only"},
	})
	runAllowed(t, review, []string{
		"git diff HEAD",
		"git diff --output=" + filepath.Join(jobDir, "review.diff") + " HEAD",
	})
}
