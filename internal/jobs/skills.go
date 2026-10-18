package jobs

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/qwwiwi/agent-jobs/internal/skills"
	"github.com/qwwiwi/agent-jobs/internal/store"
)

// SkillsCodexNote is recorded for a codex job with selected skills: codex does not load them.
const SkillsCodexNote = "skills not supported for codex runner"

// SkillBudget is the per-job skill budget from config (defaults applied).
func (c Config) SkillBudget() skills.Budget {
	b := skills.Budget{MaxCount: c.SkillsMaxPerJob, MaxKB: c.SkillsMaxKB}
	if b.MaxCount == 0 {
		b.MaxCount = skills.DefaultMaxPerJob
	}
	if b.MaxKB == 0 {
		b.MaxKB = skills.DefaultMaxKB
	}
	return b
}

// skillRoots returns the configured roots a job may read skills from, and warnings for those left out:
// a root that is (or contains) the agent's state dir or the filesystem root is never used, since the
// guard protects every skill root against writes.
func (m *Manager) skillRoots() ([]string, []string) {
	var roots, warns []string
	for _, r := range m.Cfg.SkillRoots {
		c := filepath.Clean(r)
		if c == "/" || samePath(c, m.StateDir) || under(m.StateDir, c) || under(c, m.StateDir) {
			warns = append(warns, fmt.Sprintf("skill root %s: is /, holds or lies inside the agent-jobs state, skipped", r))
			continue
		}
		roots = append(roots, r)
	}
	return roots, warns
}

// Catalog loads the skill catalog of the configured roots (read fresh on every call: the catalog
// may change between jobs).
func (m *Manager) Catalog() *skills.Catalog {
	roots, warns := m.skillRoots()
	c := skills.Load(roots)
	c.Warnings = append(warns, c.Warnings...)
	return c
}

// SkillWarnings are the catalog's warnings (missing or invalid roots and entries) for status and doctor.
func (m *Manager) SkillWarnings() []string {
	if len(m.Cfg.SkillRoots) == 0 {
		return nil
	}
	return m.Catalog().Warnings
}

// selectSkills resolves a job's skills at job_start: the defaults for its kind and repository plus
// the edits; an unknown or deprecated name, or a selection over budget, refuses the start.
func (m *Manager) selectSkills(kind, repo string, edits []string) ([]string, error) {
	if len(m.Cfg.SkillRoots) == 0 && len(edits) == 0 {
		return nil, nil
	}
	base := ""
	if repo != "" {
		base = filepath.Base(repo)
	}
	sel, err := m.Catalog().Select(kind, base, edits, m.Cfg.SkillBudget())
	if err != nil {
		return nil, err
	}
	if len(sel) == 0 {
		return nil, nil
	}
	return skills.Names(sel), nil
}

// skillExcludeBegin / skillExcludeEnd frame a job's lines in the repository's info/exclude.
func skillExcludeBegin(id string) string {
	return "# agent-jobs " + id + " skill links (removed when the job ends)"
}
func skillExcludeEnd(id string) string { return "# agent-jobs " + id + " end" }

// excludePath is the repository's info/exclude (in the common git dir, shared by its worktrees).
func excludePath(repo string) (string, error) {
	return git(repo, "rev-parse", "--path-format=absolute", "--git-path", "info/exclude")
}

// writeExclude replaces job id's block in the exclude file with patterns (none = the block removed).
func writeExclude(path, id string, patterns []string) error {
	b, err := os.ReadFile(path)
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	var out []string
	in := false
	for _, l := range strings.Split(strings.TrimRight(string(b), "\n"), "\n") {
		switch {
		case l == skillExcludeBegin(id):
			in = true
		case in && l == skillExcludeEnd(id):
			in = false
		case !in && (l != "" || len(out) > 0):
			out = append(out, l)
		}
	}
	if len(patterns) > 0 {
		out = append(out, skillExcludeBegin(id))
		out = append(out, patterns...)
		out = append(out, skillExcludeEnd(id))
	}
	if len(out) == 0 && errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	text := strings.Join(out, "\n")
	if text != "" {
		text += "\n"
	}
	tmp := path + ".agent-jobs-" + id
	if err := os.WriteFile(tmp, []byte(text), 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// deliverSkills links the job's selected skills into its work dir for this attempt (E7): each as
// <work>/.claude/skills/<name> -> its catalog directory, hidden from git by the repository's
// info/exclude. A skill the repository already has stays the repository's; a codex job gets none.
// What was linked is recorded on the row (removeSkills undoes it when the attempt ends).
func (m *Manager) deliverSkills(j *store.Job, token, work string) error {
	j.SkillsDelivered, j.SkillsNote = nil, ""
	if len(j.Skills) == 0 {
		return nil
	}
	if j.Runner == store.RunnerCodex {
		j.SkillsNote = SkillsCodexNote
		m.Log.Printf("job %s: %s (%s)", j.ID, SkillsCodexNote, strings.Join(j.Skills, ", "))
		return m.Store.SetSkillsDelivered(j.ID, token, nil, j.SkillsNote)
	}
	cat := m.Catalog()
	var entries []skills.Entry
	for _, n := range j.Skills {
		e, ok := cat.Lookup(n)
		if !ok || e.Status == skills.StatusDeprecated {
			return fmt.Errorf("skill %q is no longer in the catalog (or deprecated); start the job again", n)
		}
		entries = append(entries, e)
	}
	d, err := skills.Link(work, entries, m.Cfg.SkillBudget().MaxKB)
	if err != nil {
		return fmt.Errorf("skills: %w", err)
	}
	j.SkillsDelivered = d.Linked
	if len(d.RepoOwned) > 0 {
		j.SkillsNote = "the repository's own .claude/skills kept (repository wins): " + strings.Join(d.RepoOwned, ", ")
		m.Log.Printf("job %s: %s", j.ID, j.SkillsNote)
	}
	if j.RepoPath != "" && len(d.Linked) > 0 {
		path, err := excludePath(j.RepoPath)
		if err == nil {
			var pats []string
			for _, n := range d.Linked {
				// No trailing slash: git sees a symlink as a file, not a directory.
				pats = append(pats, "/"+skills.LinkDir+"/"+n)
			}
			err = writeExclude(path, j.ID, pats)
		}
		if err != nil {
			skills.Unlink(work, d.Linked)
			j.SkillsDelivered = nil
			return fmt.Errorf("skills: info/exclude: %w", err)
		}
	}
	if err := m.Store.SetSkillsDelivered(j.ID, token, j.SkillsDelivered, j.SkillsNote); err != nil {
		m.removeSkills(j)
		return fmt.Errorf("record skills: %w", err)
	}
	if len(d.Linked) > 0 {
		m.Log.Printf("job %s: skills linked: %s", j.ID, strings.Join(d.Linked, ", "))
	}
	return nil
}

// removeSkills removes the skill links of the job's attempt and its info/exclude lines (idempotent).
// It runs when an attempt ends, before anything diffs the work dir: the links never reach a patch.
func (m *Manager) removeSkills(j *store.Job) {
	if len(j.SkillsDelivered) == 0 {
		return
	}
	work := filepath.Join(m.JobDir(j.ID), "work")
	if left := skills.Unlink(work, j.SkillsDelivered); len(left) > 0 {
		m.Log.Printf("job %s: skill links: %s", j.ID, strings.Join(left, "; "))
	}
	if j.RepoPath != "" {
		path, err := excludePath(j.RepoPath)
		if err == nil {
			err = writeExclude(path, j.ID, nil)
		}
		if err != nil {
			m.Log.Printf("job %s: remove skill lines from info/exclude: %v", j.ID, err)
		}
	}
}

// skillProtected are the paths a job never writes because of skills: every usable catalog root (as
// configured and resolved) and the job's links (writing through one would change the catalog).
func (m *Manager) skillProtected(j *store.Job) []string {
	roots, _ := m.skillRoots()
	var out []string
	for _, r := range roots {
		out = append(out, filepath.Clean(r))
		if real, err := filepath.EvalSymlinks(r); err == nil && real != filepath.Clean(r) {
			out = append(out, real)
		}
	}
	if j != nil {
		work := filepath.Join(m.JobDir(j.ID), "work")
		for _, n := range j.SkillsDelivered {
			out = append(out, skills.LinkPath(work, n))
		}
	}
	return out
}

// skillDenyRules are the file-tool deny rules for skillProtected (absolute paths: "//" + path).
func skillDenyRules(paths []string) []string {
	var out []string
	for _, p := range paths {
		abs := "//" + strings.TrimPrefix(filepath.ToSlash(p), "/")
		for _, tool := range []string{"Edit", "Write", "MultiEdit", "NotebookEdit"} {
			out = append(out, tool+"("+abs+")", tool+"("+abs+"/**)")
		}
	}
	return slices.Compact(out)
}

// skillsLine is the one brief line naming the job's skills ("" when none or a codex job).
func skillsLine(j *store.Job) string {
	if len(j.Skills) == 0 || j.Runner == store.RunnerCodex {
		return ""
	}
	return "Skills for this job (in .claude/skills; load one when it fits the task): " + strings.Join(j.Skills, ", ") + ".\n\n"
}
