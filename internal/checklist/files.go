package checklist

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"syscall"
)

// MaxFileBytes caps BRIEF.md and CHECKLIST.md as read by the tools.
const MaxFileBytes = 1 << 20

// ErrNoChecklist marks a repository without brief/CHECKLIST.md (checklist_init has not run).
var ErrNoChecklist = errors.New("no " + ChecklistRel + " (run checklist_init)")

// Files are the two brief files of a repository as read (nil content = the file does not exist).
type Files struct {
	Repo      string
	Brief     []byte
	Checklist []byte
}

// BriefPath and ChecklistPath are the files' paths in repo.
func BriefPath(repo string) string     { return filepath.Join(repo, filepath.FromSlash(BriefRel)) }
func ChecklistPath(repo string) string { return filepath.Join(repo, filepath.FromSlash(ChecklistRel)) }

// briefDir checks <repo>/brief: absent (ok=false) or a real directory, never a symlink.
func briefDir(repo string) (bool, error) {
	dir := filepath.Join(repo, "brief")
	fi, err := os.Lstat(dir)
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if !fi.IsDir() {
		return false, fmt.Errorf("%s is not a directory (a symlink or a file)", dir)
	}
	return true, nil
}

// readFile reads a brief file without following a symlink; nil when it does not exist.
func readFile(path string) ([]byte, error) {
	f, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("open %s (a symlink is refused): %w", path, err)
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil {
		return nil, err
	}
	if !fi.Mode().IsRegular() {
		return nil, fmt.Errorf("%s is not a regular file", path)
	}
	b, err := io.ReadAll(io.LimitReader(f, MaxFileBytes+1))
	if err != nil {
		return nil, err
	}
	if len(b) > MaxFileBytes {
		return nil, fmt.Errorf("%s is larger than %d bytes", path, MaxFileBytes)
	}
	if b == nil {
		b = []byte{}
	}
	return b, nil
}

// Read reads the brief files of repo (an absolute repository root).
func Read(repo string) (*Files, error) {
	if _, err := briefDir(repo); err != nil {
		return nil, err
	}
	f := &Files{Repo: repo}
	var err error
	if f.Brief, err = readFile(BriefPath(repo)); err != nil {
		return nil, err
	}
	if f.Checklist, err = readFile(ChecklistPath(repo)); err != nil {
		return nil, err
	}
	return f, nil
}

// renameHook is os.Rename; a test swaps it to fail the second rename and see the first undone.
var renameHook = os.Rename

// WriteBoth writes BRIEF.md and CHECKLIST.md of repo as one operation: both new contents go to temp
// files in brief/ first (synced), then are renamed into place; when the second rename fails, the
// first file is put back to old (its content before the call; nil = it did not exist), so a failed
// call leaves both or neither changed. A nil brief or checklist leaves that file as it is. The
// caller holds the checklist lock (no other writer runs).
func WriteBoth(repo string, brief, checklist, oldBrief []byte) error {
	ok, err := briefDir(repo)
	if err != nil {
		return err
	}
	if !ok {
		if err := os.Mkdir(filepath.Join(repo, "brief"), 0o755); err != nil {
			return err
		}
	}
	type pending struct{ tmp, dst string }
	var ps []pending
	cleanup := func() {
		for _, p := range ps {
			_ = os.Remove(p.tmp)
		}
	}
	for _, w := range []struct {
		dst  string
		data []byte
	}{{BriefPath(repo), brief}, {ChecklistPath(repo), checklist}} {
		if w.data == nil {
			continue
		}
		if fi, err := os.Lstat(w.dst); err == nil && !fi.Mode().IsRegular() {
			cleanup()
			return fmt.Errorf("%s is not a regular file (a symlink is refused)", w.dst)
		}
		tmp, err := writeTemp(filepath.Dir(w.dst), filepath.Base(w.dst), w.data)
		if err != nil {
			cleanup()
			return err
		}
		ps = append(ps, pending{tmp, w.dst})
	}
	for i, p := range ps {
		if err := renameHook(p.tmp, p.dst); err != nil {
			cleanup()
			if i == 1 {
				if rerr := restore(ps[0].dst, oldBrief); rerr != nil {
					return fmt.Errorf("write %s: %w; restoring %s failed: %v", p.dst, err, ps[0].dst, rerr)
				}
			}
			return fmt.Errorf("write %s: %w (nothing changed)", p.dst, err)
		}
	}
	return nil
}

// restore puts path back to old (nil = remove it).
func restore(path string, old []byte) error {
	if old == nil {
		return os.Remove(path)
	}
	tmp, err := writeTemp(filepath.Dir(path), filepath.Base(path), old)
	if err != nil {
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	return nil
}

// writeTemp writes data to a new synced temp file in dir and returns its path.
func writeTemp(dir, base string, data []byte) (string, error) {
	f, err := os.CreateTemp(dir, "."+base+".tmp-*")
	if err != nil {
		return "", err
	}
	name := f.Name()
	if _, err := f.Write(data); err != nil {
		f.Close()
		os.Remove(name)
		return "", err
	}
	if err := f.Chmod(0o644); err != nil {
		f.Close()
		os.Remove(name)
		return "", err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		os.Remove(name)
		return "", err
	}
	if err := f.Close(); err != nil {
		os.Remove(name)
		return "", err
	}
	return name, nil
}
