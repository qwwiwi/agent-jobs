package mcp

import (
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/qwwiwi/agent-jobs/internal/checklist"
)

// gitRepo makes a repository with one commit and returns its path and the commit id.
func gitRepo(t *testing.T) (string, string) {
	t.Helper()
	repo := t.TempDir()
	for _, args := range [][]string{{"init", "-q", "-b", "main"}, {"-c", "user.name=T", "-c", "user.email=t@example.com",
		"commit", "-q", "--allow-empty", "-m", "init"}} {
		if out, err := exec.Command("git", append([]string{"-C", repo}, args...)...).CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v %s", args, err, out)
		}
	}
	out, err := exec.Command("git", "-C", repo, "rev-parse", "HEAD").Output()
	if err != nil {
		t.Fatal(err)
	}
	if repo, err = filepath.EvalSymlinks(repo); err != nil {
		t.Fatal(err)
	}
	return repo, strings.TrimSpace(string(out))
}

// The checklist tools are closed to the job role (allowlist rule): a job cannot see or call them.
func TestChecklistToolsClosedToJobs(t *testing.T) {
	job, err := NewServer(RoleJob, Deps{JobID: "j1"})
	if err != nil {
		t.Fatal(err)
	}
	jcs := connect(t, job)
	names := toolNames(t, jcs)
	for _, n := range []string{"checklist_init", "checklist_get", "checklist_set"} {
		if !slices.Contains(OrchestratorOnly, n) || Allowed(RoleJob, n) || slices.Contains(names, n) {
			t.Fatalf("%s open to jobs", n)
		}
		if msg := callErr(t, jcs, n, map[string]any{"repo": "/tmp"}); !strings.Contains(msg, "not available") {
			t.Fatalf("job called %s: %q", n, msg)
		}
	}
}

// init → set rows → approve → manual edit voids → set again; secrets never reach the files; waivers
// are stored bound to the approved hash and the sha; the tools never commit.
func TestChecklistToolsFlow(t *testing.T) {
	m := newManager(t)
	now := time.Date(2026, 10, 24, 9, 0, 0, 0, time.UTC)
	m.Now = func() time.Time { return now }
	repo, head := gitRepo(t)
	cs := connect(t, mustServer(t, m))
	secret := "ghp_" + strings.Repeat("a1B2", 9)

	var v ChecklistView
	call(t, cs, "checklist_init", map[string]any{"repo": repo, "brief": "I want a login page. token " + secret}, &v)
	if !v.Written || v.Version != 1 || v.Approved || v.Reason != "not approved" || v.ID != filepath.Base(repo) || v.Redacted != 1 {
		t.Fatalf("init: %+v", v)
	}
	brief, _ := os.ReadFile(checklist.BriefPath(repo))
	if strings.Contains(string(brief), secret) || !strings.Contains(string(brief), "I want a login page.") {
		t.Fatalf("BRIEF.md:\n%s", brief)
	}
	if msg := callErr(t, cs, "checklist_init", map[string]any{"repo": repo, "brief": "again"}); !strings.Contains(msg, "already has") {
		t.Fatalf("second init: %q", msg)
	}

	call(t, cs, "checklist_set", map[string]any{"repo": repo, "rows": []map[string]any{
		{"id": "R01", "verbatim": "a login page", "contract": "live", "rationale": "brief"},
		{"id": "D01", "verbatim": "builds", "contract": "live", "rationale": "build"},
	}}, &v)
	if v.Version != 2 || v.Approved || len(v.Rows) != 2 || !slices.Equal(v.Live, []string{"R01"}) {
		t.Fatalf("rows: %+v", v)
	}
	if msg := callErr(t, cs, "checklist_set", map[string]any{"repo": repo,
		"waivers": []map[string]any{{"requirement": "R01", "sha": head, "quote": "ok"}}}); !strings.Contains(msg, "approved checklist") {
		t.Fatalf("waiver before approval: %q", msg)
	}
	storeSlicing(t, m, repo)
	call(t, cs, "checklist_set", map[string]any{"repo": repo, "approve": true, "approve_quote": "yes " + secret, "expect_version": 2}, &v)
	if !v.Approved || v.Version != 2 || v.ApprovedSHA256 != v.SHA256 || v.ApprovedAt != "2026-10-24T09:00:00Z" || v.ApprovedVersion != 2 || v.Redacted != 1 {
		t.Fatalf("approve: %+v", v)
	}
	approvedHash := v.SHA256
	if msg := callErr(t, cs, "checklist_set", map[string]any{"repo": repo,
		"waivers": []map[string]any{{"requirement": "D01", "sha": head, "quote": "ok"}}}); !strings.Contains(msg, "not a live requirement") {
		t.Fatalf("waiver on D row: %q", msg)
	}
	// The sha is accepted and ignored (a waiver binds to the hash and the requirement): no commit check,
	// a warning says so; a waiver without a sha is stored too.
	call(t, cs, "checklist_set", map[string]any{"repo": repo,
		"waivers": []map[string]any{{"requirement": "R01", "sha": head, "quote": "ship without it " + secret}}}, &v)
	if !v.Approved || v.WaiversAdded != 1 || len(v.Waivers) != 1 || v.Waivers[0].ChecklistSHA256 != approvedHash || v.Waivers[0].ChecklistVersion != 2 ||
		v.Waivers[0].SHA != head || strings.Contains(v.Waivers[0].Quote, secret) || len(v.Warnings) != 1 ||
		!strings.Contains(v.Warnings[0], "waiver sha ignored") {
		t.Fatalf("waiver: %+v", v)
	}
	var nosha, unknown ChecklistView
	call(t, cs, "checklist_set", map[string]any{"repo": repo,
		"waivers": []map[string]any{{"requirement": "R01", "quote": "ok"}}}, &nosha)
	if !nosha.Approved || nosha.WaiversAdded != 1 || len(nosha.Waivers) != 2 || len(nosha.Warnings) != 0 {
		t.Fatalf("waiver without a sha: %+v", nosha)
	}
	call(t, cs, "checklist_set", map[string]any{"repo": repo,
		"waivers": []map[string]any{{"requirement": "R01", "sha": strings.Repeat("0", 40), "quote": "ok"}}}, &unknown)
	if unknown.WaiversAdded != 1 || len(unknown.Warnings) != 1 {
		t.Fatalf("waiver on an unknown sha: %+v", unknown)
	}

	// A manual edit of a Rationale voids the approval; checklist_get says so.
	path := checklist.ChecklistPath(repo)
	text, _ := os.ReadFile(path)
	if err := os.WriteFile(path, []byte(strings.Replace(string(text), "| brief |", "| edited |", 1)), 0o644); err != nil {
		t.Fatal(err)
	}
	call(t, cs, "checklist_get", map[string]any{"repo": repo}, &v)
	if v.Approved || !strings.HasPrefix(v.Reason, "approval void") || len(v.Waivers) != 0 {
		t.Fatalf("after manual edit: %+v", v)
	}

	// An owner addition with a G row: BRIEF and CHECKLIST in one call, version up.
	call(t, cs, "checklist_set", map[string]any{"repo": repo,
		"rows":      []map[string]any{{"id": "G01", "verbatim": "dark mode", "contract": "live", "rationale": "addition 2026-10-24"}},
		"additions": []map[string]any{{"quote": "also dark mode"}}}, &v)
	if !v.BriefWritten || v.Version != 3 || v.Approved || !strings.HasPrefix(v.PreviousVoid, "approval void") {
		t.Fatalf("addition: %+v", v)
	}
	brief, _ = os.ReadFile(checklist.BriefPath(repo))
	if !strings.HasSuffix(string(brief), "## Additions\n\n### 2026-10-24\n\nalso dark mode\n") {
		t.Fatalf("BRIEF.md:\n%s", brief)
	}
	// The approval is in the store, with the owner's quote redacted.
	if a, err := m.Store.LatestApproval(repo); err != nil || a == nil || a.Version != 2 || a.ChecklistSHA256 != approvedHash ||
		strings.Contains(a.Quote, secret) || !strings.HasPrefix(a.Quote, "yes ") {
		t.Fatalf("stored approval: %+v %v", a, err)
	}
	// Re-approving version 3 makes it the latest; the waivers of version 2 do not carry over.
	storeSlicing(t, m, repo)
	call(t, cs, "checklist_set", map[string]any{"repo": repo, "approve": true, "approve_quote": "ok"}, &v)
	if !v.Approved || v.ApprovedVersion != 3 || len(v.Waivers) != 0 {
		t.Fatalf("approve v3: %+v", v)
	}
	// Nothing was committed.
	out, err := exec.Command("git", "-C", repo, "rev-parse", "HEAD").Output()
	if err != nil || strings.TrimSpace(string(out)) != head {
		t.Fatalf("HEAD moved: %s %v", out, err)
	}
	if msg := callErr(t, cs, "checklist_get", map[string]any{"repo": "relative"}); !strings.Contains(msg, "absolute") {
		t.Fatalf("relative repo: %q", msg)
	}
}

// writeFile replaces path's content in a test.
func writeFile(t *testing.T, path, s string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(s), 0o644); err != nil {
		t.Fatal(err)
	}
}

// Approval lives in the store, not in the file: a forged approved line with a recomputed hash approves
// nothing, an edited version voids a stored approval, and the stored approval of unchanged files holds.
func TestChecklistForgedApprovalRefused(t *testing.T) {
	m := newManager(t)
	repo, _ := gitRepo(t)
	cs := connect(t, mustServer(t, m))
	var v ChecklistView
	call(t, cs, "checklist_init", map[string]any{"repo": repo, "brief": "I want a login page."}, &v)
	call(t, cs, "checklist_set", map[string]any{"repo": repo, "rows": []map[string]any{
		{"id": "R01", "verbatim": "a login page", "contract": "live", "rationale": "brief"}}}, &v)
	path := checklist.ChecklistPath(repo)
	text, _ := os.ReadFile(path)
	brief, _ := os.ReadFile(checklist.BriefPath(repo))
	c, err := checklist.Parse(string(text))
	if err != nil {
		t.Fatal(err)
	}
	forged := strings.Replace(string(text), "- approved: no", "- approved: 2026-10-24T09:00:00Z owner sha256:"+checklist.Hash(c.Rows, brief), 1)
	writeFile(t, path, forged)
	call(t, cs, "checklist_get", map[string]any{"repo": repo}, &v)
	if v.Approved || !strings.HasPrefix(v.Reason, "approval void") {
		t.Fatalf("forged approved line accepted: %+v", v)
	}
	writeFile(t, path, string(text))

	storeSlicing(t, m, repo)
	call(t, cs, "checklist_set", map[string]any{"repo": repo, "approve": true, "approve_quote": "yes, that is it"}, &v)
	if !v.Approved || v.ApprovedVersion != 2 {
		t.Fatalf("approve: %+v", v)
	}
	approved, _ := os.ReadFile(path)
	writeFile(t, path, strings.Replace(string(approved), "- version: 2", "- version: 3", 1))
	call(t, cs, "checklist_get", map[string]any{"repo": repo}, &v)
	if v.Approved || !strings.Contains(v.Reason, "version 2 was approved") {
		t.Fatalf("edited version accepted: %+v", v)
	}
	writeFile(t, path, string(approved))
	call(t, cs, "checklist_get", map[string]any{"repo": repo}, &v)
	if !v.Approved {
		t.Fatalf("stored approval of unchanged files: %+v", v)
	}
	// approve needs the owner's words.
	if msg := callErr(t, cs, "checklist_set", map[string]any{"repo": repo, "approve": true}); !strings.Contains(msg, "approve_quote") {
		t.Fatalf("approve without a quote: %q", msg)
	}
	// The BRIEF text above ## Additions is never edited after the approval.
	writeFile(t, checklist.BriefPath(repo), strings.Replace(string(brief), "login", "logout", 1))
	if msg := callErr(t, cs, "checklist_set", map[string]any{"repo": repo, "rows": []map[string]any{
		{"id": "R02", "verbatim": "x", "contract": "live"}}}); !strings.Contains(msg, "above ## Additions") {
		t.Fatalf("edited BRIEF accepted: %q", msg)
	}
	// A fresh start takes a new id: the approved one stays bound to its BRIEF.
	if err := os.RemoveAll(filepath.Join(repo, "brief")); err != nil {
		t.Fatal(err)
	}
	if msg := callErr(t, cs, "checklist_init", map[string]any{"repo": repo, "brief": "other words"}); !strings.Contains(msg, "new id") {
		t.Fatalf("re-init with the approved id: %q", msg)
	}
	call(t, cs, "checklist_init", map[string]any{"repo": repo, "id": "second", "brief": "other words"}, &v)
	call(t, cs, "checklist_set", map[string]any{"repo": repo, "rows": []map[string]any{
		{"id": "R01", "verbatim": "other", "contract": "live"}}}, &v)
	if v.Approved || v.ID != "second" || !strings.HasPrefix(v.Reason, "not approved") {
		t.Fatalf("second checklist: %+v", v)
	}
}

// checklist_init refuses owner words with a literal «## Additions» line and writes nothing.
func TestChecklistInitRefusesAdditionsHeading(t *testing.T) {
	m := newManager(t)
	repo, _ := gitRepo(t)
	cs := connect(t, mustServer(t, m))
	msg := callErr(t, cs, "checklist_init", map[string]any{"repo": repo, "brief": "Format:\n```\n## Additions\n```\nI want CSV export."})
	if !strings.Contains(msg, "quote it differently") {
		t.Fatalf("init: %q", msg)
	}
	if _, err := os.Stat(checklist.BriefPath(repo)); !os.IsNotExist(err) {
		t.Fatalf("BRIEF.md written: %v", err)
	}
}
