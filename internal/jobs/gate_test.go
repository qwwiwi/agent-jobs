package jobs

import (
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/qwwiwi/agent-jobs/internal/brain"
	"github.com/qwwiwi/agent-jobs/internal/store"
)

// gateRepo is briefRepo with code committed: main.go and a script at the head commit, plus the brief
// files committed too (the judge's copy leaves them out).
func gateRepo(t *testing.T) (string, string) {
	t.Helper()
	repo := briefRepo(t)
	if err := os.WriteFile(filepath.Join(repo, "main.go"), []byte("package main // CODE_MARKER\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repo, "run.sh"), []byte("#!/bin/sh\necho run\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	// A link inside the tree is kept, one that leaves it is not.
	if err := os.Symlink("main.go", filepath.Join(repo, "inside.link")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("../../outside", filepath.Join(repo, "outside.link")); err != nil {
		t.Fatal(err)
	}
	gitT(t, repo, "add", ".")
	gitT(t, repo, "commit", "-qm", "code")
	return repo, gitT(t, repo, "rev-parse", "HEAD")
}

// gateReport writes report as the fake claude's report for the next gate job (FAKE_GATE_REPORT).
func gateReport(t *testing.T, block any) string {
	t.Helper()
	b, err := json.MarshalIndent(block, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	report := "## Summary\nchecked\n\n## Findings / Changes\n\n```json\n" + string(b) + "\n```\n\n## Evidence\n-\n\n## Not done / uncertain\nnothing\n"
	p := filepath.Join(t.TempDir(), "report.md")
	if err := os.WriteFile(p, []byte(report), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("FAKE_GATE_REPORT", p)
	return report
}

func ref(g *store.GateInput) map[string]any {
	return map[string]any{"id": g.ChecklistID, "version": g.Version, "sha256": g.ChecklistSHA256}
}

func slicingBlock(g *store.GateInput, unmapped ...string) map[string]any {
	u := []map[string]any{}
	for _, q := range unmapped {
		u = append(u, map[string]any{"quote": q, "why": "no row"})
	}
	return map[string]any{"schema": SlicingSchema, "checklist": ref(g), "unmapped": u,
		"half":     []map[string]any{{"quote": "a login page", "rows": []string{"R01"}, "missing": "the pipe"}},
		"invented": []map[string]any{{"row": "R02", "why": "not in the brief"}}}
}

func judgeBlock(g *store.GateInput) map[string]any {
	var reqs []map[string]any
	for _, id := range g.Live {
		reqs = append(reqs, map[string]any{"id": id, "verdict": "met", "how": "ran", "evidence": []string{"main.go:1", "TestX"}})
	}
	return map[string]any{"schema": JudgeSchema, "checklist": ref(g), "sha": g.SHA,
		"ran": []map[string]any{{"cmd": "go test ./...", "result": "ok"}}, "could_not_run": []string{},
		"requirements": reqs, "unmapped_brief": []string{}, "extra_scope": []string{}, "suspect_tests": []string{}}
}

// The slicing check end to end: StartGate reads the brief files (no orchestrator brief); the prompt
// carries BRIEF.md and the rows, never the brief column, cards or memory notes; the job runs in an
// empty read-only dir that never reaches the repository (guard --read-only / --no-read, edit tools
// denied); the wrapper validates the JSON block and stores the lists for the checklist id, version
// and hash.
func TestSlicingJobEndToEnd(t *testing.T) {
	m := newManager(t)
	repo := briefRepo(t)
	j, err := m.StartGate(GateRequest{Kind: KindSlicing, Repo: repo, Model: "sonnet"})
	if err != nil {
		t.Fatal(err)
	}
	g := j.Gate
	if j.RepoPath != "" || j.Kind != KindSlicing || g == nil || g.ChecklistID != "demo" || g.Version != 2 || g.SHA != "" ||
		!strings.Contains(g.Brief, "BRIEF_TEXT_MARKER") || len(g.Rows) != 3 || g.Rows[0].Basis != "BASIS_MARKER" {
		t.Fatalf("gate job: %+v %+v", j, g)
	}
	gateReport(t, slicingBlock(g, "I want a login  page."))
	// A brain is configured: a gate job still gets no memory notes, and none is looked up for it.
	f := &brain.Fake{Notes: []brain.Note{{Path: "70-runbooks/n.md", Scope: "runbooks", Score: 0.9, Snippet: "NOTE_MARKER"}}}
	m.Brain = f
	j = runToEnd(t, m, j.ID)
	if j.Status != store.StatusSucceeded {
		t.Fatalf("status %s: %s", j.Status, j.Error)
	}
	dir := m.JobDir(j.ID)
	prompt, _ := os.ReadFile(filepath.Join(dir, "prompt.md"))
	for _, s := range []string{"BRIEF_TEXT_MARKER", "BASIS_MARKER", "SECOND_ROW_MARKER", "Checklist (kind: slicing)", SlicingSchema,
		"OWNER_RULE_MARKER", "## Not done / uncertain", "nick-vels/skills", g.ChecklistSHA256} {
		if !strings.Contains(string(prompt), s) {
			t.Fatalf("prompt.md lacks %q:\n%s", s, prompt)
		}
	}
	if strings.Contains(string(prompt), "Gate job (kind") || strings.Contains(string(prompt), "NOTE_MARKER") || len(f.Queries) != 0 {
		t.Fatalf("prompt.md carries the brief column or memory notes (queries %q):\n%s", f.Queries, prompt)
	}
	if seen, _ := os.ReadFile(filepath.Join(dir, "work-seen.txt")); strings.TrimSpace(string(seen)) != "" {
		t.Fatalf("slicing work dir is not empty: %q", seen)
	}
	settings, _ := os.ReadFile(filepath.Join(dir, "settings.json"))
	for _, s := range []string{"--read-only", "--no-read '" + repo + "'", `"Edit"`, `"WebFetch"`} {
		if !strings.Contains(string(settings), s) {
			t.Fatalf("settings.json lacks %q:\n%s", s, settings)
		}
	}
	r, err := m.Store.SlicingByJob(j.ID)
	if err != nil || r == nil {
		t.Fatalf("no slicing result: %v", err)
	}
	if r.Version != 2 || r.ChecklistSHA256 != g.ChecklistSHA256 || len(r.Unmapped) != 1 || r.Unmapped[0].Quote != "I want a login page." ||
		len(r.Half) != 1 || len(r.Invented) != 1 || r.Repo != mainWorktree(repo) {
		t.Fatalf("slicing result: %+v", r)
	}
	sv, err := m.SlicingOf(mainWorktree(repo), "demo", 2, g.ChecklistSHA256)
	if err != nil || sv == nil || !sv.Summary.Current || sv.Summary.Unmapped != 1 || sv.Summary.Undecided != 1 {
		t.Fatalf("SlicingOf: %+v %v", sv, err)
	}
}

// An invalid slicing block fails the job and stores nothing: a quote not in BRIEF.md, a row not in the
// checklist, an unknown field, another checklist version (a stale result), no block.
func TestSlicingInvalidResultFailsJob(t *testing.T) {
	m := newManager(t)
	repo := briefRepo(t)
	for name, mod := range map[string]func(b map[string]any){
		"invented quote": func(b map[string]any) { b["unmapped"] = []map[string]any{{"quote": "I want a pony"}} },
		"unknown row":    func(b map[string]any) { b["invented"] = []map[string]any{{"row": "R99"}} },
		"unknown field":  func(b map[string]any) { b["verdict"] = "fine" },
		"stale version":  func(b map[string]any) { b["checklist"].(map[string]any)["version"] = 1 },
		"no block":       func(b map[string]any) { b["schema"] = "something/else" },
		"null list":      func(b map[string]any) { b["half"] = nil },
	} {
		j, err := m.StartGate(GateRequest{Kind: KindSlicing, Repo: repo, Model: "sonnet"})
		if err != nil {
			t.Fatal(err)
		}
		b := slicingBlock(j.Gate)
		mod(b)
		gateReport(t, b)
		j = runToEnd(t, m, j.ID)
		if j.Status != store.StatusFailed || !strings.Contains(j.Error, "gate result rejected") {
			t.Fatalf("%s: status %s error %q", name, j.Status, j.Error)
		}
		if r, _ := m.Store.SlicingByJob(j.ID); r != nil {
			t.Fatalf("%s: stored %+v", name, r)
		}
	}
}

// A succeeded gate job without a stored result fails (fail closed), whatever its report says.
func TestGateVerdictFailsClosed(t *testing.T) {
	m := newManager(t)
	j := &store.Job{ID: "g1", Kind: KindJudge, Gate: &store.GateInput{}}
	if st, msg := m.gateVerdict(j, store.StatusSucceeded, "", StreamResult{}); st != store.StatusFailed || msg != NoteGateNoResult {
		t.Fatalf("no stored verdict: %s %q", st, msg)
	}
	if st, msg := m.gateVerdict(j, store.StatusSucceeded, "", StreamResult{GateFailure: "gate result rejected: x"}); st != store.StatusFailed || msg != "gate result rejected: x" {
		t.Fatalf("rejected: %s %q", st, msg)
	}
	other := &store.Job{ID: "g2", Kind: "review"}
	if st, _ := m.gateVerdict(other, store.StatusSucceeded, "", StreamResult{}); st != store.StatusSucceeded {
		t.Fatalf("a review job was failed: %s", st)
	}
}

// The judge end to end: it needs the approved checklist; its work dir is the files at the sha only (no
// .git, no brief files, no link out of the tree); the verdict is stored with the judge's family and the
// coders' (feature jobs with covers of the checklist), same_family flagged.
func TestJudgeJobEndToEnd(t *testing.T) {
	m := newManager(t)
	repo, sha := gateRepo(t)
	if _, err := m.StartGate(GateRequest{Kind: KindJudge, Repo: repo, SHA: sha, Model: "sonnet"}); err == nil || !strings.Contains(err.Error(), "not approved") {
		t.Fatalf("judge of an unapproved checklist: %v", err)
	}
	approveBrief(t, m, repo)
	// The coders: a claude fix job with covers of this checklist, and a codex one (inserted as stored).
	coder := enqueue(t, m, "build it", func(r *StartRequest) { r.Kind, r.RepoPath, r.Covers = "fix", repo, []string{"R01"} })
	if err := m.Cancel(coder.ID); err != nil {
		t.Fatal(err)
	}
	cx := &store.Job{ID: "cx1", Agent: m.Agent, Title: "codex coder", Kind: "general", Brief: "b", RepoPath: mainWorktree(repo),
		Status: store.StatusSucceeded, Model: "gpt-5.6-sol", Runner: store.RunnerCodex, CreatedAt: time.Now(),
		CoverContract: &store.CoverContract{ChecklistID: "demo", Version: 2}, Covers: []string{"R02"}}
	if err := m.Store.Insert(cx); err != nil {
		t.Fatal(err)
	}
	j, err := m.StartGate(GateRequest{Kind: KindJudge, Repo: repo, SHA: sha, Model: "sonnet"})
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(j.Gate.Live, []string{"R01", "R02", "R03i"}) || j.Gate.SHA != sha {
		t.Fatalf("judge input: %+v", j.Gate)
	}
	gateReport(t, judgeBlock(j.Gate))
	j = runToEnd(t, m, j.ID)
	if j.Status != store.StatusSucceeded {
		t.Fatalf("status %s: %s", j.Status, j.Error)
	}
	dir := m.JobDir(j.ID)
	seen, _ := os.ReadFile(filepath.Join(dir, "work-seen.txt"))
	if string(seen) != "./inside.link\n./main.go\n./run.sh\n./tracked.txt\n" {
		t.Fatalf("judge work dir:\n%s", seen)
	}
	if fi, err := os.Stat(filepath.Join(dir, "work", "run.sh")); err != nil || fi.Mode().Perm()&0o100 == 0 {
		t.Fatalf("run.sh lost its executable bit: %v %v", fi, err)
	}
	prompt, _ := os.ReadFile(filepath.Join(dir, "prompt.md"))
	for _, s := range []string{"BRIEF_TEXT_MARKER", "BASIS_MARKER", "Checklist (kind: judge)", JudgeSchema, sha, "R01, R02, R03i"} {
		if !strings.Contains(string(prompt), s) {
			t.Fatalf("prompt.md lacks %q", s)
		}
	}
	if strings.Contains(string(prompt), "build it") {
		t.Fatal("a coder job's brief reached the judge's prompt")
	}
	settings, _ := os.ReadFile(filepath.Join(dir, "settings.json"))
	for _, s := range []string{"--read-only '" + filepath.Join(dir, "work") + "'", "--no-read '" + repo + "'", "--no-read '" + filepath.Join(repo, ".git") + "'"} {
		if !strings.Contains(string(settings), s) {
			t.Fatalf("settings.json lacks %q:\n%s", s, settings)
		}
	}
	v, err := m.Store.VerdictByJob(j.ID)
	if err != nil || v == nil {
		t.Fatalf("no verdict: %v", err)
	}
	if v.SHA != sha || v.Version != 2 || v.JudgeFamily != "anthropic" || v.JudgeModel != "sonnet" || !v.SameFamily || v.FamilyDiffers ||
		!slices.Equal(v.CoderFamilies, []string{"anthropic", "openai"}) || !slices.Equal(v.CoderJobs, []string{coder.ID, "cx1"}) ||
		v.Counts["met"] != 3 || v.ManifestCheck != nil {
		t.Fatalf("verdict: %+v", v)
	}
	vv, err := m.LatestVerdictOf(mainWorktree(repo), "demo", sha)
	if err != nil || vv.Verdict == nil || !vv.Current {
		t.Fatalf("LatestVerdictOf: %+v %v", vv, err)
	}
}

// The judge's verdict rules: every live requirement exactly once and nothing else, the input's
// checklist and sha, known values, evidence for met / partial / placeholder, ran needs a ran list.
func TestValidateJudgeRules(t *testing.T) {
	g := &store.GateInput{ChecklistID: "demo", Version: 2, ChecklistSHA256: strings.Repeat("a", 64), SHA: strings.Repeat("b", 40),
		Live: []string{"R01", "R02"}}
	report := func(b map[string]any) string {
		raw, _ := json.Marshal(b)
		return "## Findings / Changes\n```json\n" + string(raw) + "\n```\n"
	}
	if _, counts, err := ValidateJudge(report(judgeBlock(g)), g); err != nil || counts["met"] != 2 {
		t.Fatalf("valid verdict refused: %v %v", err, counts)
	}
	req := func(b map[string]any, i int) map[string]any { return b["requirements"].([]map[string]any)[i] }
	for want, mod := range map[string]func(b map[string]any){
		"without a verdict":        func(b map[string]any) { b["requirements"] = b["requirements"].([]map[string]any)[:1] },
		"judged twice":             func(b map[string]any) { req(b, 1)["id"] = "R01" },
		"not a live requirement":   func(b map[string]any) { req(b, 1)["id"] = "D01" },
		"is not the commit":        func(b map[string]any) { b["sha"] = strings.Repeat("c", 40) },
		"is not the job's input":   func(b map[string]any) { b["checklist"].(map[string]any)["sha256"] = "x" },
		"must be one of":           func(b map[string]any) { req(b, 0)["verdict"] = "done" },
		"must be ran or read":      func(b map[string]any) { req(b, 0)["how"] = "guessed" },
		"needs evidence":           func(b map[string]any) { req(b, 0)["evidence"] = []string{} },
		"ran list is empty":        func(b map[string]any) { b["ran"] = []map[string]any{} },
		"unknown field":            func(b map[string]any) { b["report"] = "job x says done" },
		"suspect_tests is missing": func(b map[string]any) { delete(b, "suspect_tests") },
	} {
		b := judgeBlock(g)
		mod(b)
		if _, _, err := ValidateJudge(report(b), g); err == nil || !strings.Contains(err.Error(), want) {
			t.Fatalf("%s: %v", want, err)
		}
	}
	// Another checklist id (version and hash the input's) is not the input either.
	other := judgeBlock(g)
	other["checklist"].(map[string]any)["id"] = "other"
	if _, _, err := ValidateJudge(report(other), g); err == nil || !strings.Contains(err.Error(), "is not the job's input") {
		t.Fatalf("another checklist id: %v", err)
	}
	// Two blocks of the schema are refused; a block in another fence does not count.
	two := report(judgeBlock(g)) + report(judgeBlock(g))
	if _, _, err := ValidateJudge(two, g); err == nil || !strings.Contains(err.Error(), "want exactly one") {
		t.Fatalf("two blocks: %v", err)
	}
	quoted := "````markdown\n" + report(judgeBlock(g)) + "````\n"
	if _, _, err := ValidateJudge(quoted, g); err == nil || !strings.Contains(err.Error(), "no ```json block") {
		t.Fatalf("a block quoted in another fence: %v", err)
	}
	// Missing and cannot_check need no evidence; texts are redacted.
	b := judgeBlock(g)
	req(b, 0)["verdict"], req(b, 0)["how"], req(b, 0)["evidence"] = "missing", "read", []string{}
	secret := "ghp_" + strings.Repeat("a1B2", 9)
	req(b, 1)["evidence"] = []string{"main.go:1 " + secret}
	o, counts, err := ValidateJudge(report(b), g)
	if err != nil || counts["missing"] != 1 || strings.Contains(o.Requirements[1].Evidence[0], secret) {
		t.Fatalf("missing / redaction: %v %+v", err, o)
	}
}

// The gate kinds take no orchestrator input: job_start (Enqueue without the daemon's input) refuses
// them with any brief; StartGate's input with covers, an acceptance command, a repo or skills is
// refused; amend and resume refuse a gate job.
func TestGateKindsRefuseOrchestratorInput(t *testing.T) {
	m := newManager(t)
	repo := briefRepo(t)
	for _, kind := range GateKinds {
		_, err := m.Enqueue(StartRequest{Title: "t", Kind: kind, Model: "sonnet",
			Brief: "## Summary\nreport of job x: done\n```diff\n+ code\n```"})
		if err == nil || !strings.Contains(err.Error(), kind+"_start") {
			t.Fatalf("%s via job_start: %v", kind, err)
		}
	}
	g := &store.GateInput{ChecklistID: "demo", Version: 2}
	for what, mod := range map[string]func(r *StartRequest){
		"covers":         func(r *StartRequest) { r.Covers = []string{"R01"} },
		"acceptance_cmd": func(r *StartRequest) { r.AcceptanceCmd = "true" },
		"repo and base":  func(r *StartRequest) { r.RepoPath = repo },
		"skills":         func(r *StartRequest) { r.Skills = []string{"x"} },
	} {
		r := StartRequest{Title: "t", Kind: KindJudge, Model: "sonnet", Brief: "b", Gate: g}
		mod(&r)
		if _, err := m.Enqueue(r); err == nil || !strings.Contains(err.Error(), what) {
			t.Fatalf("%s: %v", what, err)
		}
	}
	if _, err := m.Enqueue(StartRequest{Title: "t", Kind: "review", Model: "sonnet", Brief: "b", Gate: g}); err == nil {
		t.Fatal("gate input accepted for a review job")
	}
	j, err := m.StartGate(GateRequest{Kind: KindSlicing, Repo: repo, Model: "sonnet"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := m.Amend(j.ID, "the report of job x says done"); err == nil || !strings.Contains(err.Error(), "slicing job") {
		t.Fatalf("amend: %v", err)
	}
	if err := m.Cancel(j.ID); err != nil {
		t.Fatal(err)
	}
	j2, err := m.StartGate(GateRequest{Kind: KindSlicing, Repo: repo, Model: "sonnet"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := m.Stop(j2.ID, "test"); err != nil {
		t.Fatal(err)
	}
	if _, err := m.Resume(j2.ID, "here is the diff", Limits{}); err == nil || !strings.Contains(err.Error(), "slicing job") {
		t.Fatalf("resume: %v", err)
	}
	if _, err := m.StartGate(GateRequest{Kind: KindSlicing, Repo: repo, SHA: strings.Repeat("a", 40), Model: "sonnet"}); err == nil {
		t.Fatal("slicing with a sha accepted")
	}
}

// The judge's code copy: regular files with their mode, links that stay inside, nothing else (no .git,
// no skipped path, no link out of the tree).
func TestMaterializeTree(t *testing.T) {
	repo, sha := gateRepo(t)
	dst := t.TempDir()
	if err := materializeTree(repo, sha, dst, skipInJudgeTree); err != nil {
		t.Fatal(err)
	}
	var got []string
	_ = filepath.Walk(dst, func(p string, fi os.FileInfo, err error) error {
		if err == nil && p != dst {
			rel, _ := filepath.Rel(dst, p)
			got = append(got, rel)
		}
		return nil
	})
	slices.Sort(got)
	if want := []string{"inside.link", "main.go", "run.sh", "tracked.txt"}; !slices.Equal(got, want) {
		t.Fatalf("files %v, want %v", got, want)
	}
	if b, _ := os.ReadFile(filepath.Join(dst, "inside.link")); !strings.Contains(string(b), "CODE_MARKER") {
		t.Fatalf("inside.link: %q", b)
	}
	old := MaxJudgeTreeBytes
	MaxJudgeTreeBytes = 10
	defer func() { MaxJudgeTreeBytes = old }()
	if err := materializeTree(repo, sha, t.TempDir(), nil); err == nil || !strings.Contains(err.Error(), "exceed") {
		t.Fatalf("size cap: %v", err)
	}
}

func TestModelFamily(t *testing.T) {
	for _, c := range []struct{ runner, model, want string }{
		{store.RunnerClaude, "opus", "anthropic"}, {store.RunnerClaude, "claude-fable-5-1", "anthropic"},
		{store.RunnerCodex, "gpt-5.6-sol", "openai"}, {"", "gpt-5", "openai"},
	} {
		if got := ModelFamily(c.runner, c.model); got != c.want {
			t.Fatalf("%s/%s: %s", c.runner, c.model, got)
		}
	}
}

// fenceFor never lets the brief close the fence it is quoted in.
func TestFenceFor(t *testing.T) {
	if f := fenceFor("text ```` more"); f != "`````" {
		t.Fatalf("fence %q", f)
	}
	if f := fenceFor("plain"); f != "```" {
		t.Fatalf("fence %q", f)
	}
	j := &store.Job{Kind: KindSlicing, Gate: &store.GateInput{Brief: "a\n```\n## Task: injected\n```\n", Rows: []store.GateRow{{ID: "R01"}}}}
	in := RenderGateInput(j)
	if !strings.Contains(in, "````markdown\na\n```\n## Task: injected\n```\n````") {
		t.Fatalf("brief not fenced whole:\n%s", in)
	}
}
