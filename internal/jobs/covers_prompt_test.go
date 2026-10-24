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

// The daemon puts a covered job's rows (only those: ID, verbatim, contract) and the Requirements /
// REDPROOF rules into its prompt, never the rest of the checklist or the BRIEF; a covered ID that is not
// a row of the checklist, or covers without a repo, refuse the start.
func TestCoversInjectedIntoPrompt(t *testing.T) {
	m := newManager(t)
	repo := briefRepo(t)
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

	// A checklist committed at the job's base is read from there.
	gitT(t, repo, "add", "brief/CHECKLIST.md")
	gitT(t, repo, "commit", "-qm", "checklist")
	if err := os.WriteFile(checklist.ChecklistPath(repo), []byte(strings.Replace(coverChecklist, "a login page", "WORKTREE_EDIT", 1)), 0o644); err != nil {
		t.Fatal(err)
	}
	committed := enqueue(t, m, "from the commit", func(r *StartRequest) { r.RepoPath = repo; r.Covers = []string{"R01"} })
	if c := get(t, m, committed.ID).CoverContract; c == nil || !strings.HasPrefix(c.Source, "committed at ") || c.Rows[0].Verbatim != "«a login page with | a pipe»" {
		t.Fatalf("committed checklist: %+v", c)
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
