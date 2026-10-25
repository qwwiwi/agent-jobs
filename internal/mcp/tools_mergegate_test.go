package mcp

import (
	"os/exec"
	"slices"
	"testing"

	"github.com/qwwiwi/agent-jobs/internal/jobs"
)

// checklist_set stores the OWNER/REPO names of the repository's remotes with the approval, for the merge
// hook (F9 (b)) to match the repository by once its work tree is gone.
func TestChecklistApprovalRecordsGateNames(t *testing.T) {
	m := newManager(t)
	repo, _ := gitRepo(t)
	if out, err := exec.Command("git", "-C", repo, "remote", "add", "origin", "git@github.com:Acme/Demo.git").CombinedOutput(); err != nil {
		t.Fatalf("git remote add: %v %s", err, out)
	}
	cs := connect(t, mustServer(t, m))
	var v ChecklistView
	call(t, cs, "checklist_init", map[string]any{"repo": repo, "brief": "I want a login page."}, &v)
	call(t, cs, "checklist_set", map[string]any{"repo": repo, "rows": []map[string]any{
		{"id": "R01", "verbatim": "a login page", "contract": "live", "rationale": "brief"}}}, &v)
	_, main, err := jobs.ChecklistRepo(repo)
	if err != nil {
		t.Fatal(err)
	}
	if g, err := m.Store.GateRepoOf(main); err != nil || g.Names != nil {
		t.Fatalf("names before the approval: %+v %v", g, err)
	}
	storeSlicing(t, m, repo)
	call(t, cs, "checklist_set", map[string]any{"repo": repo, "approve": true, "approve_quote": "yes"}, &v)
	if !v.Approved {
		t.Fatalf("not approved: %+v", v)
	}
	if g, err := m.Store.GateRepoOf(main); err != nil || !slices.Equal(g.Names, []string{"acme/demo"}) {
		t.Fatalf("names after the approval: %+v %v", g, err)
	}
}
