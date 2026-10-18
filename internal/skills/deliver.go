package skills

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
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
// has is left alone (RepoOwned). On an error nothing stays linked.
func Link(work string, entries []Entry, maxKB float64) (Delivery, error) {
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
		size, err := Measure(dir, e.Root)
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
	for _, e := range entries {
		link := LinkPath(work, e.Name)
		if _, err := os.Lstat(link); err == nil {
			d.RepoOwned = append(d.RepoOwned, e.Name)
			continue
		} else if !errors.Is(err, fs.ErrNotExist) {
			Unlink(work, d.Linked)
			return Delivery{}, err
		}
		if err := os.Symlink(e.Dir, link); err != nil {
			Unlink(work, d.Linked)
			return Delivery{}, fmt.Errorf("link skill %q: %w", e.Name, err)
		}
		d.Linked = append(d.Linked, e.Name)
	}
	return d, nil
}

// Unlink removes the links of names from work/.claude/skills (only symlinks: anything else at that
// path is the job's or the repository's and stays), then .claude/skills and .claude when they are
// left empty. It never follows a link: .claude or .claude/skills that is not a real directory is not
// entered. It returns what it could not remove.
func Unlink(work string, names []string) []string {
	var left []string
	claudeDir := filepath.Join(work, ".claude")
	skillsDir := filepath.Join(work, filepath.FromSlash(LinkDir))
	for _, p := range []string{claudeDir, skillsDir} {
		if ok, err := realDir(p); !ok || err != nil {
			if len(names) > 0 && err != nil {
				left = append(left, fmt.Sprintf("links not removed: %v", err))
			}
			return left
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
			left = append(left, fmt.Sprintf("%s: %v", n, err))
		case fi.Mode()&fs.ModeSymlink == 0:
			left = append(left, n+": no longer a link, left in place")
		default:
			if err := os.Remove(link); err != nil {
				left = append(left, fmt.Sprintf("%s: %v", n, err))
			}
		}
	}
	// Remove only when empty (os.Remove fails on a non-empty directory).
	_ = os.Remove(skillsDir)
	_ = os.Remove(claudeDir)
	return left
}
