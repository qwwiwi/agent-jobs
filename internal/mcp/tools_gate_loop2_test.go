package mcp

import (
	"strings"
	"testing"
)

// An approval a later slicing result voided stays void when the owner only decides the new quote: the
// decision is stored, but the checklist is approved again only by a new explicit approval.
func TestVoidedApprovalNeedsNewApproval(t *testing.T) {
	m := newManager(t)
	repo, head := gitRepo(t)
	cs := connect(t, mustServer(t, m))
	var v ChecklistView
	call(t, cs, "checklist_init", map[string]any{"repo": repo, "brief": "I want a login page."}, &v)
	call(t, cs, "checklist_set", map[string]any{"repo": repo, "rows": []map[string]any{
		{"id": "R01", "verbatim": "a login page", "contract": "live", "rationale": "brief"}}}, &v)
	storeSlicing(t, m, repo)
	call(t, cs, "checklist_set", map[string]any{"repo": repo, "approve": true, "approve_quote": "yes"}, &v)
	if !v.Approved {
		t.Fatalf("not approved: %+v", v)
	}
	storeSlicing(t, m, repo, "I want a login page.")
	call(t, cs, "checklist_set", map[string]any{"repo": repo,
		"unmapped_decisions": []map[string]any{{"unmapped": "I want a login page.", "quote": "R01 covers it"}}}, &v)
	if v.Approved || v.DecisionsAdded != 1 || !strings.Contains(v.Reason, "void") {
		t.Fatalf("decisions-only call after the void: approved %v, decisions %d, reason %q", v.Approved, v.DecisionsAdded, v.Reason)
	}
	call(t, cs, "checklist_get", map[string]any{"repo": repo}, &v)
	if v.Approved {
		t.Fatalf("checklist_get after a decisions-only call: approved (%+v)", v)
	}
	if msg := callErr(t, cs, "checklist_set", map[string]any{"repo": repo,
		"waivers": []map[string]any{{"requirement": "R01", "sha": head, "quote": "ok"}}}); !strings.Contains(msg, "void") {
		t.Fatalf("waiver on the voided approval after a decisions-only call: %q", msg)
	}
	call(t, cs, "checklist_set", map[string]any{"repo": repo, "approve": true, "approve_quote": "yes again"}, &v)
	if !v.Approved {
		t.Fatalf("not approved after a new approval: %+v", v)
	}
	// A later result that lists only decided quotes does not void the new approval.
	storeSlicing(t, m, repo, "I want a login page.")
	call(t, cs, "checklist_get", map[string]any{"repo": repo}, &v)
	if !v.Approved {
		t.Fatalf("a result with decided quotes voided the approval: %+v", v)
	}
}
