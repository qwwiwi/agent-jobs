package jobs

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/qwwiwi/agent-jobs/internal/checklist"
	"github.com/qwwiwi/agent-jobs/internal/store"
)

// coverRowsOK reports whether j carries the snapshot of its covered rows in the order of its covers.
func coverRowsOK(j *store.Job) bool {
	c := j.CoverContract
	return c != nil && c.ChecklistID == "demo" && c.Version == 2 && len(c.Rows) == 2 && c.Rows[0].ID == "R03i" &&
		c.Rows[0].PlaceholderOK && c.Rows[1].ID == "R01" && c.Rows[1].Verbatim == "«a login page with | a pipe»" &&
		c.Rows[1].Contract == "live"
}

// coverChecklist is a brief/CHECKLIST.md with three rows; BRIEF.md carries a marker no prompt may show.
const coverChecklist = `# Checklist

- id: demo
- brief: brief/BRIEF.md
- version: 2
- approved: no

| ID | Brief (verbatim) | Contract | Basis |
|---|---|---|---|
| R01 | «a login page with \| a pipe» | live | BASIS_MARKER |
| R02 | SECOND_ROW_MARKER | live | brief |
| R03i | «sessions survive a restart» | live placeholder-ok | implied by R01 |
`

// briefRepo is a repository whose work tree has brief/BRIEF.md and brief/CHECKLIST.md (untracked, as
// the checklist tools leave them).
func briefRepo(t *testing.T) string {
	t.Helper()
	repo := newRepo(t)
	if err := os.Mkdir(filepath.Join(repo, "brief"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(checklist.BriefPath(repo), []byte("# Brief\n\nBRIEF_TEXT_MARKER I want a login page.\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(checklist.ChecklistPath(repo), []byte(coverChecklist), 0o644); err != nil {
		t.Fatal(err)
	}
	return repo
}

// approveBrief stores an owner approval of repo's work-tree brief files in m's store, as checklist_set
// approve does.
func approveBrief(t *testing.T, m *Manager, repo string) {
	t.Helper()
	f, err := checklist.Read(repo)
	if err != nil {
		t.Fatal(err)
	}
	c, err := checklist.Parse(string(f.Checklist))
	if err != nil {
		t.Fatal(err)
	}
	if err := m.Store.PutApproval(store.ChecklistApproval{Repo: mainWorktree(repo), ChecklistID: c.ID, Version: c.Version,
		ChecklistSHA256: checklist.Hash(c.Rows, f.Brief), BriefSHA256: checklist.BriefPrefixSHA256(f.Brief), Quote: "yes",
		ApprovedAt: m.Now()}); err != nil {
		t.Fatal(err)
	}
}

// The daemon puts a covered job's rows (only those: ID, verbatim, contract) and the Requirements /
// REDPROOF rules into its prompt, never the rest of the checklist or the BRIEF; a covered ID that is not
// a row of the checklist, or covers without a repo, refuse the start.
func TestCoversInjectedIntoPrompt(t *testing.T) {
	m := newManager(t)
	repo := briefRepo(t)
	approveBrief(t, m, repo)
	j := enqueue(t, m, "build the login", func(r *StartRequest) { r.RepoPath = repo; r.Covers = []string{"R03i", "R01"} })
	runToEnd(t, m, j.ID)
	b, err := os.ReadFile(filepath.Join(m.JobDir(j.ID), "prompt.md"))
	if err != nil {
		t.Fatal(err)
	}
	prompt := string(b)
	for _, want := range []string{
		"## Requirements this job covers",
		"- R03i · live placeholder-ok · «sessions survive a restart»",
		"- R01 · live · «a login page with | a pipe»",
		"checklist demo, version 2",
		"### Requirements",
		"REDPROOF R01",
		"ran:",
	} {
		if !strings.Contains(prompt, want) {
			t.Errorf("prompt lacks %q", want)
		}
	}
	for _, never := range []string{"SECOND_ROW_MARKER", "BRIEF_TEXT_MARKER", "BASIS_MARKER", "- R02 "} {
		if strings.Contains(prompt, never) {
			t.Errorf("prompt shows %q", never)
		}
	}
	// The rows come after the task and before the report contract.
	if i, k := strings.Index(prompt, "build the login"), strings.Index(prompt, "## Requirements this job covers"); i < 0 || k < i ||
		k > strings.Index(prompt, "## Report contract") {
		t.Errorf("rows not between the task and the report contract")
	}
	// The rows are the job's snapshot, taken at job_start.
	if !coverRowsOK(get(t, m, j.ID)) {
		t.Errorf("cover rows not stored with the job")
	}
	plain := enqueue(t, m, "no covers", func(r *StartRequest) { r.RepoPath = repo })
	runToEnd(t, m, plain.ID)
	if b, err := os.ReadFile(filepath.Join(m.JobDir(plain.ID), "prompt.md")); err != nil || strings.Contains(string(b), "## Requirements this job covers") {
		t.Fatalf("job without covers: %v", err)
	}

	for name, r := range map[string]StartRequest{
		"missing id": {RepoPath: repo, Covers: []string{"R01", "R07"}},
		"no repo":    {Covers: []string{"R01"}},
		"no file":    {RepoPath: newRepo(t), Covers: []string{"R01"}},
	} {
		r.Title, r.Kind, r.Brief, r.Model = "t", "general", "b", "sonnet"
		_, err := m.Enqueue(r)
		if err == nil || !strings.Contains(err.Error(), "covers") {
			t.Errorf("%s: %v", name, err)
		}
	}
	if _, err := m.Enqueue(StartRequest{Title: "t", Kind: "general", Brief: "b", Model: "sonnet", RepoPath: repo,
		Covers: []string{"R07"}}); err == nil || !strings.Contains(err.Error(), "R07") {
		t.Errorf("missing id not named: %v", err)
	}
}

// The covered rows come from the work tree's checklist (the one the checklist tools write) when it has
// one, else from the one committed at the base; either way it must be the repository's latest stored
// approval: a stale committed checklist does not shadow the approved work tree, and an unapproved or
// edited one refuses the start.
func TestCoversNeedLatestApproval(t *testing.T) {
	m := newManager(t)
	repo := briefRepo(t)
	start := func(covers ...string) (*store.Job, error) {
		return m.Enqueue(StartRequest{Title: "t", Kind: "general", Brief: "b", Model: "sonnet", RepoPath: repo, Covers: covers})
	}
	if _, err := start("R01"); err == nil || !strings.Contains(err.Error(), "not approved") {
		t.Fatalf("unapproved checklist: %v", err)
	}
	// A stale checklist committed at the base; the work tree holds the approved next version.
	gitT(t, repo, "add", "brief")
	gitT(t, repo, "commit", "-qm", "checklist")
	approved := strings.Replace(strings.Replace(coverChecklist, "- version: 2", "- version: 3", 1), "a login page", "APPROVED_WORDS", 1)
	if err := os.WriteFile(checklist.ChecklistPath(repo), []byte(approved), 0o644); err != nil {
		t.Fatal(err)
	}
	approveBrief(t, m, repo)
	j, err := start("R01")
	if err != nil {
		t.Fatal(err)
	}
	if c := j.CoverContract; c == nil || c.Version != 3 || !strings.HasPrefix(c.Source, "work tree") || !strings.Contains(c.Rows[0].Verbatim, "APPROVED_WORDS") {
		t.Fatalf("contract: %+v", c)
	}
	// An edit after the approval refuses the start.
	if err := os.WriteFile(checklist.ChecklistPath(repo), []byte(strings.Replace(approved, "APPROVED_WORDS", "EDITED", 1)), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := start("R01"); err == nil || !strings.Contains(err.Error(), "changed since version 3 was approved") {
		t.Fatalf("edited work tree: %v", err)
	}
	// Without a work-tree checklist the committed one is read, and it is not the latest approval.
	if err := os.Remove(checklist.ChecklistPath(repo)); err != nil {
		t.Fatal(err)
	}
	if _, err := start("R01"); err == nil || !strings.Contains(err.Error(), "committed at") || !strings.Contains(err.Error(), "version 3 was approved") {
		t.Fatalf("stale committed checklist: %v", err)
	}
}

// A covered row longer than the prompt allows refuses the start, naming the row; nothing is cut.
func TestCoversRowTooLong(t *testing.T) {
	m := newManager(t)
	repo := briefRepo(t)
	long := strings.Replace(coverChecklist, "«sessions survive a restart»", strings.Repeat("ж", MaxCoverRowRunes+1), 1)
	if err := os.WriteFile(checklist.ChecklistPath(repo), []byte(long), 0o644); err != nil {
		t.Fatal(err)
	}
	approveBrief(t, m, repo)
	_, err := m.Enqueue(StartRequest{Title: "t", Kind: "general", Brief: "b", Model: "sonnet", RepoPath: repo, Covers: []string{"R01", "R03i"}})
	if err == nil || !strings.Contains(err.Error(), "R03i") || !strings.Contains(err.Error(), "longer than") {
		t.Fatalf("long row: %v", err)
	}
	j, err := m.Enqueue(StartRequest{Title: "t", Kind: "general", Brief: "b", Model: "sonnet", RepoPath: repo, Covers: []string{"R01"}})
	if err != nil || len(j.CoverContract.Rows) != 1 {
		t.Fatalf("short row: %v", err)
	}
	// At the limit the whole row is in the prompt.
	c := &store.CoverContract{ChecklistID: "demo", Version: 2, Source: "work tree", Rows: []store.CoverRow{
		{ID: "R03i", Verbatim: strings.Repeat("ж", MaxCoverRowRunes), Contract: "live"}}}
	if out := RenderCoverContract(c, "rules"); !strings.Contains(out, strings.Repeat("ж", MaxCoverRowRunes)+"\n") || strings.Contains(out, "…") {
		t.Fatalf("row cut in the prompt")
	}
}

// Covered rows are redacted: a secret put into CHECKLIST.md by hand reaches neither the job's snapshot
// nor its prompt.
func TestCoversRedacted(t *testing.T) {
	m := newManager(t)
	repo := briefRepo(t)
	secret := "ghp_" + strings.Repeat("a1B2", 9)
	if err := os.WriteFile(checklist.ChecklistPath(repo), []byte(strings.Replace(coverChecklist, "a login page", "a login page "+secret, 1)), 0o644); err != nil {
		t.Fatal(err)
	}
	f, _ := checklist.Read(repo)
	text, _ := Redact(string(f.Checklist))
	c, _ := checklist.Parse(text)
	if err := m.Store.PutApproval(store.ChecklistApproval{Repo: mainWorktree(repo), ChecklistID: "demo", Version: 2,
		ChecklistSHA256: checklist.Hash(c.Rows, f.Brief), BriefSHA256: checklist.BriefPrefixSHA256(f.Brief), Quote: "yes",
		ApprovedAt: m.Now()}); err != nil {
		t.Fatal(err)
	}
	j := enqueue(t, m, "covered", func(r *StartRequest) { r.RepoPath = repo; r.Covers = []string{"R01"} })
	if strings.Contains(j.CoverContract.Rows[0].Verbatim, secret) {
		t.Fatalf("snapshot kept the secret: %+v", j.CoverContract.Rows)
	}
	raw := &store.CoverContract{ChecklistID: "demo", Version: 2, Source: "work tree", Rows: []store.CoverRow{
		{ID: "R01", Verbatim: "x " + secret, Contract: "live"}}}
	if strings.Contains(RenderCoverContract(raw, "rules"), secret) {
		t.Fatal("prompt shows the secret")
	}
}
