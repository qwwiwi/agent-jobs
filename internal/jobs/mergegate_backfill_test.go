package jobs

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/qwwiwi/agent-jobs/internal/store"
)

// BackfillGateNames stores the remote names of every approved repository with a readable work tree
// (an approval stored before names were recorded), so the gate still matches it by name once its work
// tree is gone; a repository whose work tree is gone already is skipped without an error.
func TestBackfillGateNames(t *testing.T) {
	m, repo := mergeGateRepo(t)
	gone := filepath.Join(t.TempDir(), "gone")
	if err := os.Mkdir(gone, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := m.Store.PutApproval(store.ChecklistApproval{Repo: gone, ChecklistID: "x", Version: 1, ChecklistSHA256: "h",
		BriefSHA256: "b", Quote: "yes", ApprovedAt: m.Now()}); err != nil {
		t.Fatal(err)
	}
	if g, err := m.Store.GateRepoOf(mainWorktree(repo)); err != nil || len(g.Names) != 0 {
		t.Fatalf("names before the backfill: %+v %v", g, err)
	}
	if err := m.BackfillGateNames(); err != nil {
		t.Fatal(err)
	}
	if g, err := m.Store.GateRepoOf(mainWorktree(repo)); err != nil || len(g.Names) != 1 || g.Names[0] != "acme/demo" {
		t.Fatalf("names after the backfill: %+v %v", g, err)
	}
	if err := os.RemoveAll(repo); err != nil {
		t.Fatal(err)
	}
	// gone has no names and no remotes: while it is gated, a merge by name is unsure (TestMergeGateNoNamesUnreadable).
	if r := gateAnswer(t, m, t.TempDir(), "gh pr merge 5 -R other/repo", ""); !strings.Contains(r, "cannot tell") {
		t.Fatalf("by name while gone is gated without names: %q", r)
	}
	if _, err := m.RetireGate(gone); err != nil {
		t.Fatal(err)
	}
	if r := gateAnswer(t, m, t.TempDir(), "gh pr merge 5 -R other/repo", ""); r != "" {
		t.Fatalf("an unrelated name: %q", r)
	}
	if r := gateAnswer(t, m, t.TempDir(), "gh pr merge 5 -R acme/demo", ""); !strings.Contains(r, "its checklist approval is void") {
		t.Fatalf("gone, by backfilled name: %q", r)
	}
}
