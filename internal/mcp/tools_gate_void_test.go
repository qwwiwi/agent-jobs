package mcp

import (
	"strings"
	"testing"
	"time"

	"github.com/qwwiwi/agent-jobs/internal/jobs"
	"github.com/qwwiwi/agent-jobs/internal/store"
)

// A slicing result stored after the approval with an undecided quote voids the approval: checklist_get
// says not approved, a waiver is refused, judge_get's verdict is no longer current; the owner's decision
// and a new approval restore it.
func TestSlicingRerunVoidsApproval(t *testing.T) {
	m := newManager(t)
	repo, head := gitRepo(t)
	cs := connect(t, mustServer(t, m))
	var v ChecklistView
	call(t, cs, "checklist_init", map[string]any{"repo": repo, "brief": "I want a login page."}, &v)
	call(t, cs, "checklist_set", map[string]any{"repo": repo, "rows": []map[string]any{
		{"id": "R01", "verbatim": "a login page", "contract": "live", "basis": "brief"}}}, &v)
	storeSlicing(t, m, repo)
	call(t, cs, "checklist_set", map[string]any{"repo": repo, "approve": true, "approve_quote": "yes"}, &v)
	if !v.Approved {
		t.Fatalf("not approved: %+v", v)
	}
	id, token := gateJobRow(t, m, jobs.KindJudge)
	if _, err := m.Store.PutVerdict(token, store.JudgeVerdict{JobID: id, Repo: repo, SHA: head, ChecklistID: v.ID, Version: v.Version,
		ChecklistSHA256: v.SHA256, JudgeModel: "gpt-5", JudgeRunner: store.RunnerCodex, JudgeFamily: "openai",
		Counts: map[string]int{"met": 1}, Verdict: map[string]any{"schema": jobs.JudgeSchema}, CreatedAt: time.Now()}); err != nil {
		t.Fatal(err)
	}
	var got jobs.VerdictView
	call(t, cs, "judge_get", map[string]any{"repo": repo}, &got)
	if !got.Current {
		t.Fatalf("verdict of the approved checklist not current: %+v", got)
	}
	storeSlicing(t, m, repo, "I want a login page.")
	call(t, cs, "checklist_get", map[string]any{"repo": repo}, &v)
	if v.Approved || !strings.Contains(v.Reason, "void") || v.Slicing == nil || v.Slicing.Results != 2 || v.Slicing.Undecided != 1 {
		t.Fatalf("checklist after a re-run with an undecided quote: %+v %+v", v, v.Slicing)
	}
	if msg := callErr(t, cs, "checklist_set", map[string]any{"repo": repo,
		"waivers": []map[string]any{{"requirement": "R01", "sha": head, "quote": "ok"}}}); !strings.Contains(msg, "void") {
		t.Fatalf("waiver on a voided approval: %q", msg)
	}
	call(t, cs, "judge_get", map[string]any{"repo": repo}, &got)
	if got.Current {
		t.Fatalf("verdict of a voided approval is current: %+v", got)
	}
	call(t, cs, "checklist_set", map[string]any{"repo": repo, "approve": true, "approve_quote": "yes again",
		"unmapped_decisions": []map[string]any{{"unmapped": "I want a login page.", "quote": "R01 covers it"}}}, &v)
	if !v.Approved {
		t.Fatalf("not approved after the decision: %+v", v)
	}
}
