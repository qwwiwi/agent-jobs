package guard

import (
	"slices"
	"strings"
	"testing"
)

// GhPrMerges finds every `gh pr merge` call of a command, in chains, pipes, subshells and behind env
// prefixes and wrappers, with the words that decide what it merges; a form it cannot read is unsure.
func TestGhPrMerges(t *testing.T) {
	const sha = "0123456789abcdef0123456789abcdef01234567"
	home, _ := cdEnv("")
	for _, tc := range []struct {
		name string
		cmd  string
		// want: one call (nil = none).
		want   *GhMerge
		unsure string // part of the unsure reason ("" = none)
	}{
		{"not gh", "ls -la && git merge main", nil, ""},
		{"gh pr view", "gh pr view 5", nil, ""},
		{"gh api merge passes", "gh api -X PUT repos/o/r/pulls/5/merge", nil, ""},
		{"gh repo with a flag", "gh repo view --json name merge", nil, ""},
		{"plain", "gh pr merge 5 --squash", &GhMerge{}, ""},
		{"with sha", "gh pr merge 5 --squash --match-head-commit " + sha, &GhMerge{MatchHead: sha, HasMatchHead: true}, ""},
		{"sha with =", "gh pr merge 5 --match-head-commit=" + sha, &GhMerge{MatchHead: sha, HasMatchHead: true}, ""},
		{"quoted sha", `gh pr merge 5 "--match-head-commit" '` + sha + `'`, &GhMerge{MatchHead: sha, HasMatchHead: true}, ""},
		{"short sha", "gh pr merge 5 --match-head-commit 0123abc", &GhMerge{MatchHead: "0123abc", HasMatchHead: true}, ""},
		{"last sha wins", "gh pr merge --match-head-commit aaa 5 --match-head-commit " + sha, &GhMerge{MatchHead: sha, HasMatchHead: true}, ""},
		{"sha as a body", "gh pr merge 5 -b --match-head-commit", &GhMerge{}, ""},
		{"auto", "gh pr merge 5 --auto --match-head-commit " + sha, &GhMerge{MatchHead: sha, HasMatchHead: true, Auto: true}, ""},
		{"auto=false", "gh pr merge 5 --auto=false", &GhMerge{}, ""},
		{"disable auto", "gh pr merge 5 --disable-auto", &GhMerge{DisableAuto: true}, ""},
		{"chained &&", "git fetch && gh pr merge 5 -s", &GhMerge{}, ""},
		{"chained ;", "echo hi; gh pr merge 5", &GhMerge{}, ""},
		{"piped", "yes | gh pr merge 5", &GhMerge{}, ""},
		{"subshell", "(gh pr merge 5)", &GhMerge{}, ""},
		{"env prefix", "GH_REPO=o/r GH_TOKEN=x gh pr merge 5", &GhMerge{GHRepos: []string{"o/r"}}, ""},
		{"env wrapper", "env GH_REPO=o/r gh pr merge 5", &GhMerge{GHRepos: []string{"o/r"}}, ""},
		{"exported earlier", "export GH_REPO=o/r; gh pr merge 5", &GhMerge{GHRepos: []string{"o/r"}}, ""},
		{"full path", "/usr/bin/gh pr merge 5", &GhMerge{}, ""},
		{"command wrapper", "command gh pr merge 5", &GhMerge{}, ""},
		{"-R before", "gh -R o/r pr merge 5", &GhMerge{Repo: "o/r"}, ""},
		{"-R attached", "gh -Ro/r pr merge 5", &GhMerge{Repo: "o/r"}, ""},
		{"--repo= before", "gh --repo=o/r pr merge 5", &GhMerge{Repo: "o/r"}, ""},
		{"--repo after", "gh pr merge 5 --repo o/r", &GhMerge{Repo: "o/r"}, ""},
		{"-R after", "gh pr merge -R o/r 5", &GhMerge{Repo: "o/r"}, ""},
		{"url", "gh pr merge https://github.com/o/r/pull/5 -R x/y", &GhMerge{Repo: "github.com/o/r"}, ""},
		{"cd first", "cd /srv/app && gh pr merge 5", &GhMerge{Dirs: []string{"/w", "/srv/app"}}, ""},
		{"cd twice", "cd /srv && cd app; gh pr merge 5", &GhMerge{Dirs: []string{"/w", "/srv", "/w/app", "/srv/app"}}, ""},
		{"bare cd", "cd && gh pr merge 5", &GhMerge{Dirs: []string{"/w", home}}, ""},
		{"variable sha", `gh pr merge 5 --match-head-commit "$SHA"`, &GhMerge{HasMatchHead: true,
			Unsure: "a variable or substitution in the words of gh pr merge"}, ""},
		{"sh -c", `sh -c 'gh pr merge 5'`, &GhMerge{Unsure: "inside a substitution, eval or sh -c"}, "gh inside"},
		{"eval", `eval "gh pr merge 5"`, &GhMerge{Unsure: "inside a substitution, eval or sh -c"}, "gh inside"},
		{"wrapper option", "env -C /srv gh pr merge 5", &GhMerge{Unsure: "a wrapper option before gh"}, ""},
		{"cd to a variable", `cd "$D" && gh pr merge 5`, &GhMerge{Unsure: "a cd the reader cannot follow"}, ""},
		{"cd to a variable, no merge", `cd "$D" && git log --merges`, nil, ""},
		{"GH_REPO from a variable, no merge", `GH_REPO=$X; echo hi`, nil, ""},
		{"GH_REPO from a variable", `GH_REPO=$X; gh pr merge 5`, &GhMerge{Unsure: "GH_REPO set from a variable"}, ""},
		{"GIT_DIR prefix", "GIT_DIR=/srv/app/.git gh pr merge 5", &GhMerge{Unsure: "GIT_DIR or GIT_WORK_TREE"}, ""},
		{"GIT_WORK_TREE exported", "export GIT_WORK_TREE=/srv/app; gh pr merge 5", &GhMerge{Unsure: "GIT_DIR or GIT_WORK_TREE"}, ""},
		{"declare -x", "declare -x GH_REPO=o/r; gh pr merge 5", &GhMerge{GHRepos: []string{"o/r"}}, ""},
		{"upper-case url", "gh pr merge HTTPS://github.com/o/r/pull/5", &GhMerge{Repo: "github.com/o/r"}, ""},
		{"git merge-base in a substitution", "git diff $(git merge-base origin/main HEAD)", nil, ""},
		{"git commit here-doc about merge", "git commit -F - <<'EOF'\nmerge the branch\nEOF", nil, ""},
		{"program from a variable", `$GH pr merge 5`, nil, "command name from a variable"},
		{"subcommand from a variable", `gh pr $M 5`, nil, "gh subcommand from a variable"},
		{"unknown flag before pr", "gh --foo pr merge 5", &GhMerge{Unsure: "gh flag --foo before the subcommand"}, "gh flag --foo"},
		{"shell from stdin", "echo 'gh pr merge 5' | sh", nil, "reading commands from stdin"},
		{"unverifiable without merge or gh", "echo x | sh", nil, "reading commands from stdin"},
	} {
		calls, unsure := GhPrMerges(tc.cmd, "/w")
		if tc.unsure == "" && unsure != "" || tc.unsure != "" && !strings.Contains(unsure, tc.unsure) {
			t.Errorf("%s: unsure %q, want %q", tc.name, unsure, tc.unsure)
		}
		if tc.want == nil {
			if len(calls) != 0 {
				t.Errorf("%s: calls %+v, want none", tc.name, calls)
			}
			continue
		}
		if len(calls) != 1 {
			t.Errorf("%s: calls %+v, want one", tc.name, calls)
			continue
		}
		got, want := calls[0], *tc.want
		if !strings.Contains(got.Unsure, want.Unsure) || (want.Unsure == "") != (got.Unsure == "") {
			t.Errorf("%s: call unsure %q, want %q", tc.name, got.Unsure, want.Unsure)
		}
		if want.Unsure != "" && strings.HasPrefix(want.Unsure, "gh flag") {
			continue // the rest of the call was not read
		}
		got.Unsure, want.Unsure = "", ""
		if want.Dirs == nil {
			want.Dirs = []string{"/w"}
		}
		if got.Repo != want.Repo || !slices.Equal(got.GHRepos, want.GHRepos) || !slices.Equal(got.Dirs, want.Dirs) ||
			got.HasMatchHead != want.HasMatchHead || (want.MatchHead != "" && got.MatchHead != want.MatchHead) ||
			got.Auto != want.Auto || got.DisableAuto != want.DisableAuto {
			t.Errorf("%s: %+v, want %+v", tc.name, got, want)
		}
	}
}

// Two merges in one command are both found, each with the cd targets before it.
func TestGhPrMergesSeveral(t *testing.T) {
	calls, unsure := GhPrMerges("gh pr merge 1 && cd /other && gh pr merge 2 --auto", "/w")
	if unsure != "" || len(calls) != 2 || !slices.Equal(calls[0].Dirs, []string{"/w"}) || !slices.Equal(calls[1].Dirs, []string{"/w", "/other"}) ||
		!calls[1].Auto {
		t.Fatalf("%+v %q", calls, unsure)
	}
	if _, unsure := GhPrMerges("gh pr merge 5 "+strings.Repeat("x", maxCommandBytes), "/w"); unsure == "" {
		t.Fatal("an over-long command with a merge is read")
	}
}

// The reader follows a cd the way the shell may run it (a cd in a subshell or a branch not taken adds a
// candidate and removes none) and is unsure of every directory change, selector or limit it cannot
// follow exactly; a literal gh pr subcommand other than merge is never unsure, wherever it is nested.
func TestGhPrMergesFollowsTheShell(t *testing.T) {
	deep := "gh pr merge 5"
	for range maxNesting + 1 {
		deep = "eval " + shellQuote(deep)
	}
	for _, tc := range []struct {
		name    string
		cmd     string
		calls   int
		dir     string // a directory the (only) call must list
		unsure  string // part of the call's Unsure ("" = none)
		command string // part of the command's unsure ("" = none)
	}{
		{"cd in a subshell", "(cd /elsewhere); cd gated && gh pr merge 5", 1, "/w/gated", "", ""},
		{"cd in a branch not taken", "false && cd /elsewhere; cd gated && gh pr merge 5", 1, "/w/gated", "", ""},
		{"HOME set, bare cd", "HOME=/srv/r; cd; gh pr merge 5", 1, "", "HOME, GIT_CONFIG*", ""},
		{"bash -c with an option after -c", "bash -c -e 'gh pr merge 5 -R o/r'", 0, "", "", "bash"},
		{"sh -c --", "sh -c -- 'gh pr merge 5'", 0, "", "", "sh"},
		{"GH_REPO exported in a function", "f(){ gh pr merge 5; }; export GH_REPO=o/r; f", 1, "", "loop or a function", ""},
		{"GIT_DIR exported in a loop", "for i in 1 2; do gh pr merge 5; export GIT_DIR=/x/.git; done", 1, "", "loop or a function", ""},
		{"printf -v GH_REPO", "printf -v GH_REPO %s o/r; export GH_REPO; gh pr merge 5", 1, "", "set by printf", ""},
		{"read GH_REPO", "read GH_REPO <<< o/r; gh pr merge 5", 1, "", "set by read", ""},
		{"readonly GH_REPO", "readonly GH_REPO=o/r; gh pr merge 5", 1, "", "", ""},
		{"echo $GH_REPO", `echo "$GH_REPO"; gh pr merge 5`, 1, "/w", "", ""},
		{"git remote set-url", "git remote set-url origin https://github.com/o/r.git && gh pr merge 5", 1, "", "git remote", ""},
		{"git clone", "git clone https://github.com/o/r /x/g && cd /x/g && gh pr merge 5", 1, "", "git clone", ""},
		{"git remote -v", "git remote -v && gh pr merge 5", 1, "/w", "", ""},
		{"GIT_CONFIG_COUNT prefix", "GIT_CONFIG_COUNT=1 GIT_CONFIG_KEY_0=url.x.insteadOf gh pr merge 5", 1, "", "GIT_CONFIG*", ""},
		{"HOME prefix on gh", "HOME=/x gh pr merge 5", 1, "", "HOME, GIT_CONFIG*", ""},
		{"gh alias", "gh alias set mg 'pr merge' && gh mg 5 -R o/r", 0, "", "", "no built-in gh command"},
		{"gh extension", "gh dash", 0, "", "", "no built-in gh command"},
		{"gh api", "gh api repos/{owner}/{repo}/pulls", 0, "", "", ""},
		{"cd -- -x", "cd -- -gated && gh pr merge 5", 1, "", "starts with -", ""},
		{"-R ignores an unreadable cd", `cd "$X" && gh pr merge 5 -R o/r`, 1, "", "", ""},
		{"source still unsure with -R", "source ./x.sh; gh pr merge 5 -R o/r", 1, "", "source or .", ""},
		{"HOME prefix on cd", "HOME=/srv/r cd && gh pr merge 5", 1, "", "a cd the reader cannot follow", ""},
		{"CDPATH exported", "export CDPATH=/srv; cd r && gh pr merge 5", 1, "", "a cd the reader cannot follow", ""},
		{"popd", "popd; gh pr merge 5", 1, "", "a cd the reader cannot follow", ""},
		{"cd ~user", "cd ~bob && gh pr merge 5", 1, "", "a cd the reader cannot follow", ""},
		{"cd glob", "cd dem* && gh pr merge 5", 1, "", "glob, brace or tilde in a cd target", ""},
		{"cd -P ..", "cd -P .. && gh pr merge 5", 1, "", "cd -P with ..", ""},
		{"cd in eval", "eval cd /x; gh pr merge 5", 1, "", "inside an eval or trap body", ""},
		{"GH_REPO in eval", "eval export GH_REPO=o/r; gh pr merge 5", 1, "", "inside an eval or trap body", ""},
		{"cd in a trap", "trap 'cd /x' DEBUG; gh pr merge 5", 1, "", "inside an eval or trap body", ""},
		{"cd in a function", "f() { cd x; }; f; gh pr merge 5", 1, "", "loop or a function", ""},
		{"cd in a loop", "for i in 1 2; do cd ..; done; gh pr merge 5", 1, "", "loop or a function", ""},
		{"source", "source ./env.sh; gh pr merge 5", 1, "", "source or .", ""},
		{"alias", "alias c='cd /x'; gh pr merge 5", 1, "", "an alias", ""},
		{"-R glob", "gh pr merge 5 -R acme/dem*", 1, "", "glob, brace or tilde in the words", ""},
		{"-R brace", "gh pr merge 5 -R acme/{demo,x}", 1, "", "glob, brace or tilde in the words", ""},
		{"tilde PR argument", "gh pr merge ~/x", 1, "", "glob, brace or tilde in the words", ""},
		{"GH_REPO glob", "GH_REPO=acme/dem* gh pr merge 5", 1, "", "glob, brace or tilde in GH_REPO", ""},
		{"OWNER/REPO#N", "gh pr merge acme/demo#5", 1, "", "OWNER/REPO#N reference", ""},
		{"other # argument", "gh pr merge 5#x", 1, "", "neither a number", ""},
		{"no PR URL", "gh pr merge https://github.com/o/r/pulls/5", 1, "", "no pull request URL", ""},
		{"#N", "gh pr merge '#5'", 1, "/w", "", ""},
		{"branch name", "gh pr merge feature/x", 1, "/w", "", ""},
		{"over-long, continued", "# " + strings.Repeat("x", maxCommandBytes) + "\ngh pr mer\\\nge 5", 0, "", "", "longer than the reader's limit"},
		{"too deep", deep, 0, "", "", "nests deeper than the reader's limit"},
		{"stdin shell without merge", "echo Z2g= | base64 -d | sh", 0, "", "", "reading commands from stdin"},
		{"gh pr view in a substitution", "X=$(gh pr view 5 --json mergeStateStatus -q .mergeStateStatus)", 0, "", "", ""},
		{"gh pr view in a quoted substitution", `echo "$(gh pr view 5 --json mergeable)"`, 0, "", "", ""},
		{"gh pr view in bash -c", "bash -c 'gh pr view 5 --json mergeable'", 0, "", "", ""},
		{"gh pr subcommand from a variable in a substitution", "X=$(gh pr $S 5)", 0, "", "", "gh"},
		{"gh pr merge --help", "gh pr merge --help", 1, "/w", "", ""},
	} {
		calls, unsure := GhPrMerges(tc.cmd, "/w")
		if tc.command == "" && unsure != "" || tc.command != "" && !strings.Contains(unsure, tc.command) {
			t.Errorf("%s: unsure %q, want %q", tc.name, unsure, tc.command)
		}
		if len(calls) != tc.calls {
			t.Errorf("%s: calls %+v, want %d", tc.name, calls, tc.calls)
			continue
		}
		if tc.calls == 0 {
			continue
		}
		c := calls[0]
		if tc.unsure == "" && c.Unsure != "" || tc.unsure != "" && !strings.Contains(c.Unsure, tc.unsure) {
			t.Errorf("%s: call unsure %q, want %q", tc.name, c.Unsure, tc.unsure)
		}
		if tc.dir != "" && !slices.Contains(c.Dirs, tc.dir) {
			t.Errorf("%s: dirs %q lack %s", tc.name, c.Dirs, tc.dir)
		}
	}
}

// shellQuote single-quotes s for a shell.
func shellQuote(s string) string { return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'" }
