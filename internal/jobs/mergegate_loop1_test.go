package jobs

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/qwwiwi/agent-jobs/internal/store"
)

// mergeGateRepo is a repository with an approved checklist and the remote acme/demo (no release_check
// pass: a gated merge is refused for its missing --match-head-commit or pass).
func mergeGateRepo(t *testing.T) (*Manager, string) {
	t.Helper()
	m := acceptanceManager(t, "")
	repo, _ := gateRepo(t)
	gitT(t, repo, "remote", "add", "origin", "git@github.com:Acme/Demo.git")
	approveBrief(t, m, repo)
	return m, repo
}

func gateAnswer(t *testing.T, m *Manager, cwd, cmd, env string) string {
	t.Helper()
	r, err := m.MergeGate(cwd, cmd, env)
	if err != nil {
		t.Fatalf("%s: %v", cmd, err)
	}
	return r
}

// A gated repository whose work tree is gone is still gated by its stored OWNER/REPO names (recorded by
// the hook when it read them), and never makes a merge in an unrelated repository unreadable.
func TestMergeGateWorkTreeGone(t *testing.T) {
	m, repo := mergeGateRepo(t)
	plain := newRepo(t)
	gitT(t, plain, "remote", "add", "origin", "https://github.com/other/repo.git")
	// The hook reads (and stores) the gated repository's names on a merge it checks.
	if r := gateAnswer(t, m, plain, "gh pr merge 5 -R acme/demo", ""); !strings.Contains(r, "without --match-head-commit") {
		t.Fatalf("by name, work tree present: %q", r)
	}
	clone := filepath.Join(t.TempDir(), "clone")
	gitT(t, t.TempDir(), "clone", "-q", repo, clone)
	gitT(t, clone, "remote", "set-url", "origin", "https://github.com/acme/demo.git")
	if err := os.RemoveAll(repo); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name, cwd, cmd string
		want           string // part of the refusal ("" = allowed)
	}{
		{"unrelated repo", plain, "gh pr merge 5", ""},
		{"unrelated -R", plain, "gh pr merge 5 -R other/repo", ""},
		{"not in a repo", t.TempDir(), "gh pr merge 5", ""},
		{"gone repo by name", plain, "gh pr merge 5 -R Acme/Demo", "its checklist approval is void"},
		{"clone of the gone repo", clone, "gh pr merge 5", "its checklist approval is void"},
	} {
		if r := gateAnswer(t, m, tc.cwd, tc.cmd, ""); tc.want == "" && r != "" || tc.want != "" && !strings.Contains(r, tc.want) {
			t.Errorf("%s: %q, want %q", tc.name, r, tc.want)
		}
	}
}

// A cd the reader cannot follow exactly never counts as proof that the merge runs outside the gated
// repository: a cd in a subshell or a branch not taken adds a candidate directory, HOME or CDPATH set in
// the command makes the call unsure.
func TestMergeGateCdLikeTheShell(t *testing.T) {
	m, repo := mergeGateRepo(t)
	parent, name := filepath.Dir(repo), filepath.Base(repo)
	elsewhere := t.TempDir()
	for _, tc := range []struct {
		name, cwd, cmd string
		want           string
	}{
		{"cd in a subshell", parent, "(cd " + elsewhere + "); cd " + name + " && gh pr merge 5", "without --match-head-commit"},
		{"cd in a branch not taken", parent, "false && cd " + elsewhere + "; cd " + name + " && gh pr merge 5", "without --match-head-commit"},
		{"HOME set, bare cd", elsewhere, "HOME=" + repo + "; cd; gh pr merge 5", "cannot tell"},
		{"HOME prefix on cd", elsewhere, "HOME=" + repo + " cd && gh pr merge 5", "cannot tell"},
		{"CDPATH exported", elsewhere, "export CDPATH=" + parent + "; cd " + name + " && gh pr merge 5", "cannot tell"},
		{"plain cd to an unrelated directory", parent, "cd " + elsewhere + " && gh pr merge 5", ""},
	} {
		if r := gateAnswer(t, m, tc.cwd, tc.cmd, ""); tc.want == "" && r != "" || tc.want != "" && !strings.Contains(r, tc.want) {
			t.Errorf("%s: %q, want %q", tc.name, r, tc.want)
		}
	}
}

// Selectors the shell would expand, PR arguments tied to no one repository and a command the reader
// cannot read whole are unsure; a literal gh pr subcommand other than merge is not, wherever it is nested.
func TestMergeGateSelectorsAndLimits(t *testing.T) {
	m, repo := mergeGateRepo(t)
	elsewhere := t.TempDir()
	for _, tc := range []struct {
		name, cwd, cmd string
		want           string
	}{
		{"-R glob", elsewhere, "gh pr merge 5 -R acme/dem*", "cannot tell"},
		{"-R brace", elsewhere, "gh pr merge 5 -R acme/{demo,x}", "cannot tell"},
		{"GH_REPO glob", elsewhere, "GH_REPO=acme/dem* gh pr merge 5", "cannot tell"},
		{"cd glob", filepath.Dir(repo), "cd " + filepath.Base(repo)[:1] + "* && gh pr merge 5", "cannot tell"},
		{"OWNER/REPO#N", elsewhere, "gh pr merge acme/demo#5", "cannot tell"},
		{"over-long, continued", elsewhere, "# " + strings.Repeat("x", 64<<10) + "\ngh pr mer\\\nge 5", "cannot tell"},
		{"gh pr view in a substitution", repo, "X=$(gh pr view 5 --json mergeStateStatus -q .mergeStateStatus)", ""},
		{"gh pr view in a quoted substitution", repo, `echo "$(gh pr view 5 --json mergeable)"`, ""},
		{"gh pr view in bash -c", repo, "bash -c 'gh pr view 5 --json mergeable'", ""},
		{"gh pr merge in a substitution", elsewhere, "X=$(gh pr merge 5)", "cannot tell"},
	} {
		if r := gateAnswer(t, m, tc.cwd, tc.cmd, ""); tc.want == "" && r != "" || tc.want != "" && !strings.Contains(r, tc.want) {
			t.Errorf("%s: %q, want %q", tc.name, r, tc.want)
		}
	}
}

// The owner retires a repository from the gate (also once its work tree is gone): its merges and the
// merges matched by its names pass; a later approval gates it again. RecordGateNames stores the names
// of its remotes, so a work tree removed before any hook call is still matched by name.
func TestMergeGateRetire(t *testing.T) {
	m, repo := mergeGateRepo(t)
	if err := m.RecordGateNames(repo); err != nil {
		t.Fatal(err)
	}
	if g, err := m.Store.GateRepoOf(mainWorktree(repo)); err != nil || len(g.Names) != 1 || g.Names[0] != "acme/demo" {
		t.Fatalf("recorded names: %+v %v", g, err)
	}
	if _, err := m.RetireGate("relative/path"); err == nil {
		t.Fatal("a relative path retired")
	}
	if _, err := m.RetireGate(t.TempDir()); !errors.Is(err, store.ErrNotGated) {
		t.Fatalf("an ungated path: %v", err)
	}
	main := mainWorktree(repo)
	if err := os.RemoveAll(repo); err != nil {
		t.Fatal(err)
	}
	elsewhere := t.TempDir()
	if r := gateAnswer(t, m, elsewhere, "gh pr merge 5 -R acme/demo", ""); !strings.Contains(r, "its checklist approval is void") {
		t.Fatalf("gone, by recorded name: %q", r)
	}
	if got, err := m.RetireGate(main); err != nil || got != main {
		t.Fatalf("retire: %q %v", got, err)
	}
	if r := gateAnswer(t, m, elsewhere, "gh pr merge 5 -R acme/demo", ""); r != "" {
		t.Fatalf("after retire: %q", r)
	}
	if r := gateAnswer(t, m, elsewhere, "sh -c 'gh pr merge 5'", ""); r != "" {
		t.Fatalf("unsure with nothing gated after retire: %q", r)
	}

	// Approved again: gated again.
	m2, repo2 := mergeGateRepo(t)
	if _, err := m2.RetireGate(repo2); err != nil {
		t.Fatal(err)
	}
	if r := gateAnswer(t, m2, repo2, "gh pr merge 5", ""); r != "" {
		t.Fatalf("retired, work tree present: %q", r)
	}
	approveBrief(t, m2, repo2)
	if r := gateAnswer(t, m2, repo2, "gh pr merge 5", ""); !strings.Contains(r, "without --match-head-commit") {
		t.Fatalf("approved after retire: %q", r)
	}
}
