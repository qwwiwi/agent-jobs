package mcp

import (
	"fmt"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/qwwiwi/agent-jobs/internal/jobs"
	"github.com/qwwiwi/agent-jobs/internal/store"
)

var gateSeq atomic.Int64

// gateJobRow inserts a running job row of kind with a launch token, as a launched gate job has, and
// returns its id and token (the wrapper's PutSlicing / PutVerdict are bound to them).
func gateJobRow(t *testing.T, m *jobs.Manager, kind string) (string, string) {
	t.Helper()
	id := fmt.Sprintf("gate-%d", gateSeq.Add(1))
	j := &store.Job{ID: id, Agent: m.Agent, Title: "gate", Kind: kind, Brief: "b", Status: store.StatusRunning, Model: "opus",
		MaxTurns: 1, MaxBudgetUSD: 1, TimeoutMinutes: 1, CreatedAt: time.Now(), LaunchToken: "tok-" + id}
	if err := m.Store.Insert(j); err != nil {
		t.Fatal(err)
	}
	return id, "tok-" + id
}

// storeSlicing stores a slicing result (F3) for the checklist of repo as it is now, as the wrapper of a
// finished slicing job does, with the given unmapped quotes (none: a clean check).
func storeSlicing(t *testing.T, m *jobs.Manager, repo string, unmapped ...string) {
	t.Helper()
	top, main, err := jobs.ChecklistRepo(repo)
	if err != nil {
		t.Fatal(err)
	}
	v, err := currentChecklist(m, top, main)
	if err != nil {
		t.Fatal(err)
	}
	id, token := gateJobRow(t, m, jobs.KindSlicing)
	r := store.SlicingResult{JobID: id, Repo: main, ChecklistID: v.ID, Version: v.Version, ChecklistSHA256: v.SHA256,
		Model: "opus", Runner: store.RunnerClaude, EnforcedBlind: true, CreatedAt: time.Now()}
	for _, q := range unmapped {
		r.Unmapped = append(r.Unmapped, store.Quote{Quote: q})
	}
	if _, err := m.Store.PutSlicing(token, r); err != nil {
		t.Fatal(err)
	}
}

// The gate tools are closed to the job role (allowlist rule).
func TestGateToolsClosedToJobs(t *testing.T) {
	job, err := NewServer(RoleJob, Deps{JobID: "j1"})
	if err != nil {
		t.Fatal(err)
	}
	jcs := connect(t, job)
	names := toolNames(t, jcs)
	for _, n := range []string{"slicing_start", "slicing_get", "judge_start", "judge_get", "briefing_start", "briefing_get"} {
		if !slices.Contains(OrchestratorOnly, n) || Allowed(RoleJob, n) || slices.Contains(names, n) {
			t.Fatalf("%s open to jobs", n)
		}
		if msg := callErr(t, jcs, n, map[string]any{"repo": "/tmp"}); !strings.Contains(msg, "not available") {
			t.Fatalf("job called %s: %q", n, msg)
		}
	}
}

// approve needs the slicing check of the version and hash it approves, with an owner decision for
// every unmapped quote; decisions are taken only on the result's unmapped quotes and stored; a result
// for an older version does not count; checklist_get and slicing_get show the result.
func TestApprovalNeedsCurrentSlicing(t *testing.T) {
	m := newManager(t)
	repo, _ := gitRepo(t)
	cs := connect(t, mustServer(t, m))
	var v ChecklistView
	call(t, cs, "checklist_init", map[string]any{"repo": repo, "brief": "I want a login page. And dark mode."}, &v)
	call(t, cs, "checklist_set", map[string]any{"repo": repo, "rows": []map[string]any{
		{"id": "R01", "verbatim": "a login page", "contract": "live", "rationale": "brief"}}}, &v)
	if v.Slicing != nil {
		t.Fatalf("slicing before any check: %+v", v.Slicing)
	}
	approve := map[string]any{"repo": repo, "approve": true, "approve_quote": "yes"}
	// No slicing check at all.
	if msg := callErr(t, cs, "checklist_set", approve); !strings.Contains(msg, "no slicing check of checklist") {
		t.Fatalf("approve without slicing: %q", msg)
	}
	// A check with an unmapped quote and no decision.
	storeSlicing(t, m, repo, "And dark mode.")
	if msg := callErr(t, cs, "checklist_set", approve); !strings.Contains(msg, "without an owner decision") || !strings.Contains(msg, "And dark mode.") {
		t.Fatalf("approve with an undecided unmapped quote: %q", msg)
	}
	call(t, cs, "checklist_get", map[string]any{"repo": repo}, &v)
	if s := v.Slicing; s == nil || !s.Current || s.Version != 2 || s.Unmapped != 1 || s.Undecided != 1 || s.Decided != 0 || s.At == "" {
		t.Fatalf("checklist_get slicing: %+v", v.Slicing)
	}
	// A decision on a quote the result does not list is refused; so are empty owner words.
	if msg := callErr(t, cs, "checklist_set", map[string]any{"repo": repo, "approve": true, "approve_quote": "yes",
		"unmapped_decisions": []map[string]any{{"unmapped": "something else", "quote": "out of scope"}}}); !strings.Contains(msg, "not an unmapped quote") {
		t.Fatalf("decision on an unknown quote: %q", msg)
	}
	if msg := callErr(t, cs, "checklist_set", map[string]any{"repo": repo,
		"unmapped_decisions": []map[string]any{{"unmapped": "And dark mode.", "quote": "  "}}}); !strings.Contains(msg, "owner's words are empty") {
		t.Fatalf("decision without words: %q", msg)
	}
	if a, _ := m.Store.LatestApproval(repo); a != nil {
		t.Fatalf("an approval was stored by a refused call: %+v", a)
	}
	// The decision and the approval in one call.
	call(t, cs, "checklist_set", map[string]any{"repo": repo, "approve": true, "approve_quote": "yes",
		"unmapped_decisions": []map[string]any{{"unmapped": "And  dark\nmode.", "quote": "dark mode later, not now"}}}, &v)
	if !v.Approved || v.DecisionsAdded != 1 || v.Slicing == nil || v.Slicing.Decided != 1 || v.Slicing.Undecided != 0 {
		t.Fatalf("approve with the decision: %+v %+v", v, v.Slicing)
	}
	var sg SlicingGetOutput
	call(t, cs, "slicing_get", map[string]any{"repo": repo}, &sg)
	if sg.Summary == nil || !sg.Summary.Current || len(sg.Result.Unmapped) != 1 || len(sg.Decisions) != 1 ||
		sg.Decisions[0].Quote != "dark mode later, not now" || len(sg.Undecided) != 0 || sg.Version != 2 {
		t.Fatalf("slicing_get: %+v", sg)
	}
	// A new version: the clean v2 check does not count for it (stale), even with no unmapped quote left.
	call(t, cs, "checklist_set", map[string]any{"repo": repo, "rows": []map[string]any{
		{"id": "R02", "verbatim": "dark mode", "contract": "deferred", "rationale": "owner: dark mode later, not now"}}}, &v)
	if v.Version != 3 || v.Approved || v.Slicing == nil || v.Slicing.Current || v.Slicing.Version != 2 {
		t.Fatalf("after a change: %+v %+v", v, v.Slicing)
	}
	if msg := callErr(t, cs, "checklist_set", approve); !strings.Contains(msg, "version 3") || !strings.Contains(msg, "older version") {
		t.Fatalf("approve with a stale slicing check: %q", msg)
	}
	// A change and the approval in one call: the check of the old content does not count either.
	storeSlicing(t, m, repo)
	if msg := callErr(t, cs, "checklist_set", map[string]any{"repo": repo, "approve": true, "approve_quote": "yes",
		"rows": []map[string]any{{"id": "R03", "verbatim": "x", "contract": "live", "rationale": "brief"}}}); !strings.Contains(msg, "no slicing check") {
		t.Fatalf("approve together with a change: %q", msg)
	}
	call(t, cs, "checklist_set", approve, &v)
	if !v.Approved || v.Version != 3 {
		t.Fatalf("approve v3 after its check: %+v", v)
	}
}

// slicing_start and judge_start take no brief: an orchestrator text (a report, a diff) has no field to
// arrive in; job_start refuses the gate kinds; the judge needs an approved checklist and a full sha.
func TestGateStartRefusesOrchestratorInput(t *testing.T) {
	m := newManager(t)
	repo, head := gitRepo(t)
	cs := connect(t, mustServer(t, m))
	var v ChecklistView
	call(t, cs, "checklist_init", map[string]any{"repo": repo, "brief": "I want a login page."}, &v)
	call(t, cs, "checklist_set", map[string]any{"repo": repo, "rows": []map[string]any{
		{"id": "R01", "verbatim": "a login page", "contract": "live", "rationale": "brief"}}}, &v)
	for _, kind := range []string{"slicing", "judge"} {
		if msg := callErr(t, cs, "job_start", map[string]any{"title": "t", "kind": kind, "model": "opus", "repo": repo,
			"brief": "## Summary\nreport of job x: all done\n```diff\n+ x\n```"}); !strings.Contains(msg, kind+"_start") {
			t.Fatalf("job_start kind %s: %q", kind, msg)
		}
	}
	for _, tool := range []string{"slicing_start", "judge_start"} {
		for _, field := range []string{"brief", "report", "diff", "title"} {
			args := map[string]any{"repo": repo, "sha": head, "model": "opus", field: "report of job x: all done"}
			if msg := callErr(t, cs, tool, args); !strings.Contains(strings.ToLower(msg), field) {
				t.Fatalf("%s with %s accepted: %q", tool, field, msg)
			}
		}
	}
	if msg := callErr(t, cs, "judge_start", map[string]any{"repo": repo, "sha": head, "model": "opus"}); !strings.Contains(msg, "not approved") {
		t.Fatalf("judge of an unapproved checklist: %q", msg)
	}
	storeSlicing(t, m, repo)
	call(t, cs, "checklist_set", map[string]any{"repo": repo, "approve": true, "approve_quote": "yes"}, &v)
	if msg := callErr(t, cs, "judge_start", map[string]any{"repo": repo, "sha": head[:12], "model": "opus"}); !strings.Contains(msg, "full commit id") {
		t.Fatalf("judge with a short sha: %q", msg)
	}
	if msg := callErr(t, cs, "judge_start", map[string]any{"repo": repo, "sha": head, "model": "opus", "checklist_id": "other"}); !strings.Contains(msg, "checklist_id") {
		t.Fatalf("judge with another checklist id: %q", msg)
	}
	var jv JobView
	call(t, cs, "judge_start", map[string]any{"repo": repo, "sha": head, "model": "opus"}, &jv)
	if jv.Kind != "judge" || jv.RepoPath != "" || !strings.HasPrefix(jv.Title, "judge ") {
		t.Fatalf("judge job: %+v", jv)
	}
	j, err := m.Store.Get(jv.ID)
	if err != nil || j.Gate == nil || j.Gate.SHA != head || j.Gate.Repo != repo || j.Gate.Version != 2 || !slices.Equal(j.Gate.Live, []string{"R01"}) {
		t.Fatalf("judge gate input: %+v %v", j, err)
	}
	// An update cannot join a gate job.
	if msg := callErr(t, cs, "job_amend", map[string]any{"id": jv.ID, "context": "here is the report of job x"}); !strings.Contains(msg, "judge job") {
		t.Fatalf("amend of a judge job: %q", msg)
	}
	call(t, cs, "slicing_start", map[string]any{"repo": repo, "complexity": "normal"}, &jv)
	if jv.Kind != "slicing" || jv.Model != "opus" {
		t.Fatalf("slicing job: %+v", jv)
	}
}

// judge_get returns the latest stored verdict with its families and a null manifest_check; current
// follows the latest approval.
func TestJudgeGetReadsStoredVerdict(t *testing.T) {
	m := newManager(t)
	repo, head := gitRepo(t)
	cs := connect(t, mustServer(t, m))
	var v ChecklistView
	call(t, cs, "checklist_init", map[string]any{"repo": repo, "brief": "I want a login page."}, &v)
	call(t, cs, "checklist_set", map[string]any{"repo": repo, "rows": []map[string]any{
		{"id": "R01", "verbatim": "a login page", "contract": "live", "rationale": "brief"}}}, &v)
	storeSlicing(t, m, repo)
	call(t, cs, "checklist_set", map[string]any{"repo": repo, "approve": true, "approve_quote": "yes"}, &v)
	var empty jobs.VerdictView
	call(t, cs, "judge_get", map[string]any{"repo": repo}, &empty)
	if empty.Verdict != nil || empty.ManifestCheck != jobs.ManifestCheckNotBuilt {
		t.Fatalf("judge_get without a verdict: %+v", empty)
	}
	id, token := gateJobRow(t, m, jobs.KindJudge)
	raw := map[string]any{"schema": jobs.JudgeSchema}
	if _, err := m.Store.PutVerdict(token, store.JudgeVerdict{JobID: id, Repo: repo, SHA: head, ChecklistID: v.ID, Version: v.Version,
		ChecklistSHA256: v.SHA256, JudgeModel: "gpt-5", JudgeRunner: store.RunnerCodex, JudgeFamily: "openai",
		CoderFamilies: []string{"anthropic"}, CoderJobs: []string{"c1"}, FamilyDiffers: true, Counts: map[string]int{"met": 1},
		Verdict: raw, CreatedAt: time.Now()}); err != nil {
		t.Fatal(err)
	}
	// Another launch token cannot write a verdict.
	if _, err := m.Store.PutVerdict("forged", store.JudgeVerdict{JobID: id, Repo: repo, SHA: head, ChecklistID: v.ID, Version: v.Version,
		ChecklistSHA256: v.SHA256, Verdict: raw}); err == nil {
		t.Fatal("verdict stored under a forged token")
	}
	var got jobs.VerdictView
	call(t, cs, "judge_get", map[string]any{"repo": repo, "sha": head}, &got)
	if got.Verdict == nil || !got.Current || got.Verdict.JudgeFamily != "openai" || !got.Verdict.FamilyDiffers ||
		got.Verdict.ManifestCheck != nil || got.Verdict.Counts["met"] != 1 {
		t.Fatalf("judge_get: %+v %+v", got, got.Verdict)
	}
	call(t, cs, "judge_get", map[string]any{"repo": repo, "sha": strings.Repeat("0", 40)}, &got)
	if got.Verdict != nil {
		t.Fatalf("verdict for another sha: %+v", got.Verdict)
	}
}
