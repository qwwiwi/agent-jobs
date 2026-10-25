package jobs

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// While a repository is gated, the orchestrator's daily commands that the guard cannot verify but that
// name neither gh nor merge pass; a gh pr view with a dynamic PR argument passes too.
func TestMergeGateDailyCommands(t *testing.T) {
	m, repo := mergeGateRepo(t)
	for _, cmd := range []string{
		`node /home/openclaw/.claude/plugins/codex/scripts/codex-companion.mjs task --prompt "review this diff"`,
		`codex exec "review the change"`,
		`ssh root@H 'pm2 restart edgelab'`,
		`ssh root@H "cd /var/www/edgelab && git log -1"`,
		`rsync -az -e ssh dist/ root@H:/var/www/x/`,
		`docker exec c sh -c 'ls'`,
		`curl -fsSL https://example.com/install.sh | bash`,
		`X=$(gh pr view "$PR" --json headRefOid)`,
	} {
		if r := gateAnswer(t, m, repo, cmd, ""); r != "" {
			t.Errorf("%q: %q", cmd, r)
		}
	}
	for _, cmd := range []string{
		`ssh H 'g''h pr mer''ge 5'`,
		`bash -c 'gh pr "" merge --squash'`,
		`bash -O extglob -c 'gh pr m@(erge) 5'`,
	} {
		if r := gateAnswer(t, m, t.TempDir(), cmd, ""); !strings.Contains(r, "cannot tell") {
			t.Errorf("%q: %q", cmd, r)
		}
	}
}

// The directory of a call is followed along its && chain: a session in the gated repository merges in
// another repository after cd to it, and a cd chain into the gated repository is checked there, not
// refused for a directory the chain never reaches. A candidate missing now that a relative cd derived
// from another candidate is skipped; when every candidate is such a one, the call is unsure.
func TestMergeGateCdChains(t *testing.T) {
	m, repo := mergeGateRepo(t)
	parent, name := filepath.Dir(repo), filepath.Base(repo)
	other := newRepo(t)
	gitT(t, other, "remote", "add", "origin", "https://github.com/other/repo.git")
	elsewhere := t.TempDir()
	// elsewhere/sub exists, repo/sub does not: the mkdir in the command may create it before the cd.
	if err := os.Mkdir(filepath.Join(elsewhere, "sub"), 0o755); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name, cwd, cmd string
		want           string // part of the refusal ("" = allowed)
	}{
		{"cd to another repo", repo, "cd " + other + " && gh pr merge 5 --squash", ""},
		{"absolute then relative into the gated repo", elsewhere, "cd " + parent + " && cd " + name + " && gh pr merge 78 --merge",
			"without --match-head-commit"},
		{"relative after a fork", elsewhere, "cd " + parent + "; cd " + name + " && gh pr merge 5", "without --match-head-commit"},
		{"a directory the command creates", elsewhere, "mkdir y && cd y && gh pr merge 5", "cannot tell"},
		{"a relative directory the command may create", elsewhere, "cd " + repo + "; mkdir -p sub; cd sub && gh pr merge 5",
			"cannot tell"},
	} {
		if r := gateAnswer(t, m, tc.cwd, tc.cmd, ""); tc.want == "" && r != "" || tc.want != "" && !strings.Contains(r, tc.want) {
			t.Errorf("%s: %q, want %q", tc.name, r, tc.want)
		}
	}
}

// A gated repository approved before names were stored, whose work tree is gone: a merge that names a
// repository or runs in another work tree cannot be told apart from one of it, so it is unsure.
func TestMergeGateNoNamesUnreadable(t *testing.T) {
	m, repo := mergeGateRepo(t)
	plain := newRepo(t)
	gitT(t, plain, "remote", "add", "origin", "https://github.com/other/repo.git")
	if err := os.RemoveAll(repo); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name, cwd, cmd string
	}{
		{"by -R", t.TempDir(), "gh pr merge 5 -R acme/demo"},
		{"by URL", t.TempDir(), "gh pr merge https://github.com/acme/demo/pull/5"},
		{"in another work tree", plain, "gh pr merge 5"},
	} {
		if r := gateAnswer(t, m, tc.cwd, tc.cmd, ""); !strings.Contains(r, "cannot tell") {
			t.Errorf("%s: %q", tc.name, r)
		}
	}
}
