package jobs

import (
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

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

// judge_start notes (information only, owner decision 2026-10-26) when any succeeded coder of the
// checklist shares the judge's family, as same_family
// is set: mixed coders do not hide it.
func TestJudgeFamilyNoteMixedCoders(t *testing.T) {
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
	insertCoder(t, m, repo, "claude-ok", store.StatusSucceeded, SizeLarge, store.RunnerClaude, "opus", "R01")
	insertCoder(t, m, repo, "codex-ok", store.StatusSucceeded, SizeLarge, store.RunnerCodex, "gpt-5.6-sol", "R01")
	for _, model := range []string{"sonnet", "gpt-5.6-sol"} {
		j := judge(model)
		share := "claude-ok"
		if model != "sonnet" {
			share = "codex-ok"
		}
		w, err := m.JudgeFamilyNote(j)
		if err != nil || !strings.Contains(w, "same_family") || !strings.Contains(w, share) {
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

// teeResult is what the wrapper's tee reads from a runner's stdout written as chunks.
func teeResult(runner string, chunks ...string) StreamResult {
	tee := &resultTee{w: io.Discard, runner: runner}
	if runner == store.RunnerCodex {
		tee.res.Runner = runner
	}
	for _, c := range chunks {
		_, _ = tee.Write([]byte(c))
	}
	tee.end()
	return tee.res
}

// claudeResultLine is a claude result event whose result is text.
func claudeResultLine(t *testing.T, text string) string {
	t.Helper()
	b, err := json.Marshal(map[string]any{"type": "result", "subtype": "success", "result": text})
	if err != nil {
		t.Fatal(err)
	}
	return string(b) + "\n"
}

// codexMessageLine is a codex agent_message item.
func codexMessageLine(t *testing.T, text string) string {
	t.Helper()
	b, err := json.Marshal(map[string]any{"type": "item.completed", "item": map[string]any{"type": "agent_message", "text": text}})
	if err != nil {
		t.Fatal(err)
	}
	return string(b) + "\n"
}

// A codex gate job's stdout carries exactly one terminal turn event with nothing after it: a second
// turn.completed or an event after it came from another process writing into the pipe.
func TestCodexGateResultEventsCounted(t *testing.T) {
	m := newManager(t)
	repo := briefRepo(t)
	j, err := m.StartGate(GateRequest{Kind: KindSlicing, Repo: repo, Model: "gpt-5.6-sol"})
	if err != nil {
		t.Fatal(err)
	}
	if err := m.Store.Claim(j.ID, "tok-"+j.ID, m.Now()); err != nil {
		t.Fatal(err)
	}
	j = get(t, m, j.ID)
	report := slicingReport(t, j.Gate).Text
	done := `{"type":"turn.completed","usage":{"input_tokens":1,"cached_input_tokens":0,"output_tokens":1}}` + "\n"
	start := `{"type":"thread.started","thread_id":"t"}` + "\n" + `{"type":"turn.started"}` + "\n"
	for name, tc := range map[string]struct {
		res  StreamResult
		want string
	}{
		"two turns completed": {teeResult(store.RunnerCodex, start, codexMessageLine(t, report), done, start,
			codexMessageLine(t, report), done), "2 terminal turn events"},
		"event after turn.completed": {teeResult(store.RunnerCodex, start, codexMessageLine(t, report), done,
			codexMessageLine(t, report)), "after the terminal turn event"},
	} {
		if !tc.res.Found {
			t.Fatalf("%s: stream not ended: %+v", name, tc.res)
		}
		if msg := gateStep(m.Store, j.ID, "tok-"+j.ID, tc.res, m.ChecklistLockDir(), time.Now()); !strings.Contains(msg, tc.want) {
			t.Errorf("%s: %q", name, msg)
		}
	}
	ok := teeResult(store.RunnerCodex, start, codexMessageLine(t, "working"), `{"type":"item.completed","item":{"type":"command_execution"}}`+"\n",
		codexMessageLine(t, report), done)
	if msg := gateStep(m.Store, j.ID, "tok-"+j.ID, ok, m.ChecklistLockDir(), time.Now()); msg != "" {
		t.Fatalf("one codex turn: %s", msg)
	}
	if r, _ := m.Store.SlicingByJob(j.ID, j.Attempt); r == nil || r.EnforcedBlind {
		t.Fatalf("codex result: %+v", r)
	}
}

// The slicing check is advisory (owner decision 2026-10-25): approve passes with no result and with a
// codex result only (enforced_blind false); a result's undecided unmapped quote, codex or not, still
// needs an owner decision.
func TestCodexSlicingDoesNotCountForApproval(t *testing.T) {
	m := newManager(t)
	repo := briefRepo(t)
	main := mainWorktree(repo)
	j, err := m.StartGate(GateRequest{Kind: KindSlicing, Repo: repo, Model: "gpt-5.6-sol"})
	if err != nil {
		t.Fatal(err)
	}
	if err := m.Store.Claim(j.ID, "tok-"+j.ID, m.Now()); err != nil {
		t.Fatal(err)
	}
	g := j.Gate
	if _, _, err := m.SlicingGate(main, g.ChecklistID, g.Version, g.ChecklistSHA256, true, nil, m.Now()); err != nil {
		t.Fatalf("approve without a slicing result: %v", err)
	}
	if _, err := m.Store.PutSlicing("tok-"+j.ID, store.SlicingResult{JobID: j.ID, Repo: g.Repo, ChecklistID: g.ChecklistID,
		Version: g.Version, ChecklistSHA256: g.ChecklistSHA256, Model: "gpt-5.6-sol", Runner: store.RunnerCodex, CreatedAt: m.Now(),
		Unmapped: []store.Quote{{Quote: "a quote no row covers"}}}); err != nil {
		t.Fatal(err)
	}
	if _, _, err := m.SlicingGate(main, g.ChecklistID, g.Version, g.ChecklistSHA256, true, nil, m.Now()); err == nil ||
		!strings.Contains(err.Error(), "without an owner decision") {
		t.Fatalf("approve with an undecided quote of a codex result: %v", err)
	}
	given := []UnmappedDecision{{Unmapped: "a quote no row covers", Quote: "out of scope"}}
	if _, _, err := m.SlicingGate(main, g.ChecklistID, g.Version, g.ChecklistSHA256, true, given, m.Now()); err != nil {
		t.Fatalf("approve on a codex slicing result only, quote decided: %v", err)
	}
}

// A gate job's stdout is JSON lines only: an unterminated prefix written into the pipe joins the next
// line (the runner's own result) into one that is not JSON, so the run's result is not trusted.
func TestGateNonJSONLineRejected(t *testing.T) {
	m := newManager(t)
	repo := briefRepo(t)
	j, err := m.StartGate(GateRequest{Kind: KindSlicing, Repo: repo, Model: "sonnet"})
	if err != nil {
		t.Fatal(err)
	}
	if err := m.Store.Claim(j.ID, "tok-"+j.ID, m.Now()); err != nil {
		t.Fatal(err)
	}
	j = get(t, m, j.ID)
	forged := slicingReport(t, j.Gate).Text
	res := teeResult(store.RunnerClaude, claudeResultLine(t, forged), `{"type":"x","pad":"`, claudeResultLine(t, "the real one"))
	if res.Results != 1 || res.Text != forged {
		t.Fatalf("setup: %d results, text %q", res.Results, res.Text)
	}
	if msg := gateStep(m.Store, j.ID, "tok-"+j.ID, res, m.ChecklistLockDir(), time.Now()); !strings.Contains(msg, "not JSON") {
		t.Fatalf("claude stream with a line that is not JSON: %q", msg)
	}
	cx := teeResult(store.RunnerCodex, "garbage\n", codexMessageLine(t, forged), `{"type":"turn.completed"}`+"\n")
	if msg := gateStep(m.Store, j.ID, "tok-"+j.ID, cx, m.ChecklistLockDir(), time.Now()); !strings.Contains(msg, "not JSON") {
		t.Fatalf("codex stream with a line that is not JSON: %q", msg)
	}
	if msg := gateStep(m.Store, j.ID, "tok-"+j.ID, teeResult(store.RunnerClaude, "\n", claudeResultLine(t, forged)), m.ChecklistLockDir(),
		time.Now()); msg != "" {
		t.Fatalf("an empty line is no forgery: %q", msg)
	}
}
