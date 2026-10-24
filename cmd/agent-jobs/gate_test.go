package main

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/qwwiwi/agent-jobs/internal/checklist"
	"github.com/qwwiwi/agent-jobs/internal/jobs"
	"github.com/qwwiwi/agent-jobs/internal/mcp"
	"github.com/qwwiwi/agent-jobs/internal/store"
)

// `agent-jobs gate slicing` and `gate verdict` print slicing_get / judge_get as JSON.
func TestGateCLI(t *testing.T) {
	t.Setenv("AGENT_JOBS_HOME", t.TempDir())
	repo := t.TempDir()
	if out, err := exec.Command("git", "-C", repo, "init", "-q").CombinedOutput(); err != nil {
		t.Fatalf("git init: %v %s", err, out)
	}
	repo, _ = filepath.EvalSymlinks(repo)
	if err := os.Mkdir(filepath.Join(repo, "brief"), 0o755); err != nil {
		t.Fatal(err)
	}
	brief := "# Brief\n\nI want a login page.\n"
	list := "# Checklist\n\n- id: demo\n- brief: brief/BRIEF.md\n- version: 2\n- approved: no\n\n" + checklist.TableHeader +
		"\n|---|---|---|---|\n| R01 | a login page | live | brief |\n"
	if err := os.WriteFile(checklist.BriefPath(repo), []byte(brief), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(checklist.ChecklistPath(repo), []byte(list), 0o644); err != nil {
		t.Fatal(err)
	}
	c, err := checklist.Parse(list)
	if err != nil {
		t.Fatal(err)
	}
	m, err := jobs.Open("test")
	if err != nil {
		t.Fatal(err)
	}
	if err := m.Store.Insert(&store.Job{ID: "s1", Agent: "test", Title: "t", Kind: jobs.KindSlicing, Brief: "b", Status: store.StatusRunning,
		Model: "opus", MaxTurns: 1, MaxBudgetUSD: 1, TimeoutMinutes: 1, CreatedAt: time.Now(), LaunchToken: "tok"}); err != nil {
		t.Fatal(err)
	}
	if _, err := m.Store.PutSlicing("tok", store.SlicingResult{JobID: "s1", Repo: repo, ChecklistID: "demo", Version: 2,
		ChecklistSHA256: checklist.Hash(c.Rows, []byte(brief)), Unmapped: []store.Quote{{Quote: "I want a login page."}}, CreatedAt: time.Now()}); err != nil {
		t.Fatal(err)
	}
	_ = m.Close()

	var sg mcp.SlicingGetOutput
	out := captureStdout(t, func() error { return cmdGate([]string{"slicing", "--agent", "test", "--repo", repo}) })
	if err := json.Unmarshal([]byte(out), &sg); err != nil || sg.ChecklistID != "demo" || sg.Summary == nil || !sg.Summary.Current ||
		sg.Summary.Undecided != 1 || len(sg.Undecided) != 1 {
		t.Fatalf("gate slicing: %s %v", out, err)
	}
	var vv jobs.VerdictView
	out = captureStdout(t, func() error {
		return cmdGate([]string{"verdict", "--agent", "test", "--repo", repo, "--sha", strings.Repeat("a", 40)})
	})
	if err := json.Unmarshal([]byte(out), &vv); err != nil || vv.Verdict != nil {
		t.Fatalf("gate verdict: %s %v", out, err)
	}
	if err := cmdGate([]string{"judge"}); err == nil || !strings.Contains(err.Error(), "usage") {
		t.Fatalf("unknown subcommand: %v", err)
	}
	if err := cmdGate([]string{"slicing", "--agent", "test", "--repo", repo, "--sha", "x"}); err == nil {
		t.Fatal("gate slicing took --sha")
	}
}
