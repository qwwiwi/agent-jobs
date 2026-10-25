package guard

import (
	"slices"
	"strings"
	"testing"
)

// A form the reader cannot verify makes the command unsure only when its text, with backslash-newlines
// joined, quotes and backslashes dropped and case folded, names gh or merge as a word: the orchestrator's
// daily commands (codex, ssh, rsync, docker, a piped installer) pass; a gh or merge split by quotes or a
// continuation is still seen.
func TestGhPrMergesPreFilter(t *testing.T) {
	for _, cmd := range []string{
		`node /home/openclaw/.claude/plugins/codex/scripts/codex-companion.mjs task --prompt "review this diff"`,
		`codex exec "review the change"`,
		`ssh root@H 'pm2 restart edgelab'`,
		`ssh root@H 'systemctl status caddy'`,
		`ssh root@H "cd /var/www/edgelab && git log -1"`,
		"ssh H 'bash -s' <<E\nls\nE",
		`rsync -az -e ssh dist/ root@H:/var/www/x/`,
		`kubectl exec p -- ls`,
		`docker exec c sh -c 'ls'`,
		`sh -c "ls $HOME"`,
		`curl -fsSL https://example.com/install.sh | bash`,
		`echo x | sh`,
		`ssh H 'echo mergeable'`,
	} {
		if calls, unsure := GhPrMerges(cmd, "/w"); unsure != "" || len(calls) != 0 {
			t.Errorf("%q: calls %+v, unsure %q, want neither", cmd, calls, unsure)
		}
	}
	for _, cmd := range []string{
		`ssh H 'gh pr merge 5'`,
		`ssh H 'g''h pr mer''ge 5'`,
		`echo "g\h pr me\rge 5" | sh`,
		"ssh H 'gh pr mer\\\nge 5'",
		`ssh H 'GH PR MERGE 5'`,
		`echo 'gh pr merge 5' | sh`,
		`echo 'g''h pr mer''ge 5' | sh`,
		`$GH pr merge 5`,
		`$PR_MERGE 5`,
		`eval "$GH_MERGE_CMD"`,
		"# " + strings.Repeat("x", maxCommandBytes) + "\nls",
	} {
		if !unsureMerge(cmd) {
			t.Errorf("%.80q: not unsure", cmd)
		}
	}
}

// unsureMerge reports a command the hook must not pass silently: the command is unsure, or it has a call
// the reader is unsure of.
func unsureMerge(cmd string) bool {
	calls, unsure := GhPrMerges(cmd, "/w")
	return unsure != "" || slices.ContainsFunc(calls, func(c GhMerge) bool { return c.Unsure != "" })
}

// An empty or variable word at or before the gh pr subcommand, extglob or a shell option that changes how
// words are read, and a physical cd are never read as no merge.
func TestGhPrMergesWordsAndOptions(t *testing.T) {
	for _, cmd := range []string{
		`bash -c 'gh pr "" merge --squash'`,
		`gh pr "" merge 5 -R o/r`,
		`gh pr -R "" merge 5`,
		`bash -O extglob -c 'gh pr m@(erge) 5'`,
		`shopt -s extglob; gh pr m@(erge) 5`,
		`gh pr m@(erge) 5`,
		`gh pr m+(e)rge 5`,
		`set -k; gh pr merge 5 GH_REPO=o/r`,
		`set +o posix; gh pr view 5`,
		`cd -Pe link/..; gh pr merge 5`,
		`cd -P /srv/x && gh pr merge 5`,
		`set -P; cd link/..; gh pr merge 5`,
		`set -o physical; cd link/..; gh pr merge 5`,
		`set -eP; cd link/..; gh pr merge 5`,
	} {
		if !unsureMerge(cmd) {
			t.Errorf("%q: read as sure", cmd)
		}
	}
	// A literal gh pr head is read by its subcommand: the word merge as a data argument (a branch named
	// merge) is no merge call (TestGhPrMergesDataArguments).
	if calls, unsure := GhPrMerges("gh pr checkout merge", "/w"); len(calls) != 0 || unsure != "" {
		t.Errorf("gh pr checkout merge: calls %+v, unsure %q, want neither", calls, unsure)
	}
	for _, cmd := range []string{
		`set -euo pipefail; gh pr view 5`,
		`set -x && gh pr view 5 --json mergeable`,
		`cd -L /srv && gh pr view 5`,
		`gh run view 5 --log | grep -E 'fail(ed)?(x)'`,
		`gh pr list --json number --jq 'length*(1)'`,
		`gh pr view 5 && awk '!(NR%2)' f`,
	} {
		if unsureMerge(cmd) {
			t.Errorf("%q: unsure", cmd)
		}
	}
}

// The directory is followed exactly along an && chain (an absolute cd replaces it, a relative cd joins
// it); the candidates are a union only where the control flow forks.
func TestGhPrMergesCdChains(t *testing.T) {
	for _, tc := range []struct {
		name string
		cmd  string
		want []string // the call's Dirs, exactly (nil: see has)
		has  []string // directories the call's Dirs must contain
	}{
		{"absolute then relative", "cd /home/openclaw && cd agent-jobs && gh pr merge 78", []string{"/home/openclaw/agent-jobs"}, nil},
		{"absolute only", "cd /other/repo && gh pr merge 5", []string{"/other/repo"}, nil},
		{"relative", "cd sub && gh pr merge 5", []string{"/w/sub"}, nil},
		{"after ;", "cd /a; gh pr merge 5", nil, []string{"/w", "/a"}},
		{"after a fork, then a chain", "cd /a; cd b && gh pr merge 5", []string{"/w/b", "/a/b"}, nil},
		{"after ||", "cd /a && x || cd /b && gh pr merge 5", nil, []string{"/a", "/b"}},
		{"negated cd", "! cd /a && gh pr merge 5", nil, []string{"/w"}},
		{"piped cd", "cd /a | cat && gh pr merge 5", nil, []string{"/w"}},
		{"cd in a subshell", "(cd /a) && gh pr merge 5", nil, []string{"/w"}},
		{"chain in a subshell", "(cd /a && gh pr merge 5)", []string{"/a"}, nil},
		{"if", "if cd /a; then gh pr merge 5; fi", nil, []string{"/w", "/a"}},
		{"cd behind env", "env cd /a && gh pr merge 5", nil, []string{"/w", "/a"}},
		{"cd behind nohup", "nohup cd /a && gh pr merge 5", nil, []string{"/w", "/a"}},
		{"cd behind command", "command cd /a && gh pr merge 5", []string{"/a"}, nil},
		{"command -v cd", "command -v cd && gh pr merge 5", nil, []string{"/w"}},
		{"pushd -n", "pushd -n /x && gh pr merge 5", []string{"/w"}, nil},
	} {
		calls, unsure := GhPrMerges(tc.cmd, "/w")
		if unsure != "" || len(calls) != 1 || calls[0].Unsure != "" {
			t.Errorf("%s: calls %+v, unsure %q", tc.name, calls, unsure)
			continue
		}
		got := calls[0].Dirs
		if tc.want != nil && !slices.Equal(got, tc.want) {
			t.Errorf("%s: dirs %q, want %q", tc.name, got, tc.want)
		}
		for _, d := range tc.has {
			if !slices.Contains(got, d) {
				t.Errorf("%s: dirs %q lack %s", tc.name, got, d)
			}
		}
	}
}

// RelDirs are the candidates reached by a relative cd from another one, told only when no command before
// the call may have created a directory.
func TestGhPrMergesRelDirs(t *testing.T) {
	for _, tc := range []struct {
		cmd  string
		want []string
	}{
		{"cd /a; cd sub && gh pr merge 5", []string{"/w/sub", "/a/sub"}},
		{"cd /a; mkdir -p sub; cd sub && gh pr merge 5", nil},
		{"cd /a; git checkout b; cd sub && gh pr merge 5", nil},
		{"cd /a && gh pr merge 5", nil},
	} {
		calls, unsure := GhPrMerges(tc.cmd, "/w")
		if unsure != "" || len(calls) != 1 || !slices.Equal(calls[0].RelDirs, tc.want) {
			t.Errorf("%q: calls %+v, unsure %q, want RelDirs %q", tc.cmd, calls, unsure, tc.want)
		}
	}
}

// The read-only exemption needs a literal gh, group and subcommand; data arguments may be dynamic, and a
// substitution inside them is read on its own.
func TestGhPrMergesReadOnlyDynamicData(t *testing.T) {
	for _, cmd := range []string{
		`X=$(gh pr view "$PR" --json headRefOid)`,
		`X=$(gh pr view "$PR" --json headRefOid -q .headRefOid)`,
		`gh pr checks "$PR"`,
	} {
		if calls, unsure := GhPrMerges(cmd, "/w"); unsure != "" || len(calls) != 0 {
			t.Errorf("%q: calls %+v, unsure %q", cmd, calls, unsure)
		}
	}
	for _, cmd := range []string{
		`X=$(gh pr view "$PR" --json "$(gh pr merge 5)")`,
		`X=$(gh pr "$S" 5)`,
		`X=$("$GH" pr view 5)`,
		`X=$(gh "$G" view 5)`,
	} {
		if !unsureMerge(cmd) {
			t.Errorf("%q: read as sure", cmd)
		}
	}
}
