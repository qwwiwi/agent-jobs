package mcp

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/qwwiwi/agent-jobs/internal/checklist"
)

// approvedChecklist inits id in repo, adds one row and approves it.
func approvedChecklist(t *testing.T, cs *sdk.ClientSession, repo, id, words string) ChecklistView {
	t.Helper()
	var v ChecklistView
	call(t, cs, "checklist_init", map[string]any{"repo": repo, "id": id, "brief": words}, &v)
	call(t, cs, "checklist_set", map[string]any{"repo": repo, "rows": []map[string]any{
		{"id": "R01", "verbatim": "a login page", "contract": "live", "basis": "brief"}}}, &v)
	call(t, cs, "checklist_set", map[string]any{"repo": repo, "approve": true, "approve_quote": "yes"}, &v)
	if !v.Approved {
		t.Fatalf("approve %s: %+v", id, v)
	}
	return v
}

// The BRIEF prefix is bound per checklist id: renaming the id in the file (with an edited BRIEF) finds
// no record of the new id and is refused, and an id once created in the repository is never reused.
func TestChecklistPrefixBoundPerID(t *testing.T) {
	m := newManager(t)
	repo, _ := gitRepo(t)
	cs := connect(t, mustServer(t, m))
	approvedChecklist(t, cs, repo, "first", "I want a login page.")
	path, briefPath := checklist.ChecklistPath(repo), checklist.BriefPath(repo)
	text, _ := os.ReadFile(path)
	brief, _ := os.ReadFile(briefPath)
	writeFile(t, path, strings.Replace(string(text), "- id: first", "- id: renamed", 1))
	writeFile(t, briefPath, strings.Replace(string(brief), "login", "logout", 1))
	for _, in := range []map[string]any{
		{"repo": repo, "approve": true, "approve_quote": "yes"},
		{"repo": repo, "rows": []map[string]any{{"id": "R02", "verbatim": "x", "contract": "live"}}},
	} {
		if msg := callErr(t, cs, "checklist_set", in); !strings.Contains(msg, `"renamed"`) {
			t.Fatalf("renamed id with an edited BRIEF accepted: %q", msg)
		}
	}
	if a, err := m.Store.LatestApproval(repo); err != nil || a == nil || a.ChecklistID != "first" {
		t.Fatalf("an approval was stored for the renamed id: %+v %v", a, err)
	}
	// An id is never reused, not even one older than the latest approval.
	if err := os.RemoveAll(filepath.Join(repo, "brief")); err != nil {
		t.Fatal(err)
	}
	approvedChecklist(t, cs, repo, "second", "other words")
	if err := os.RemoveAll(filepath.Join(repo, "brief")); err != nil {
		t.Fatal(err)
	}
	if msg := callErr(t, cs, "checklist_init", map[string]any{"repo": repo, "id": "first", "brief": "new words"}); !strings.Contains(msg, "new id") {
		t.Fatalf("an older id reused: %q", msg)
	}
	// An id only created (never approved) is not reused either.
	var v ChecklistView
	call(t, cs, "checklist_init", map[string]any{"repo": repo, "id": "third", "brief": "draft"}, &v)
	if err := os.RemoveAll(filepath.Join(repo, "brief")); err != nil {
		t.Fatal(err)
	}
	if msg := callErr(t, cs, "checklist_init", map[string]any{"repo": repo, "id": "third", "brief": "draft 2"}); !strings.Contains(msg, "new id") {
		t.Fatalf("an unapproved id reused: %q", msg)
	}
}

// Before any approval the BRIEF prefix is bound to checklist_init's record: an edit is refused.
func TestChecklistPrefixBoundAtInit(t *testing.T) {
	m := newManager(t)
	repo, _ := gitRepo(t)
	cs := connect(t, mustServer(t, m))
	var v ChecklistView
	call(t, cs, "checklist_init", map[string]any{"repo": repo, "brief": "I want a login page."}, &v)
	brief, _ := os.ReadFile(checklist.BriefPath(repo))
	writeFile(t, checklist.BriefPath(repo), strings.Replace(string(brief), "login", "logout", 1))
	if msg := callErr(t, cs, "checklist_set", map[string]any{"repo": repo, "approve": true, "approve_quote": "yes",
		"rows": []map[string]any{{"id": "R01", "verbatim": "a logout page", "contract": "live"}}}); !strings.Contains(msg, "above ## Additions") {
		t.Fatalf("BRIEF edited after init accepted: %q", msg)
	}
}

// One agent manages a repository's checklist: another agent's store has no record of it, so its no-op
// checklist_set writes nothing (the first agent's approval holds) and a change is refused.
func TestChecklistOtherAgentStoreNoVoid(t *testing.T) {
	repo, _ := gitRepo(t)
	a := connect(t, mustServer(t, newManager(t)))
	approvedChecklist(t, a, repo, "demo", "I want a login page.")
	before, _ := os.ReadFile(checklist.ChecklistPath(repo))
	b := connect(t, mustServer(t, newManager(t)))
	var v ChecklistView
	call(t, b, "checklist_set", map[string]any{"repo": repo}, &v)
	if v.Written || v.VersionBumped {
		t.Fatalf("no-op from another store wrote: %+v", v)
	}
	if after, _ := os.ReadFile(checklist.ChecklistPath(repo)); string(after) != string(before) {
		t.Fatalf("CHECKLIST.md changed:\n%s", after)
	}
	call(t, a, "checklist_get", map[string]any{"repo": repo}, &v)
	if !v.Approved {
		t.Fatalf("the managing agent's approval was voided: %+v", v)
	}
	if msg := callErr(t, b, "checklist_set", map[string]any{"repo": repo, "rows": []map[string]any{
		{"id": "R02", "verbatim": "x", "contract": "live"}}}); !strings.Contains(msg, "another agent") {
		t.Fatalf("change from another store: %q", msg)
	}
}
