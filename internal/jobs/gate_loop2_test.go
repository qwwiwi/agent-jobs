package jobs

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/qwwiwi/agent-jobs/internal/store"
)

// A replace ref cannot swap what the judge sees: the copy of the files at the sha is the commit's own
// tree and blobs, and a sha that is a commit only through a replacement is no commit.
func TestJudgeIgnoresReplaceRefs(t *testing.T) {
	repo, sha := gateRepo(t)
	if err := os.WriteFile(filepath.Join(repo, "main.go"), []byte("package main // FAKE\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitT(t, repo, "commit", "-qam", "fake")
	fake := gitT(t, repo, "rev-parse", "HEAD")
	gitT(t, repo, "replace", sha, fake)
	realBlob := gitT(t, repo, "--no-replace-objects", "rev-parse", sha+":main.go")
	fakeBlob := gitT(t, repo, "rev-parse", fake+":main.go")
	gitT(t, repo, "replace", realBlob, fakeBlob)
	// Inherited from the daemon's environment, a replace-ref base would bring them back.
	t.Setenv("GIT_REPLACE_REF_BASE", "refs/replace/")
	dst := t.TempDir()
	if err := materializeTree(repo, sha, dst, skipInJudgeTree); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(filepath.Join(dst, "main.go")); !strings.Contains(string(b), "CODE_MARKER") {
		t.Fatalf("judge copy of main.go at %s: %q (the replacement)", sha, b)
	}
	tree := gitT(t, repo, "--no-replace-objects", "rev-parse", sha+"^{tree}")
	gitT(t, repo, "replace", "-f", tree, fake)
	if err := CheckCommit(repo, tree); err == nil {
		t.Fatalf("tree %s replaced by a commit passed as a commit", tree)
	}
	if err := CheckCommit(repo, sha); err != nil {
		t.Fatal(err)
	}
}

// judge_start warns when any coder of the checklist shares the judge's family, as same_family is set:
// mixed coders do not hide it.
func TestJudgeFamilyWarningMixedCoders(t *testing.T) {
	m := newManager(t)
	m.Cfg.CodexWriteAllowed = true // a codex fix job
	repo, sha := gateRepo(t)
	approveBrief(t, m, repo)
	judge := func(model string) *store.Job {
		t.Helper()
		j, err := m.StartGate(GateRequest{Kind: KindJudge, Repo: repo, SHA: sha, Model: model})
		if err != nil {
			t.Fatal(err)
		}
		return j
	}
	claude := enqueue(t, m, "build it", func(r *StartRequest) { r.Kind, r.RepoPath, r.Covers = "fix", repo, []string{"R01"} })
	codex := enqueue(t, m, "build it too", func(r *StartRequest) {
		r.Kind, r.RepoPath, r.Covers, r.Model = "fix", repo, []string{"R01"}, "gpt-5.6-sol"
	})
	for _, c := range []*store.Job{claude, codex} {
		if err := m.Cancel(c.ID); err != nil {
			t.Fatal(err)
		}
	}
	for _, model := range []string{"sonnet", "gpt-5.6-sol"} {
		j := judge(model)
		share := claude
		if model != "sonnet" {
			share = codex
		}
		w, err := m.JudgeFamilyWarning(j)
		if err != nil || !strings.Contains(w, "same_family") || !strings.Contains(w, share.ID) {
			t.Fatalf("judge %s of mixed coders: %q %v", model, w, err)
		}
	}
}

// The judge's prompt names the paths of the tree at the sha that its copy leaves out, as present at the
// commit but withheld, so a requirement that needs one is not judged missing.
func TestJudgePromptNamesWithheldPaths(t *testing.T) {
	m := newManager(t)
	repo, sha := contextRepo(t)
	approveBrief(t, m, repo)
	j, err := m.StartGate(GateRequest{Kind: KindJudge, Repo: repo, SHA: sha, Model: "sonnet"})
	if err != nil {
		t.Fatal(err)
	}
	prompt, err := m.BuildPrompt(j, "rules", nil, MemoryNotes{})
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{`".claude"`, `"CLAUDE.md"`, `"AGENTS.md"`, `"sub/deep/.claude"`, `"pkg/AGENTS.md"`, `"docs/claude.md"`, `"x/.CODEX"`, "withheld", "could_not_run"} {
		if !strings.Contains(prompt, p) {
			t.Errorf("prompt does not name %s", p)
		}
	}
	if strings.Contains(prompt, `".claude/settings.json"`) {
		t.Error("prompt lists the files inside a withheld dir, not the dir")
	}
	// A tree with nothing withheld says nothing of it.
	plain, psha := gateRepo(t)
	m2 := newManager(t)
	approveBrief(t, m2, plain)
	j2, err := m2.StartGate(GateRequest{Kind: KindJudge, Repo: plain, SHA: psha, Model: "sonnet"})
	if err != nil {
		t.Fatal(err)
	}
	if prompt, err = m2.BuildPrompt(j2, "rules", nil, MemoryNotes{}); err != nil || strings.Contains(prompt, "withheld") {
		t.Fatalf("plain tree: %v\n%s", err, prompt)
	}
}
