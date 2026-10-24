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
		{"id": "R01", "verbatim": "a login page", "contract": "live", "basis": "brief"},
		{"id": "D01", "verbatim": "builds", "contract": "live", "basis": "build"},
	}}, &v)
	if v.Version != 2 || v.Approved || len(v.Rows) != 2 || !slices.Equal(v.Live, []string{"R01"}) {
		t.Fatalf("rows: %+v", v)
	}
	if msg := callErr(t, cs, "checklist_set", map[string]any{"repo": repo,
		"waivers": []map[string]any{{"requirement": "R01", "sha": head, "quote": "ok"}}}); !strings.Contains(msg, "approved checklist") {
		t.Fatalf("waiver before approval: %q", msg)
	}
	call(t, cs, "checklist_set", map[string]any{"repo": repo, "approve": true, "expect_version": 2}, &v)
	if !v.Approved || v.Version != 2 || v.ApprovedSHA256 != v.SHA256 || v.ApprovedAt != "2026-10-24T09:00:00Z" {
		t.Fatalf("approve: %+v", v)
	}
	approvedHash := v.SHA256
	if msg := callErr(t, cs, "checklist_set", map[string]any{"repo": repo,
		"waivers": []map[string]any{{"requirement": "D01", "sha": head, "quote": "ok"}}}); !strings.Contains(msg, "not a live requirement") {
		t.Fatalf("waiver on D row: %q", msg)
	}
	if msg := callErr(t, cs, "checklist_set", map[string]any{"repo": repo,
		"waivers": []map[string]any{{"requirement": "R01", "sha": strings.Repeat("0", 40), "quote": "ok"}}}); !strings.Contains(msg, "not a commit") {
		t.Fatalf("waiver on unknown sha: %q", msg)
	}
	call(t, cs, "checklist_set", map[string]any{"repo": repo,
		"waivers": []map[string]any{{"requirement": "R01", "sha": head, "quote": "ship without it " + secret}}}, &v)
	if !v.Approved || v.WaiversAdded != 1 || len(v.Waivers) != 1 || v.Waivers[0].ChecklistSHA256 != approvedHash ||
		v.Waivers[0].SHA != head || strings.Contains(v.Waivers[0].Quote, secret) {
		t.Fatalf("waiver: %+v", v)
	}

	// A manual edit of a Basis voids the approval; checklist_get says so.
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
		"rows":      []map[string]any{{"id": "G01", "verbatim": "dark mode", "contract": "live", "basis": "addition 2026-10-24"}},
		"additions": []map[string]any{{"quote": "also dark mode"}}}, &v)
	if !v.BriefWritten || v.Version != 3 || v.Approved || !strings.HasPrefix(v.PreviousVoid, "approval void") {
		t.Fatalf("addition: %+v", v)
	}
	brief, _ = os.ReadFile(checklist.BriefPath(repo))
	if !strings.HasSuffix(string(brief), "## Additions\n\n### 2026-10-24\n\nalso dark mode\n") {
		t.Fatalf("BRIEF.md:\n%s", brief)
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
