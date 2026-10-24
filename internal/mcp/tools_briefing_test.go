package mcp

import (
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/qwwiwi/agent-jobs/internal/checklist"
	"github.com/qwwiwi/agent-jobs/internal/jobs"
	"github.com/qwwiwi/agent-jobs/internal/store"
)

// briefing_start runs on the draft checklist (not approved), takes no brief and no sha; job_start
// refuses the kind; job_amend refuses its jobs; briefing_get reads the stored result, current while the
// checklist is at the version and hash it was run on.
func TestBriefingTools(t *testing.T) {
	m := newManager(t)
	repo, _ := gitRepo(t)
	cs := connect(t, mustServer(t, m))
	var v ChecklistView
	call(t, cs, "checklist_init", map[string]any{"repo": repo, "brief": "I want a shop that takes payments."}, &v)
	call(t, cs, "checklist_set", map[string]any{"repo": repo, "rows": []map[string]any{
		{"id": "R01", "verbatim": "a shop", "contract": "live", "rationale": "brief"},
		{"id": "R02", "verbatim": "takes payments", "contract": "live", "rationale": "brief"}}}, &v)
	if v.Approved {
		t.Fatal("draft approved")
	}
	if msg := callErr(t, cs, "job_start", map[string]any{"title": "t", "kind": "briefing", "model": "opus", "repo": repo,
		"brief": "ask the owner about payments"}); !strings.Contains(msg, "briefing_start") {
		t.Fatalf("job_start kind briefing: %q", msg)
	}
	for _, field := range []string{"brief", "report", "diff", "title"} {
		if msg := callErr(t, cs, "briefing_start", map[string]any{"repo": repo, "model": "opus", field: "report of job x"}); !strings.Contains(strings.ToLower(msg), field) {
			t.Fatalf("briefing_start with %s accepted: %q", field, msg)
		}
	}
	if msg := callErr(t, cs, "briefing_start", map[string]any{"repo": repo, "model": "opus", "checklist_id": "other"}); !strings.Contains(msg, "checklist_id") {
		t.Fatalf("briefing_start with another checklist id: %q", msg)
	}
	var empty BriefingGetOutput
	call(t, cs, "briefing_get", map[string]any{"repo": repo}, &empty)
	if empty.Result != nil || empty.Current || empty.Version != 2 {
		t.Fatalf("briefing_get before any: %+v", empty)
	}
	if msg := callErr(t, cs, "briefing_get", map[string]any{"repo": repo, "sha": strings.Repeat("a", 40)}); !strings.Contains(msg, "sha does not apply") {
		t.Fatalf("briefing_get with a sha: %q", msg)
	}
	var jv JobView
	call(t, cs, "briefing_start", map[string]any{"repo": repo, "complexity": "normal"}, &jv)
	if jv.Kind != "briefing" || jv.Model != "opus" || jv.RepoPath != "" || !strings.HasPrefix(jv.Title, "briefing ") {
		t.Fatalf("briefing job: %+v", jv)
	}
	j, err := m.Store.Get(jv.ID)
	if err != nil || j.Gate == nil || j.Gate.Version != 2 || len(j.Gate.Rows) != 2 || !strings.Contains(j.Gate.Brief, "takes payments") {
		t.Fatalf("briefing gate input: %+v %v", j, err)
	}
	if msg := callErr(t, cs, "job_amend", map[string]any{"id": jv.ID, "context": "also ask about colours"}); !strings.Contains(msg, "briefing job") {
		t.Fatalf("amend of a briefing job: %q", msg)
	}
	id, token := gateJobRow(t, m, jobs.KindBriefing)
	q := store.BriefingQuestion{N: 1, Closes: []string{"R02"}, Kind: "cost_or_vendor", Q: "YooKassa or Stripe?",
		Recommended: "YooKassa", Why: "the provider stalls the launch"}
	if _, err := m.Store.PutBriefing(token, store.BriefingResult{JobID: id, Repo: repo, ChecklistID: v.ID, Version: v.Version,
		ChecklistSHA256: v.SHA256, Model: "opus", Runner: store.RunnerClaude, EnforcedBlind: true,
		Rounds: []store.BriefingRound{{Questions: []store.BriefingQuestion{q}}}, CreatedAt: time.Now()}); err != nil {
		t.Fatal(err)
	}
	var got BriefingGetOutput
	call(t, cs, "briefing_get", map[string]any{"repo": repo}, &got)
	if !got.Current || got.Result == nil || got.Result.JobID != id || got.Result.Rounds[0].Questions[0].Recommended != "YooKassa" {
		t.Fatalf("briefing_get: %+v", got)
	}
	// The owner's reply changes a row: the result is now of an older draft.
	call(t, cs, "checklist_set", map[string]any{"repo": repo, "rows": []map[string]any{
		{"id": "R02", "verbatim": "takes payments", "contract": "live", "rationale": "owner: «YooKassa»"}}}, &v)
	call(t, cs, "briefing_get", map[string]any{"repo": repo}, &got)
	if got.Current || got.Result == nil || got.Version != 3 || got.Result.Version != 2 {
		t.Fatalf("briefing_get after a change: %+v", got)
	}
}

// The owner's replies to a briefing go back through checklist_set (F8): new scope is a G row with a
// dated BRIEF Addition in the same call, a choice goes into the Rationale of its row, a cancellation is
// dropped with the owner's quote; each raises the version, and changed_since_approval names exactly the
// rows to re-approve; the next approval records them and clears it.
func TestBriefingRepliesThroughChecklistSet(t *testing.T) {
	m := newManager(t)
	repo, _ := gitRepo(t)
	cs := connect(t, mustServer(t, m))
	var v ChecklistView
	call(t, cs, "checklist_init", map[string]any{"repo": repo, "brief": "I want a shop that takes payments and dark mode."}, &v)
	call(t, cs, "checklist_set", map[string]any{"repo": repo, "rows": []map[string]any{
		{"id": "R01", "verbatim": "a shop", "contract": "live", "rationale": "brief"},
		{"id": "R02", "verbatim": "takes payments", "contract": "live", "rationale": "brief"},
		{"id": "R03", "verbatim": "dark mode", "contract": "live", "rationale": "brief"}}}, &v)
	if v.ChangedSinceApproval != nil {
		t.Fatalf("changes before any approval: %+v", v.ChangedSinceApproval)
	}
	storeSlicing(t, m, repo)
	call(t, cs, "checklist_set", map[string]any{"repo": repo, "approve": true, "approve_quote": "yes"}, &v)
	if !v.Approved || v.Version != 2 || v.ChangedSinceApproval != nil {
		t.Fatalf("approved v2: %+v", v)
	}
	// New scope without the owner's words in additions is refused; a drop without the owner's words too.
	if msg := callErr(t, cs, "checklist_set", map[string]any{"repo": repo, "rows": []map[string]any{
		{"id": "G01", "verbatim": "a CSV export", "contract": "live", "rationale": "owner addition"}}}); !strings.Contains(msg, "additions") {
		t.Fatalf("G row without an addition: %q", msg)
	}
	if msg := callErr(t, cs, "checklist_set", map[string]any{"repo": repo, "rows": []map[string]any{
		{"id": "R03", "verbatim": "dark mode", "contract": "dropped"}}}); msg == "" {
		t.Fatal("dropped without the owner's words accepted")
	}
	// The replies of one round in one call.
	call(t, cs, "checklist_set", map[string]any{"repo": repo, "expect_version": 2,
		"additions": []map[string]any{{"quote": "and a CSV export of orders", "date": "2026-10-24"}},
		"rows": []map[string]any{
			{"id": "G01", "verbatim": "a CSV export of orders", "contract": "live", "rationale": "addition 2026-10-24"},
			{"id": "R02", "verbatim": "takes payments", "contract": "live", "rationale": "owner chose YooKassa: «YooKassa, not Stripe»"},
			{"id": "R03", "verbatim": "dark mode", "contract": "dropped", "rationale": "owner: «no dark mode, drop it»"}}}, &v)
	if v.Approved || v.Version != 3 || !v.VersionBumped || !v.BriefWritten {
		t.Fatalf("after the replies: %+v", v)
	}
	brief, err := checklist.Read(repo)
	if err != nil || !strings.Contains(string(brief.Brief), "## Additions") || !strings.Contains(string(brief.Brief), "2026-10-24") ||
		!strings.Contains(string(brief.Brief), "and a CSV export of orders") {
		t.Fatalf("BRIEF.md addition: %s %v", brief.Brief, err)
	}
	c := v.ChangedSinceApproval
	if c == nil || !c.Known || c.ApprovedVersion != 2 || !c.BriefChanged || len(c.Rows) != 3 {
		t.Fatalf("changed_since_approval: %+v", c)
	}
	byID := map[string]checklist.RowChange{}
	for _, r := range c.Rows {
		byID[r.ID] = r
	}
	if byID["R02"].Change != checklist.RowChanged || !slices.Equal(byID["R02"].Fields, []string{"rationale"}) ||
		byID["R03"].Change != checklist.RowChanged || !slices.Equal(byID["R03"].Fields, []string{"contract", "rationale"}) ||
		byID["G01"].Change != checklist.RowAdded || byID["R01"].ID != "" {
		t.Fatalf("changed rows: %+v", c.Rows)
	}
	if slices.Contains(v.Live, "R03") || !slices.Contains(v.Live, "G01") {
		t.Fatalf("live after the drop: %v", v.Live)
	}
	// checklist_get shows the same; the re-approval (after the slicing check of v3) records the new rows.
	var g ChecklistView
	call(t, cs, "checklist_get", map[string]any{"repo": repo}, &g)
	if g.ChangedSinceApproval == nil || len(g.ChangedSinceApproval.Rows) != 3 {
		t.Fatalf("checklist_get changes: %+v", g.ChangedSinceApproval)
	}
	storeSlicing(t, m, repo)
	call(t, cs, "checklist_set", map[string]any{"repo": repo, "approve": true, "approve_quote": "yes to R02, R03, G01"}, &v)
	if !v.Approved || v.Version != 3 || v.ChangedSinceApproval != nil {
		t.Fatalf("re-approved v3: %+v", v)
	}
	a, err := m.Store.LatestApproval(repo)
	if err != nil || a == nil || len(a.Rows) != 4 || a.BriefFileSHA256 != checklist.SHA256Hex(brief.Brief) {
		t.Fatalf("approval record: %+v %v", a, err)
	}
	// A later change of one row lists only that row.
	call(t, cs, "checklist_set", map[string]any{"repo": repo, "rows": []map[string]any{
		{"id": "R01", "verbatim": "a shop", "contract": "live", "rationale": "owner: «a small shop»"}}}, &v)
	if c := v.ChangedSinceApproval; c == nil || c.BriefChanged || len(c.Rows) != 1 || c.Rows[0].ID != "R01" || c.ApprovedVersion != 3 {
		t.Fatalf("one change: %+v", v.ChangedSinceApproval)
	}
}
