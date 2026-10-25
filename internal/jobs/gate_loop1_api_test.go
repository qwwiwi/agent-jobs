package jobs

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/qwwiwi/agent-jobs/internal/store"
)

// A gate job's work dir with an agent-context entry at any depth is refused before the runner starts.
func TestCheckGateWork(t *testing.T) {
	for _, rel := range []string{".claude/settings.json", "a/b/CLAUDE.md", "x/claude.local.md", "AGENTS.md", "d/.codex/c.toml",
		".agents/skills/s", "sub/.git"} {
		work := t.TempDir()
		p := filepath.Join(work, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, nil, 0o600); err != nil {
			t.Fatal(err)
		}
		if err := checkGateWork(work); err == nil || !strings.Contains(err.Error(), "not launched") {
			t.Errorf("%s: %v", rel, err)
		}
	}
	work := t.TempDir()
	if err := os.MkdirAll(filepath.Join(work, "pkg", "claude"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := checkGateWork(work); err != nil {
		t.Errorf("clean work dir: %v", err)
	}
}

// slicingReport is a final message carrying a valid slicing block for g.
func slicingReport(t *testing.T, g *store.GateInput) StreamResult {
	t.Helper()
	b, err := json.Marshal(slicingBlock(g))
	if err != nil {
		t.Fatal(err)
	}
	return StreamResult{Text: "## Findings / Changes\n```json\n" + string(b) + "\n```\n", Results: 1}
}

// A claude gate result is stored with enforced_blind true, a codex one with false (no OS confinement
// of its reads, ticket 684); a slicing summary with any codex result is not blind. Without the
// checklist lock dir a slicing result is not stored.
func TestGateEnforcedBlindAndLockDir(t *testing.T) {
	m := newManager(t)
	repo := briefRepo(t)
	main := mainWorktree(repo)
	start := func(model string) *store.Job {
		t.Helper()
		j, err := m.StartGate(GateRequest{Kind: KindSlicing, Repo: repo, Model: model})
		if err != nil {
			t.Fatal(err)
		}
		if err := m.Store.Claim(j.ID, "tok-"+j.ID, m.Now()); err != nil {
			t.Fatal(err)
		}
		return get(t, m, j.ID)
	}
	cl := start("sonnet")
	if msg := gateStep(m.Store, cl.ID, "tok-"+cl.ID, slicingReport(t, cl.Gate), "", time.Now()); !strings.Contains(msg, "checklist lock dir") {
		t.Fatalf("no lock dir: %q", msg)
	}
	if msg := gateStep(m.Store, cl.ID, "tok-"+cl.ID, slicingReport(t, cl.Gate), m.ChecklistLockDir(), time.Now()); msg != "" {
		t.Fatal(msg)
	}
	if r, _ := m.Store.SlicingByJob(cl.ID, cl.Attempt); r == nil || !r.EnforcedBlind || r.Attempt != 1 {
		t.Fatalf("claude result: %+v", r)
	}
	sv, err := m.SlicingOf(main, cl.Gate.ChecklistID, cl.Gate.Version, cl.Gate.ChecklistSHA256)
	if err != nil || !sv.Summary.EnforcedBlind || sv.Summary.Results != 1 {
		t.Fatalf("summary: %+v %v", sv, err)
	}
	cx := start("gpt-5.6-sol")
	if cx.Runner != store.RunnerCodex {
		t.Fatalf("runner %q", cx.Runner)
	}
	if msg := gateStep(m.Store, cx.ID, "tok-"+cx.ID, slicingReport(t, cx.Gate), m.ChecklistLockDir(), time.Now()); msg != "" {
		t.Fatal(msg)
	}
	if r, _ := m.Store.SlicingByJob(cx.ID, cx.Attempt); r == nil || r.EnforcedBlind {
		t.Fatalf("codex result: %+v", r)
	}
	if sv, _ = m.SlicingOf(main, cx.Gate.ChecklistID, cx.Gate.Version, cx.Gate.ChecklistSHA256); sv.Summary.EnforcedBlind || sv.Summary.Results != 2 {
		t.Fatalf("summary with a codex result: %+v", sv.Summary)
	}
	// An empty final message is no result, whatever report.md says; nor is a claude stream with a second
	// result event (a line another process wrote into the pipe).
	if msg := gateStep(m.Store, cl.ID, "tok-"+cl.ID, StreamResult{}, m.ChecklistLockDir(), time.Now()); !strings.Contains(msg, "final message is empty") {
		t.Fatalf("empty final message: %q", msg)
	}
	twice := slicingReport(t, cl.Gate)
	twice.Results = 2
	if msg := gateStep(m.Store, cl.ID, "tok-"+cl.ID, twice, m.ChecklistLockDir(), time.Now()); !strings.Contains(msg, "2 result events") {
		t.Fatalf("two result events: %q", msg)
	}
}

// judge_start warns when the judge runs on the family of a coder of the checklist, and not when no
// coder shares it or there is none.
func TestJudgeFamilyWarning(t *testing.T) {
	m := newManager(t)
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
	if w, err := m.JudgeFamilyWarning(judge("sonnet")); err != nil || w != "" {
		t.Fatalf("no coders: %q %v", w, err)
	}
	coder := enqueue(t, m, "build it", func(r *StartRequest) { r.Kind, r.RepoPath, r.Covers = "fix", repo, []string{"R01"} })
	if err := m.Cancel(coder.ID); err != nil {
		t.Fatal(err)
	}
	w, err := m.JudgeFamilyWarning(judge("sonnet"))
	if err != nil || !strings.Contains(w, "same_family") || !strings.Contains(w, coder.ID) {
		t.Fatalf("claude judge of claude coders: %q %v", w, err)
	}
	if w, err := m.JudgeFamilyWarning(judge("gpt-5.6-sol")); err != nil || w != "" {
		t.Fatalf("codex judge of claude coders: %q %v", w, err)
	}
}

// The run-wrapper takes the checklist lock dir as an absolute path only.
func TestWrapperChecklistLockDirOption(t *testing.T) {
	if err := (WrapperOptions{ChecklistLockDir: "rel/locks"}).validate(); err == nil {
		t.Fatal("relative lock dir accepted")
	}
	if err := (WrapperOptions{ChecklistLockDir: "/abs/locks"}).validate(); err != nil {
		t.Fatal(err)
	}
}

// A codex gate job launched under the read-only codex sandbox (the launch record) stores its result with
// enforced_blind true (owner decision 2026-10-25); one under worktree-full-access stays not blind.
func TestGateCodexReadOnlySandboxIsBlind(t *testing.T) {
	m := newManager(t)
	repo := briefRepo(t)
	for _, tc := range []struct {
		sandbox string
		blind   bool
	}{{CodexSandboxReadOnly, true}, {CodexSandboxFullAccess, false}} {
		j, err := m.StartGate(GateRequest{Kind: KindSlicing, Repo: repo, Model: "gpt-5.6-sol"})
		if err != nil {
			t.Fatal(err)
		}
		tok := "tok-" + j.ID
		if err := m.Store.Claim(j.ID, tok, m.Now()); err != nil {
			t.Fatal(err)
		}
		cx := get(t, m, j.ID)
		if err := m.Store.SetLaunchAuthorization(cx.ID, tok, cx.Attempt, store.LaunchAuthorization{CodexSandbox: tc.sandbox}); err != nil {
			t.Fatal(err)
		}
		if msg := gateStep(m.Store, cx.ID, tok, slicingReport(t, cx.Gate), m.ChecklistLockDir(), time.Now()); msg != "" {
			t.Fatal(msg)
		}
		if r, _ := m.Store.SlicingByJob(cx.ID, cx.Attempt); r == nil || r.EnforcedBlind != tc.blind {
			t.Fatalf("codex result under %s: %+v", tc.sandbox, r)
		}
	}
}
