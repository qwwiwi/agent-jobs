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
	"github.com/qwwiwi/agent-jobs/internal/jobs"
	"github.com/qwwiwi/agent-jobs/internal/store"
)

// coverRepo is a repository whose work tree has a checklist with rows R01 and R03i.
func coverRepo(t *testing.T) string {
	t.Helper()
	repo, _ := gitRepo(t)
	if err := os.Mkdir(filepath.Join(repo, "brief"), 0o755); err != nil {
		t.Fatal(err)
	}
	list := "# Checklist\n\n- id: demo\n- brief: brief/BRIEF.md\n- version: 2\n- approved: no\n\n" + checklist.TableHeader +
		"\n|---|---|---|---|\n| R01 | a login page | live | brief |\n| R03i | sessions survive | live | implied |\n"
	writeFile(t, checklist.BriefPath(repo), "# Brief\n\nwords\n")
	writeFile(t, checklist.ChecklistPath(repo), list)
	return repo
}

// approveCovers stores an owner approval of repo's work-tree brief files, as checklist_set approve does.
func approveCovers(t *testing.T, m *jobs.Manager, repo string) {
	t.Helper()
	f, err := checklist.Read(repo)
	if err != nil {
		t.Fatal(err)
	}
	c, err := checklist.Parse(string(f.Checklist))
	if err != nil {
		t.Fatal(err)
	}
	if err := m.Store.PutApproval(store.ChecklistApproval{Repo: repo, ChecklistID: c.ID, Version: c.Version,
		ChecklistSHA256: checklist.Hash(c.Rows, f.Brief), BriefSHA256: checklist.BriefPrefixSHA256(f.Brief), Quote: "yes",
		ApprovedAt: time.Now()}); err != nil {
		t.Fatal(err)
	}
}

// job_start takes covers (stored, shown by job_status); job_result of a covered job reads its
// Requirements block (claimed done lines) and its commits' Requirements: trailers; a job without
// covers shows neither.
func TestJobCoversRequirements(t *testing.T) {
	m := newManager(t)
	cs := connect(t, mustServer(t, m))
	covered := coverRepo(t)
	approveCovers(t, m, covered)
	var v JobView
	call(t, cs, "job_start", startArgs(func(a map[string]any) { a["repo"] = covered; a["covers"] = []string{"R01", "R03i"} }), &v)
	if !slices.Equal(v.Covers, []string{"R01", "R03i"}) {
		t.Fatalf("job_start covers: %+v", v.Covers)
	}
	var st JobView
	call(t, cs, "job_status", map[string]any{"id": v.ID}, &st)
	if !slices.Equal(st.Covers, []string{"R01", "R03i"}) {
		t.Fatalf("job_status covers: %+v", st.Covers)
	}
	if msg := callErr(t, cs, "job_start", startArgs(func(a map[string]any) { a["repo"] = covered; a["covers"] = []string{"A1"} })); !strings.Contains(msg, "covers") {
		t.Fatalf("bad covers: %q", msg)
	}
	if msg := callErr(t, cs, "job_start", startArgs(func(a map[string]any) { a["repo"] = covered; a["covers"] = []string{"R02"} })); !strings.Contains(msg, "R02 is not a row") {
		t.Fatalf("covers missing from the checklist: %q", msg)
	}

	// A finished covered job whose commit (saved under its ref, worktree gone) carries a trailer.
	repo, base := gitRepo(t)
	git := func(args ...string) string {
		t.Helper()
		out, err := exec.Command("git", append([]string{"-C", repo, "-c", "user.name=T", "-c", "user.email=t@example.com"}, args...)...).CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %v %s", args, err, out)
		}
		return strings.TrimSpace(string(out))
	}
	git("commit", "-q", "--allow-empty", "-m", "Add login\n\nRequirements: R01, R03i")
	head := git("rev-parse", "HEAD")
	git("reset", "-q", "--hard", base)
	now := time.Now()
	report := "## Summary\nok\n\n## Findings / Changes\n### Requirements\n" +
		"- R01 · done · a.go:1 · ran: go test → ok\n- R03i · partial · form only\n\n## Not done / uncertain\nnothing\n"
	for i, covers := range [][]string{{"R01", "R03i"}, nil} {
		id := []string{"261024-120000-c0fe", "261024-120000-c0ff"}[i]
		done := &store.Job{ID: id, Agent: "test", Title: "done", Kind: "general", Brief: "b", Status: store.StatusSucceeded,
			Model: "opus", MaxTurns: 10, MaxBudgetUSD: 1, TimeoutMinutes: 5, CreatedAt: now, StartedAt: &now, FinishedAt: &now,
			RepoPath: repo, BaseCommit: base, Covers: covers}
		if err := m.Store.Insert(done); err != nil {
			t.Fatal(err)
		}
		git("update-ref", "refs/agent-jobs/jobs/"+id, head)
		if err := os.MkdirAll(m.JobDir(id), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(m.JobDir(id), "report.md"), []byte(report), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	var res JobResultOutput
	call(t, cs, "job_result", map[string]any{"id": "261024-120000-c0fe"}, &res)
	r := res.Requirements
	if r == nil || !r.Block || len(r.Lines) != 2 || len(r.Claimed) != 0 || !r.Lines[0].Verified ||
		!slices.Equal(r.Trailer, []string{"R01", "R03i"}) || r.TrailerNote != "" || !slices.Equal(res.Covers, []string{"R01", "R03i"}) {
		t.Fatalf("job_result requirements: %+v %+v", r, res.Covers)
	}
	var plain JobResultOutput
	call(t, cs, "job_result", map[string]any{"id": "261024-120000-c0ff"}, &plain)
	if plain.Requirements != nil || plain.Covers != nil {
		t.Fatalf("job without covers: %+v", plain)
	}
}
