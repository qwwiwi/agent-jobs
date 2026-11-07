package main

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/qwwiwi/agent-jobs/internal/jobs"
)

// `agent-jobs restack` needs no agent: it creates the restacked branch from the squash-merged parent's
// target, prints it (--json: the branch_restack output) and fails with the reason on a refusal.
func TestRestackCLI(t *testing.T) {
	repo := t.TempDir()
	gitOut(t, repo, "init", "-q", "-b", "main")
	commit := func(file string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(repo, file), []byte(file+"\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		gitOut(t, repo, "add", "-A")
		gitOut(t, repo, "commit", "-q", "-m", "add "+file)
	}
	commit("base.txt")
	gitOut(t, repo, "checkout", "-q", "-b", "P")
	commit("p.txt")
	gitOut(t, repo, "checkout", "-q", "-b", "C")
	commit("c.txt")
	gitOut(t, repo, "checkout", "-q", "main")

	// Parent not merged yet: refused, nothing created.
	if err := cmdRestack([]string{"--repo", repo, "--child", "C", "--parent", "P"}); err == nil || !strings.Contains(err.Error(), "not contained") {
		t.Fatalf("unmerged parent: %v", err)
	}
	if out := gitOut(t, repo, "branch", "--list", "C-restacked"); out != "" {
		t.Fatalf("branch created on refusal: %q", out)
	}
	if err := cmdRestack([]string{"--repo", repo, "--child", "C"}); err == nil || !strings.Contains(err.Error(), "--parent") {
		t.Fatalf("missing --parent: %v", err)
	}

	gitOut(t, repo, "merge", "--squash", "P")
	gitOut(t, repo, "commit", "-q", "-m", "squash P")
	var res jobs.RestackResult
	raw := captureStdout(t, func() error {
		return cmdRestack([]string{"--repo", repo, "--child", "C", "--parent", "P", "--name", "C-on-main", "--json"})
	})
	if err := json.Unmarshal([]byte(raw), &res); err != nil {
		t.Fatalf("json: %v: %q", err, raw)
	}
	if res.Branch != "C-on-main" || res.Commits != 1 || res.Head != strings.TrimSpace(gitOut(t, repo, "rev-parse", "C-on-main")) {
		t.Fatalf("result: %+v", res)
	}
	text := captureStdout(t, func() error {
		return cmdRestack([]string{"--repo", repo, "--child", "C", "--parent", "P"})
	})
	if !strings.Contains(text, "C-restacked") || !strings.Contains(text, "1 commit") {
		t.Fatalf("text output: %q", text)
	}
}

// Cleanup warnings go to stderr in the text output; stdout keeps only the summary line.
func TestPrintRestackWarnings(t *testing.T) {
	res := jobs.RestackResult{Branch: "C-restacked", Head: "abc123", Commits: 2, Warnings: []string{"remove temporary directory: boom"}}
	var out, errb bytes.Buffer
	printRestack(&out, &errb, res, "C", "main")
	if got := out.String(); got != "C-restacked abc123: 2 commit(s) of C on top of main\n" {
		t.Fatalf("stdout: %q", got)
	}
	if got := errb.String(); got != "warning: remove temporary directory: boom\n" {
		t.Fatalf("stderr: %q", got)
	}
}
