package jobs

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"syscall"

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
	for _, r := range m.Config().SkillRoots {
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
	if len(m.Config().SkillRoots) == 0 {
		return nil
	}
	return m.Catalog().Warnings
}

// selectSkills resolves a job's skills at job_start: the defaults for its kind and repository plus
// the edits; an unknown or deprecated name, or a selection over budget, refuses the start.
func (m *Manager) selectSkills(kind, repo string, edits []string) ([]string, error) {
	if len(m.Config().SkillRoots) == 0 && len(edits) == 0 {
		return nil, nil
	}
	base := ""
	if repo != "" {
		base = filepath.Base(repo)
	}
	sel, err := m.Catalog().Select(kind, base, edits, m.Config().SkillBudget())
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

// excludeLockFile is the lock file in a repository's common git dir that serializes updates of its
// info/exclude.
const excludeLockFile = "agent-jobs-exclude.lock"

// updateExclude replaces job id's block in repo's info/exclude (none = removed) under an exclusive
// flock on <common git dir>/agent-jobs-exclude.lock: the file is shared by every worktree of the
// repository, so jobs of one repository (in this daemon or another process) rewrite it one at a time
// and none drops another's block.
func updateExclude(repo, id string, patterns []string) error {
	path, err := excludePath(repo)
	if err != nil {
		return err
	}
	common, err := git(repo, "rev-parse", "--path-format=absolute", "--git-common-dir")
	if err != nil {
		return err
	}
	if real, err := filepath.EvalSymlinks(common); err == nil {
		common = real
	}
	f, err := os.OpenFile(filepath.Join(common, excludeLockFile), os.O_CREATE|os.O_RDWR|syscall.O_NOFOLLOW, 0o644)
	if err != nil {
		return fmt.Errorf("open exclude lock: %w", err)
	}
	defer f.Close()
	for {
		err = syscall.Flock(int(f.Fd()), syscall.LOCK_EX)
		if !errors.Is(err, syscall.EINTR) {
			break
		}
	}
	if err != nil {
		return fmt.Errorf("flock exclude lock: %w", err)
	}
	defer func() { _ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN) }()
	return writeExclude(path, id, patterns)
}

// committedSkillLinks returns the delivered skill links (names) that the job put into its index or
// into a commit since its base: info/exclude only hides them from git status, git add -f still takes
// them, and a symlink into a skill catalog must never reach a patch or the repository.
func committedSkillLinks(work, base string, names []string) ([]string, error) {
	var specs []string
	for _, n := range names {
		if skills.ValidName(n) {
			specs = append(specs, ":(literal)"+skills.LinkDir+"/"+n)
		}
	}
	if len(specs) == 0 {
		return nil, nil
	}
	staged, err := gitRaw(work, append([]string{"ls-files", "--cached", "-z", "--"}, specs...)...)
	if err != nil {
		return nil, err
	}
	committed, err := gitRaw(work, append([]string{"log", "--format=", "--name-only", "--no-renames", "-z",
		base + "..HEAD", "--"}, specs...)...)
	if err != nil {
		return nil, err
	}
	var out []string
	for _, p := range strings.Split(string(staged)+"\x00"+string(committed), "\x00") {
		if p != "" && !slices.Contains(out, p) {
			out = append(out, p)
		}
	}
	slices.Sort(out)
	return out, nil
}

// checkSkillLinks refuses (an error) the attempt's result when the job staged or committed one of
// the skill links delivered to it.
func (m *Manager) checkSkillLinks(j *store.Job, delivered []string) error {
	if j.RepoPath == "" || len(delivered) == 0 {
		return nil
	}
	work := filepath.Join(m.JobDir(j.ID), "work")
	found, err := committedSkillLinks(work, j.BaseCommit, delivered)
	if err != nil {
		return fmt.Errorf("skill links in the job's index and commits not checked: %w", err)
	}
	if len(found) > 0 {
		return fmt.Errorf("the job staged or committed its skill link %s (a symlink into the skill catalog); "+
			"result refused, drop it from the index and the commits", strings.Join(found, ", "))
	}
	return nil
}

// deliverSkills links the job's selected skills into its work dir for this attempt (E7): each as
// <work>/.claude/skills/<name> -> its catalog directory, hidden from git by the repository's
// info/exclude. A skill the repository already has stays the repository's; a link an earlier attempt
// left behind (leftSkillLinks) is adopted, not taken for the repository's; a codex job gets none.
// What was linked is recorded on the row (removeSkills undoes it when the attempt ends). Until that
// is recorded, j.SkillsDelivered keeps the earlier attempt's list, so a failed delivery still lets
// removeSkills retry the links left behind.
func (m *Manager) deliverSkills(j *store.Job, token, work string) error {
	j.SkillsNote = ""
	if len(j.Skills) == 0 {
		j.SkillsDelivered = nil
		return nil
	}
	if j.Runner == store.RunnerCodex {
		j.SkillsDelivered, j.SkillsNote = nil, SkillsCodexNote
		m.Log.Printf("job %s: %s (%s)", j.ID, SkillsCodexNote, strings.Join(j.Skills, ", "))
		return m.Store.SetSkillsDelivered(j.ID, token, nil, j.SkillsNote)
	}
	cat := m.Catalog()
	var entries []skills.Entry
	for _, n := range j.Skills {
		e, ok := cat.Lookup(n)
		if !ok || e.Status == skills.StatusDeprecated {
			cause := "skill missing from all daemon skill roots"
			if len(m.Config().SkillRoots) == 0 {
				cause = "daemon catalog has no skill roots (config skill_roots)"
			} else if ok {
				cause = "skill is deprecated in the daemon catalog"
			}
			detail := ""
			if cfg, err := LoadConfig(m.StateDir); err != nil {
				detail = "; config.json does not load; daemon keeps the last good config"
			} else if !slices.Equal(cfg.SkillRoots, m.Config().SkillRoots) {
				detail = "; daemon config differs from the file (skill_roots)"
			}
			if len(cat.Warnings) > 0 {
				detail += "; catalog warnings: " + strings.Join(cat.Warnings, "; ")
			}
			return fmt.Errorf("skill %q: %s%s; check config.json and catalog warnings before starting again", n, cause, detail)
		}
		entries = append(entries, e)
	}
	adopt := m.leftSkillLinks(j, work)
	d, err := skills.Link(work, entries, m.Config().SkillBudget().MaxKB, adopt)
	if err != nil {
		return fmt.Errorf("skills: %w", err)
	}
	j.SkillsDelivered = d.Linked
	if len(d.RepoOwned) > 0 {
		j.SkillsNote = "the repository's own .claude/skills kept (repository wins): " + strings.Join(d.RepoOwned, ", ")
		m.Log.Printf("job %s: %s", j.ID, j.SkillsNote)
	}
	if j.RepoPath != "" && len(d.Linked) > 0 {
		if err := updateExclude(j.RepoPath, j.ID, excludePatterns(d.Linked)); err != nil {
			left, uerr := skills.Unlink(work, d.Linked)
			if uerr != nil {
				m.Log.Printf("job %s: skill links: %v", j.ID, uerr)
			}
			j.SkillsDelivered = left
			return fmt.Errorf("skills: info/exclude: %w", err)
		}
	}
	if err := m.Store.SetSkillsDelivered(j.ID, token, j.SkillsDelivered, j.SkillsNote); err != nil {
		_ = m.removeSkills(j)
		return fmt.Errorf("record skills: %w", err)
	}
	if len(d.Linked) > 0 {
		m.Log.Printf("job %s: skills linked: %s", j.ID, strings.Join(d.Linked, ", "))
	}
	return nil
}

// excludePatterns are the info/exclude lines of the links of names.
func excludePatterns(names []string) []string {
	var pats []string
	for _, n := range names {
		// No trailing slash: git sees a symlink as a file, not a directory.
		pats = append(pats, "/"+skills.LinkDir+"/"+n)
	}
	return pats
}

// leftSkillLinks are the links of the job's recorded skills (j.SkillsDelivered) still in work: a
// symlink at .claude/skills/<name> that resolves into a usable catalog root. Such a link is the
// daemon's (an earlier removal failed), not the repository's.
func (m *Manager) leftSkillLinks(j *store.Job, work string) []string {
	roots, _ := m.skillRoots()
	var out []string
	for _, n := range j.SkillsDelivered {
		if !skills.ValidName(n) {
			continue
		}
		link := skills.LinkPath(work, n)
		if fi, err := os.Lstat(link); err != nil || fi.Mode()&fs.ModeSymlink == 0 {
			continue
		}
		for _, r := range roots {
			if under(link, r) {
				out = append(out, n)
				break
			}
		}
	}
	return out
}

// removeSkills removes the skill links of the job's attempt and its info/exclude lines (idempotent).
// It runs when an attempt ends, before anything diffs the work dir: the links never reach a patch.
// A link that cannot be removed keeps its exclude line and stays on the row's delivered list, so a
// later call (the next attempt adopts it, cleanup retries) removes it; the error says what is left.
func (m *Manager) removeSkills(j *store.Job) error {
	if len(j.SkillsDelivered) == 0 {
		return nil
	}
	work := filepath.Join(m.JobDir(j.ID), "work")
	left, err := skills.Unlink(work, j.SkillsDelivered)
	if err != nil {
		err = fmt.Errorf("skill links: %w", err)
		m.Log.Printf("job %s: %v", j.ID, err)
	}
	if j.RepoPath != "" {
		if xerr := updateExclude(j.RepoPath, j.ID, excludePatterns(left)); xerr != nil {
			xerr = fmt.Errorf("skill lines in info/exclude: %w", xerr)
			m.Log.Printf("job %s: %v", j.ID, xerr)
			err = errors.Join(err, xerr)
		}
	}
	if len(left) > 0 {
		err = errors.Join(err, fmt.Errorf("skill links still in the work dir: %s", strings.Join(left, ", ")))
	}
	return err
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
