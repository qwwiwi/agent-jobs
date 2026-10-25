package jobs

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/qwwiwi/agent-jobs/internal/checklist"
	"github.com/qwwiwi/agent-jobs/internal/store"
)

// The hook input must be a PreToolUse event with a tool_name; a Bash call needs tool_input.command.
func TestParsePreToolUseInput(t *testing.T) {
	in, err := ParsePreToolUseInput(strings.NewReader(`{"hook_event_name":"PreToolUse","session_id":"s1","cwd":"/x",` +
		`"tool_name":"Bash","tool_input":{"command":"gh pr merge 5"}}`))
	if err != nil || in.Cwd != "/x" || in.ToolName != "Bash" || in.Command != "gh pr merge 5" || in.SessionID != "s1" {
		t.Fatalf("%+v %v", in, err)
	}
	if in, err := ParsePreToolUseInput(strings.NewReader(`{"hook_event_name":"PreToolUse","tool_name":"Read","tool_input":{}}`)); err != nil ||
		in.Command != "" {
		t.Fatalf("another tool: %+v %v", in, err)
	}
	for name, raw := range map[string]string{
		"other event":     `{"hook_event_name":"Stop","session_id":"s1"}`,
		"no tool":         `{"hook_event_name":"PreToolUse"}`,
		"bash no command": `{"hook_event_name":"PreToolUse","tool_name":"Bash","tool_input":{}}`,
		"bash bad input":  `{"hook_event_name":"PreToolUse","tool_name":"Bash","tool_input":{"command":5}}`,
		"bad json":        `{`,
		"empty":           ``,
	} {
		if _, err := ParsePreToolUseInput(strings.NewReader(raw)); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

// End to end: release_check passed on sha A in a repository with an approved checklist; gh pr merge with
// --match-head-commit A is allowed (also from another directory after cd, by -R or by GH_REPO), with B,
// a short sha, --auto or no flag it is refused with the missing condition named; another clone is gated
// by its remote; a newer verdict makes the pass stale; a merge the reader cannot read is refused; a
// repository without a checklist is not gated; a void or removed approval stays gated.
func TestMergeGateEndToEnd(t *testing.T) {
	m := acceptanceManager(t, "")
	repo, shaA := gateRepo(t)
	gitT(t, repo, "remote", "add", "origin", "git@github.com:Acme/Demo.git")
	approveBrief(t, m, repo)
	if err := m.Store.Insert(&store.Job{ID: "cx-merge", Agent: m.Agent, Title: "codex coder", Kind: "general", Brief: "b",
		RepoPath: mainWorktree(repo), Status: store.StatusSucceeded, Model: "gpt-5.6-sol", Runner: store.RunnerCodex, CreatedAt: time.Now(),
		Size: SizeLarge, CoverContract: &store.CoverContract{ChecklistID: "demo", Version: 2}, Covers: []string{"R01", "R02", "R03i"}}); err != nil {
		t.Fatal(err)
	}
	gate := func(cwd, cmd, env string) string {
		t.Helper()
		r, err := m.MergeGate(cwd, cmd, env)
		if err != nil {
			t.Fatalf("%s: %v", cmd, err)
		}
		if strings.Contains(r, "\n") || (r != "" && !strings.HasPrefix(r, MergeGatePrefix)) {
			t.Fatalf("%s: not one refusal line: %q", cmd, r)
		}
		return r
	}
	merge := "gh pr merge 5 --squash --match-head-commit "
	if r := gate(repo, merge+shaA, ""); !strings.Contains(r, "release_check has not passed on "+shaA) {
		t.Fatalf("before release_check: %q", r)
	}
	if j := runToEnd(t, m, judgeJob(t, m, repo, shaA).ID); j.Status != store.StatusSucceeded {
		t.Fatalf("judge: %s %s", j.Status, j.Error)
	}
	if r, err := m.ReleaseCheck(repo, shaA); err != nil || !r.Pass {
		t.Fatalf("release_check: %+v %v", r, err)
	}
	if err := os.WriteFile(filepath.Join(repo, "next.txt"), []byte("next\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitT(t, repo, "add", "next.txt")
	gitT(t, repo, "commit", "-qm", "next")
	shaB := gitT(t, repo, "rev-parse", "HEAD")
	elsewhere := t.TempDir()
	clone := filepath.Join(t.TempDir(), "clone")
	gitT(t, elsewhere, "clone", "-q", repo, clone)
	gitT(t, clone, "remote", "set-url", "origin", "https://github.com/acme/demo.git")

	for _, tc := range []struct {
		name, cwd, cmd, env string
		want                string // part of the refusal ("" = allowed)
	}{
		{"A", repo, merge + shaA, "", ""},
		{"A upper case", repo, merge + strings.ToUpper(shaA), "", ""},
		{"A from a subdirectory", filepath.Join(repo, "brief"), merge + shaA, "", ""},
		{"A after cd", elsewhere, "cd " + repo + " && " + merge + shaA, "", ""},
		{"A by -R", elsewhere, "gh -R acme/demo pr merge 5 --match-head-commit " + shaA, "", ""},
		{"B", repo, merge + shaB, "", "release_check has not passed on " + shaB},
		{"no flag", repo, "gh pr merge 5 --squash", "", "without --match-head-commit"},
		{"short sha", repo, merge + shaA[:12], "", "is not a full 40-hex sha"},
		{"auto", repo, merge + shaA + " --auto", "", "--auto"},
		{"chained B", elsewhere, "git status; cd " + repo + " && " + merge + shaB, "", "has not passed on " + shaB},
		{"env prefix B", elsewhere, "GH_REPO=acme/demo " + merge + shaB, "", "has not passed on " + shaB},
		{"session GH_REPO B", elsewhere, merge + shaB, "Acme/Demo", "has not passed on " + shaB},
		{"-R B", elsewhere, "gh pr merge 5 -R github.com/acme/demo --match-head-commit " + shaB, "", "has not passed on " + shaB},
		{"url without flag", elsewhere, "gh pr merge https://github.com/acme/demo/pull/5", "", "without --match-head-commit"},
		{"-R names another repo", repo, "gh pr merge 5 -R other/repo", "", ""},
		{"not in a repo", elsewhere, "gh pr merge 5", "", ""},
		{"disable auto", repo, "gh pr merge 5 --disable-auto", "", ""},
		{"no merge", repo, "gh pr view 5 && git log", "", ""},
		{"sh -c", elsewhere, "sh -c 'gh pr merge 5'", "", "cannot tell whether this command runs gh pr merge"},
		{"variable sha", repo, `gh pr merge 5 --match-head-commit "$SHA"`, "", "cannot tell"},
		{"subcommand from a variable", elsewhere, `gh pr $M 5`, "", "cannot tell"},
		{"GIT_DIR", elsewhere, "GIT_DIR=" + repo + "/.git " + merge + shaA, "", "cannot tell"},
		{"another clone without flag", clone, "gh pr merge 5", "", "without --match-head-commit"},
		{"another clone, A", clone, merge + shaA, "", ""},
		{"git about merge, not gated", elsewhere, "git diff $(git merge-base main HEAD)", "", ""},
	} {
		r := gate(tc.cwd, tc.cmd, tc.env)
		if tc.want == "" && r != "" || tc.want != "" && !strings.Contains(r, tc.want) {
			t.Errorf("%s: %q, want %q", tc.name, r, tc.want)
		}
	}

	// A newer verdict for the sha makes the pass stale until release_check runs again.
	if j := runToEnd(t, m, judgeJob(t, m, repo, shaA).ID); j.Status != store.StatusSucceeded {
		t.Fatalf("second judge: %s %s", j.Status, j.Error)
	}
	if r := gate(repo, merge+shaA, ""); !strings.Contains(r, "the release_check pass on "+shaA+" is stale") {
		t.Fatalf("newer verdict: %q", r)
	}
	if r, err := m.ReleaseCheck(repo, shaA); err != nil || !r.Pass {
		t.Fatalf("release_check again: %+v %v", r, err)
	}
	if r := gate(repo, merge+shaA, ""); r != "" {
		t.Fatalf("after release_check again: %q", r)
	}

	// A repository without a checklist is not gated (here, and with nothing gated at all).
	plain := newRepo(t)
	if r := gate(plain, "gh pr merge 5", ""); r != "" {
		t.Fatalf("no checklist: %q", r)
	}
	other := newManager(t)
	if r, err := other.MergeGate(plain, "sh -c 'gh pr merge 5'", ""); err != nil || r != "" {
		t.Fatalf("nothing gated, unsure: %q %v", r, err)
	}

	// A checklist edited by hand after the approval voids it: the repository stays gated.
	p := checklist.ChecklistPath(repo)
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(strings.Replace(string(b), "RATIONALE_MARKER", "edited by hand", 1)), 0o644); err != nil {
		t.Fatal(err)
	}
	if r := gate(repo, merge+shaA, ""); !strings.Contains(r, "its checklist approval is void") {
		t.Fatalf("void approval: %q", r)
	}
	// So does a checklist removed after the approval.
	if err := os.Remove(p); err != nil {
		t.Fatal(err)
	}
	if r := gate(repo, merge+shaA, ""); !strings.Contains(r, "its checklist approval is void (the work tree has no brief/CHECKLIST.md)") {
		t.Fatalf("removed checklist: %q", r)
	}
}

// gitHubRepoName reads OWNER/REPO the way gh and git remotes write it.
func TestGitHubRepoName(t *testing.T) {
	for in, want := range map[string]string{
		"acme/demo": "acme/demo", "github.com/Acme/Demo": "acme/demo", "https://github.com/acme/demo.git": "acme/demo",
		"ssh://git@github.com/acme/demo.git": "acme/demo", "git@github.com:acme/demo.git": "acme/demo",
		"https://github.com/acme/demo/": "acme/demo", "demo": "", "": "", "a/b/c/d": "",
	} {
		if got := gitHubRepoName(in); got != want {
			t.Errorf("%q: %q, want %q", in, got, want)
		}
	}
}
