package jobs

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/qwwiwi/agent-jobs/internal/checklist"
	"github.com/qwwiwi/agent-jobs/internal/store"
)

// shippedRepo is gateRepo with an approved checklist, a GitHub origin and a feature commit X on top of
// the head A; release_check "passed" on X at the approval (a stored pass, no verdict). origin's default
// branch (refs/remotes/origin/main, origin/HEAD pointing at it) is at A, so X is not on it yet.
func shippedRepo(t *testing.T, m *Manager) (repo, shaA, shaX string) {
	t.Helper()
	repo, shaA = gateRepo(t)
	gitT(t, repo, "remote", "add", "origin", "git@github.com:Acme/Demo.git")
	approveBrief(t, m, repo)
	if err := os.WriteFile(filepath.Join(repo, "feature.txt"), []byte("feature\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitT(t, repo, "add", "feature.txt")
	gitT(t, repo, "commit", "-qm", "feature")
	shaX = gitT(t, repo, "rev-parse", "HEAD")
	last, err := m.Store.LatestApproval(mainWorktree(repo))
	if err != nil || last == nil {
		t.Fatalf("approval: %+v %v", last, err)
	}
	if err := m.Store.PutReleasePass(store.ReleasePass{Repo: mainWorktree(repo), SHA: shaX, ChecklistID: last.ChecklistID,
		Version: last.Version, ChecklistSHA256: last.ChecklistSHA256, CreatedAt: time.Now()}); err != nil {
		t.Fatal(err)
	}
	gitT(t, repo, "update-ref", "refs/remotes/origin/main", shaA)
	gitT(t, repo, "symbolic-ref", "refs/remotes/origin/HEAD", "refs/remotes/origin/main")
	return repo, shaA, shaX
}

// A checklist whose release_check passed on a sha now on origin's default branch is shipped at the next
// tick: it stops gating merges (an unrelated fix merges, also without --match-head-commit), shows as
// shipped, and a void approval or a new approval gates the repository again. While the pass sha is not
// on the default branch the repository stays gated.
func TestMergeGateShipped(t *testing.T) {
	m := newManager(t)
	repo, _, shaX := shippedRepo(t, m)
	main := mainWorktree(repo)
	unrelated := strings.Repeat("ab", 20)
	merge := "gh pr merge 7 --squash --match-head-commit " + unrelated
	gate := func() string {
		t.Helper()
		a, err := m.MergeGateCheck(repo, merge, "")
		if err != nil {
			t.Fatal(err)
		}
		return a.Refusal
	}

	// The pass sha is not on origin's default branch: still gated, and the refusal says what lifts it.
	if err := m.Tick(); err != nil {
		t.Fatal(err)
	}
	if g, err := m.Store.GateRepoOf(main); err != nil || g.ShippedSeq != 0 || g.ShippedSHA != "" {
		t.Fatalf("shipped before the pass sha is on the default branch: %+v %v", g, err)
	}
	if r := gate(); !strings.Contains(r, "release_check has not passed on "+unrelated) ||
		!strings.Contains(r, "passed on "+shaX) || !strings.Contains(r, "git fetch") {
		t.Fatalf("not shipped: %q", r)
	}

	// The pass sha reaches origin/main (a merge commit, fetched): shipped at the next tick.
	gitT(t, repo, "update-ref", "refs/remotes/origin/main", shaX)
	if err := m.Tick(); err != nil {
		t.Fatal(err)
	}
	last, err := m.Store.LatestApproval(main)
	if err != nil {
		t.Fatal(err)
	}
	if g, err := m.Store.GateRepoOf(main); err != nil || g.ShippedSeq != last.Seq || g.ShippedSHA != shaX || g.ShippedAt.IsZero() {
		t.Fatalf("not recorded as shipped: %+v %v", g, err)
	}
	for _, cmd := range []string{merge, "gh pr merge 7", "gh pr merge 7 --auto", "gh -R acme/demo pr merge 7"} {
		if a, err := m.MergeGateCheck(repo, cmd, ""); err != nil || a.Refusal != "" || a.Warning != "" {
			t.Errorf("%s after shipped: %+v %v", cmd, a, err)
		}
	}
	views, err := m.MergeGates()
	if err != nil || len(views) != 1 || views[0].State != MergeGateShipped || views[0].ShippedSHA != shaX ||
		views[0].ChecklistID != "demo" || views[0].Version != 2 || views[0].Repo != main {
		t.Fatalf("doctor view: %+v %v", views, err)
	}
	if s := MergeGateText(views[0]); !strings.Contains(s, "checklist demo v2 shipped (release_check passed on "+shaX) {
		t.Fatalf("status line: %q", s)
	}

	// A void approval stays gated although a shipped record exists.
	p := checklist.ChecklistPath(repo)
	orig, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(strings.Replace(string(orig), "RATIONALE_MARKER", "edited by hand", 1)), 0o644); err != nil {
		t.Fatal(err)
	}
	if r := gate(); !strings.Contains(r, "its checklist approval is void") {
		t.Fatalf("void after shipped: %q", r)
	}
	if views, err := m.MergeGates(); err != nil || len(views) != 1 || views[0].State != MergeGateVoid {
		t.Fatalf("void view: %+v %v", views, err)
	}
	if err := os.WriteFile(p, orig, 0o644); err != nil {
		t.Fatal(err)
	}
	if r := gate(); r != "" {
		t.Fatalf("restored checklist, shipped: %q", r)
	}

	// A pass that no longer counts (release_check ran again on the sha and failed) lifts nothing.
	if err := m.Store.DropReleasePasses(main, shaX); err != nil {
		t.Fatal(err)
	}
	if r := gate(); !strings.Contains(r, "release_check has not passed on "+unrelated) {
		t.Fatalf("shipped record without its pass: %q", r)
	}
	if err := m.Store.PutReleasePass(store.ReleasePass{Repo: main, SHA: shaX, ChecklistID: last.ChecklistID,
		Version: last.Version, ChecklistSHA256: last.ChecklistSHA256, CreatedAt: time.Now()}); err != nil {
		t.Fatal(err)
	}
	if r := gate(); r != "" {
		t.Fatalf("pass restored: %q", r)
	}

	// A new approval (a new version) gates the repository again; the tick does not ship it (no pass at it).
	if err := os.WriteFile(p, []byte(strings.Replace(string(orig), "- version: 2", "- version: 3", 1)), 0o644); err != nil {
		t.Fatal(err)
	}
	approveBrief(t, m, repo)
	if r := gate(); !strings.Contains(r, "release_check has not passed on "+unrelated) || !strings.Contains(r, "checklist demo v3") {
		t.Fatalf("new approval after shipped: %q", r)
	}
	if err := m.Tick(); err != nil {
		t.Fatal(err)
	}
	if r := gate(); !strings.Contains(r, "release_check has not passed on "+unrelated) {
		t.Fatalf("new approval after a tick: %q", r)
	}
	if views, err := m.MergeGates(); err != nil || len(views) != 1 || views[0].State != MergeGateGated || views[0].Version != 3 {
		t.Fatalf("gated view: %+v %v", views, err)
	}
}

// A squash merge leaves the pass sha off the default branch but puts its exact tree there: shipped. A
// commit on the default branch with another tree (the branch was behind) is not.
func TestMergeGateShippedSquash(t *testing.T) {
	m := newManager(t)
	repo, shaA, shaX := shippedRepo(t, m)
	main := mainWorktree(repo)
	tree := gitT(t, repo, "rev-parse", shaX+"^{tree}")
	other := gitT(t, repo, "commit-tree", "-p", shaA, "-m", "squash of another tree", gitT(t, repo, "rev-parse", shaA+"^{tree}"))
	gitT(t, repo, "update-ref", "refs/remotes/origin/main", other)
	if err := m.Tick(); err != nil {
		t.Fatal(err)
	}
	if g, err := m.Store.GateRepoOf(main); err != nil || g.ShippedSeq != 0 {
		t.Fatalf("shipped by another tree: %+v %v", g, err)
	}
	squash := gitT(t, repo, "commit-tree", "-p", other, "-m", "feature (#5)", tree)
	gitT(t, repo, "update-ref", "refs/remotes/origin/main", squash)
	if err := m.Tick(); err != nil {
		t.Fatal(err)
	}
	if g, err := m.Store.GateRepoOf(main); err != nil || g.ShippedSeq == 0 || g.ShippedSHA != shaX {
		t.Fatalf("squash merge not shipped: %+v %v", g, err)
	}
	if a, err := m.MergeGateCheck(repo, "gh pr merge 8", ""); err != nil || a.Refusal != "" {
		t.Fatalf("merge after a squash ship: %+v %v", a, err)
	}
}

// Without a remote-tracking default branch (no origin/HEAD, origin/main or origin/master: nothing
// fetched) nothing ships.
func TestMergeGateShippedNeedsRemoteRef(t *testing.T) {
	m := newManager(t)
	repo, _, shaX := shippedRepo(t, m)
	gitT(t, repo, "symbolic-ref", "--delete", "refs/remotes/origin/HEAD")
	gitT(t, repo, "update-ref", "-d", "refs/remotes/origin/main")
	// HEAD (the local branch) holds shaX: local history is not the remote's default branch.
	if head := gitT(t, repo, "rev-parse", "HEAD"); head != shaX {
		t.Fatalf("HEAD %s, want %s", head, shaX)
	}
	if err := m.Tick(); err != nil {
		t.Fatal(err)
	}
	if g, err := m.Store.GateRepoOf(mainWorktree(repo)); err != nil || g.ShippedSeq != 0 {
		t.Fatalf("shipped without a remote ref: %+v %v", g, err)
	}
	if a, err := m.MergeGateCheck(repo, "gh pr merge 8", ""); err != nil || a.Refusal == "" {
		t.Fatalf("not gated: %+v %v", a, err)
	}
}

// The tick ships no stale pass (a newer verdict than the one it read), and once the shipped pass no
// longer counts (release_check ran again on it and failed) it re-reads the approval: another pass now
// on the default branch ships it again. An origin/HEAD that points outside refs/remotes/origin is not
// origin's default branch.
func TestMergeGateShippedRecheck(t *testing.T) {
	m := newManager(t)
	repo, _, shaX := shippedRepo(t, m)
	main := mainWorktree(repo)
	last, err := m.Store.LatestApproval(main)
	if err != nil {
		t.Fatal(err)
	}
	pass := func(sha, verdict string) {
		t.Helper()
		if err := m.Store.PutReleasePass(store.ReleasePass{Repo: main, SHA: sha, ChecklistID: last.ChecklistID, Version: last.Version,
			ChecklistSHA256: last.ChecklistSHA256, VerdictJob: verdict, CreatedAt: time.Now()}); err != nil {
			t.Fatal(err)
		}
	}
	shipped := func() store.GateRepo {
		t.Helper()
		if err := m.Tick(); err != nil {
			t.Fatal(err)
		}
		g, err := m.Store.GateRepoOf(main)
		if err != nil {
			t.Fatal(err)
		}
		return g
	}
	// origin/HEAD naming a local branch that holds shaX does not count.
	gitT(t, repo, "branch", "local-default", shaX)
	gitT(t, repo, "symbolic-ref", "refs/remotes/origin/HEAD", "refs/heads/local-default")
	if g := shipped(); g.ShippedSeq != 0 {
		t.Fatalf("shipped by a local branch: %+v", g)
	}
	gitT(t, repo, "symbolic-ref", "refs/remotes/origin/HEAD", "refs/remotes/origin/main")
	// A stale pass (its verdict is not the sha's latest) on the default branch does not ship.
	if err := m.Store.DropReleasePasses(main, shaX); err != nil {
		t.Fatal(err)
	}
	pass(shaX, "ghost-verdict")
	gitT(t, repo, "update-ref", "refs/remotes/origin/main", shaX)
	if g := shipped(); g.ShippedSeq != 0 {
		t.Fatalf("a stale pass shipped: %+v", g)
	}
	pass(shaX, "")
	if g := shipped(); g.ShippedSeq != last.Seq || g.ShippedSHA != shaX {
		t.Fatalf("not shipped: %+v", g)
	}
	// The shipped pass is dropped; a fix Y passes and reaches origin/main: shipped again on Y.
	if err := m.Store.DropReleasePasses(main, shaX); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repo, "fix.txt"), []byte("fix\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitT(t, repo, "add", "fix.txt")
	gitT(t, repo, "commit", "-qm", "fix")
	shaY := gitT(t, repo, "rev-parse", "HEAD")
	if a, err := m.MergeGateCheck(repo, "gh pr merge 9", ""); err != nil || a.Refusal == "" {
		t.Fatalf("shipped pass dropped, not gated: %+v %v", a, err)
	}
	pass(shaY, "")
	gitT(t, repo, "update-ref", "refs/remotes/origin/main", shaY)
	if g := shipped(); g.ShippedSeq != last.Seq || g.ShippedSHA != shaY {
		t.Fatalf("not shipped again on Y: %+v", g)
	}
	if a, err := m.MergeGateCheck(repo, "gh pr merge 9", ""); err != nil || a.Refusal != "" {
		t.Fatalf("shipped again, still gated: %+v %v", a, err)
	}
}
