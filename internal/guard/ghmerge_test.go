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
		{"cd first", "cd /srv/app && gh pr merge 5", &GhMerge{Dirs: []string{"/srv/app"}}, ""},
		{"cd twice", "cd /srv && cd app; gh pr merge 5", &GhMerge{Dirs: []string{"/srv", "app"}}, ""},
		{"bare cd", "cd && gh pr merge 5", &GhMerge{Dirs: []string{""}}, ""},
		{"variable sha", `gh pr merge 5 --match-head-commit "$SHA"`, &GhMerge{HasMatchHead: true,
			Unsure: "a variable or substitution in the words of gh pr merge"}, ""},
		{"sh -c", `sh -c 'gh pr merge 5'`, &GhMerge{Unsure: "inside a substitution, eval or sh -c"}, "indirectly"},
		{"eval", `eval "gh pr merge 5"`, &GhMerge{Unsure: "inside a substitution, eval or sh -c"}, "gh inside"},
		{"wrapper option", "env -C /srv gh pr merge 5", &GhMerge{Unsure: "a wrapper option before gh"}, ""},
		{"cd to a variable", `cd "$D" && gh pr merge 5`, &GhMerge{Unsure: "cd to a variable"}, ""},
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
		{"unverifiable without merge or gh", "echo x | sh", nil, ""},
	} {
		calls, unsure := GhPrMerges(tc.cmd)
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
		if got.Repo != want.Repo || !slices.Equal(got.GHRepos, want.GHRepos) || !slices.Equal(got.Dirs, want.Dirs) ||
			got.HasMatchHead != want.HasMatchHead || (want.MatchHead != "" && got.MatchHead != want.MatchHead) ||
			got.Auto != want.Auto || got.DisableAuto != want.DisableAuto {
			t.Errorf("%s: %+v, want %+v", tc.name, got, want)
		}
	}
}

// Two merges in one command are both found, each with the cd targets before it.
func TestGhPrMergesSeveral(t *testing.T) {
	calls, unsure := GhPrMerges("gh pr merge 1 && cd /other && gh pr merge 2 --auto")
	if unsure != "" || len(calls) != 2 || len(calls[0].Dirs) != 0 || !slices.Equal(calls[1].Dirs, []string{"/other"}) || !calls[1].Auto {
		t.Fatalf("%+v %q", calls, unsure)
	}
	if _, unsure := GhPrMerges("gh pr merge 5 " + strings.Repeat("x", maxCommandBytes)); unsure == "" {
		t.Fatal("an over-long command with a merge is read")
	}
	if calls, unsure := GhPrMerges("cat > f <<'EOF'\n" + strings.Repeat("x", maxCommandBytes) + "\nEOF"); unsure != "" || calls != nil {
		t.Fatalf("an over-long command without merge: %+v %q", calls, unsure)
	}
}
