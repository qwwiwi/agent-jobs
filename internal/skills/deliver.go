package skills

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
)

// LinkDir is where a job's skills are linked, relative to its work dir: claude loads project skills
// from <cwd>/.claude/skills (--setting-sources project).
const LinkDir = ".claude/skills"

// LinkPath is the link of skill name in work.
func LinkPath(work, name string) string {
	return filepath.Join(work, filepath.FromSlash(LinkDir), name)
}

// realDir reports whether p is a real directory (not a symlink); missing is (false, nil).
func realDir(p string) (bool, error) {
	fi, err := os.Lstat(p)
	if errors.Is(err, fs.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if !fi.IsDir() {
		return false, fmt.Errorf("%s is not a real directory (%s)", filepath.Base(p), fi.Mode().Type())
	}
	return true, nil
}

// Delivery is what Link did.
type Delivery struct {
	// Linked are the skills linked into the work dir, in order.
	Linked []string
	// RepoOwned are skills the repository already has in its own .claude/skills: left untouched
	// (the repository wins), not linked.
	RepoOwned []string
}

// Link links every entry into work/.claude/skills/<name> -> <entry dir> (absolute). Each entry is
// checked again first: its directory still resolves to itself inside its root, holds SKILL.md, and
// the selection still fits the size budget (measured on the target). A name the repository already
// has is left alone (RepoOwned), except a name in adopt (a link of an earlier attempt that could not
// be removed): a symlink there to the entry's directory is taken over as linked, one elsewhere is
// replaced. On an error nothing linked by this call stays linked (adopted links stay).
func Link(work string, entries []Entry, maxKB float64, adopt []string) (Delivery, error) {
	var d Delivery
	if len(entries) == 0 {
		return d, nil
	}
	total := 0.0
	for _, e := range entries {
		dir, err := filepath.EvalSymlinks(e.Dir)
		if err != nil || dir != e.Dir || !within(dir, e.Root) {
			return d, fmt.Errorf("skill %q: its directory moved or leaves its root", e.Name)
		}
		if fi, err := os.Lstat(filepath.Join(dir, SkillFile)); err != nil || !fi.Mode().IsRegular() {
			return d, fmt.Errorf("skill %q: no regular %s", e.Name, SkillFile)
		}
		size, err := Measure(dir)
		if err != nil {
			return d, fmt.Errorf("skill %q: %w", e.Name, err)
		}
		total += size
	}
	if maxKB > 0 && total > maxKB {
		return d, fmt.Errorf("the selected skills now total %.0f KB, the limit is %.0f KB per job (skills_max_kb)", total, maxKB)
	}
	claudeDir := filepath.Join(work, ".claude")
	skillsDir := filepath.Join(work, filepath.FromSlash(LinkDir))
	for _, p := range []string{claudeDir, skillsDir} {
		ok, err := realDir(p)
		if err != nil {
			return d, fmt.Errorf("skills not linked: %w", err)
		}
		if !ok {
			if err := os.Mkdir(p, 0o700); err != nil {
				return d, fmt.Errorf("skills not linked: %w", err)
			}
		}
	}
	var created []string
	rollback := func() {
		_, _ = Unlink(work, created)
	}
	for _, e := range entries {
		link := LinkPath(work, e.Name)
		fi, err := os.Lstat(link)
		switch {
		case errors.Is(err, fs.ErrNotExist):
		case err != nil:
			rollback()
			return Delivery{}, err
		case fi.Mode()&fs.ModeSymlink != 0 && slices.Contains(adopt, e.Name):
			if target, err := os.Readlink(link); err == nil && target == e.Dir {
				d.Linked = append(d.Linked, e.Name)
				continue
			}
			if err := os.Remove(link); err != nil {
				rollback()
				return Delivery{}, fmt.Errorf("skill %q: stale link of an earlier attempt: %w", e.Name, err)
			}
		default:
			d.RepoOwned = append(d.RepoOwned, e.Name)
			continue
		}
		if err := os.Symlink(e.Dir, link); err != nil {
			rollback()
			return Delivery{}, fmt.Errorf("link skill %q: %w", e.Name, err)
		}
		created = append(created, e.Name)
		d.Linked = append(d.Linked, e.Name)
	}
	return d, nil
}

// Unlink removes the links of names from work/.claude/skills (only symlinks: anything else at that
// path is the job's or the repository's and stays, and is no longer tracked), then .claude/skills
// and .claude when they are left empty. It never follows a link: .claude or .claude/skills that is
// not a real directory is not entered. It returns the names whose link is still there (removal
// failed) and what went wrong.
func Unlink(work string, names []string) ([]string, error) {
	var left []string
	var errs []error
	claudeDir := filepath.Join(work, ".claude")
	skillsDir := filepath.Join(work, filepath.FromSlash(LinkDir))
	for _, p := range []string{claudeDir, skillsDir} {
		if ok, err := realDir(p); !ok || err != nil {
			// Missing: no link can be there. Not a real directory: the links are not at their path any
			// more (the job replaced the directory).
			if len(names) > 0 && err != nil {
				errs = append(errs, fmt.Errorf("links not removed: %w", err))
			}
			return nil, errors.Join(errs...)
		}
	}
	for _, n := range names {
		if !ValidName(n) {
			continue
		}
		link := LinkPath(work, n)
		fi, err := os.Lstat(link)
		switch {
		case errors.Is(err, fs.ErrNotExist):
		case err != nil:
			left = append(left, n)
			errs = append(errs, fmt.Errorf("%s: %w", n, err))
		case fi.Mode()&fs.ModeSymlink == 0:
			errs = append(errs, fmt.Errorf("%s: no longer a link, left in place", n))
		default:
			if err := os.Remove(link); err != nil {
				left = append(left, n)
				errs = append(errs, fmt.Errorf("%s: %w", n, err))
			}
		}
	}
	// Remove only when empty (os.Remove fails on a non-empty directory).
	_ = os.Remove(skillsDir)
	_ = os.Remove(claudeDir)
	return left, errors.Join(errs...)
}
