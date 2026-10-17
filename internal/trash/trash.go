// Package trash is reversible deletion for the agent (reference, «MCP-тулы», group «Файлы»:
// «trash_move, trash_restore (обратимое удаление вместо rm)»).
//
// trash_move renames a path into <trash>/<id>/item and records where it came from in
// <trash>/<id>/meta.json; trash_restore renames it back. Nothing is copied or deleted: a move is one
// rename(2) on the same filesystem (a path on another filesystem is refused, not copied).
//
// Boundaries:
//   - only absolute paths without a `..` segment, strictly inside one of the allowed roots; the root
//     itself, an ancestor of a root and anything inside, or containing, a protected dir (the agent's
//     state dir, which holds the trash and the job database) is refused;
//   - symlinks are never followed out: the parent directory is resolved and must lie inside a root
//     (also after resolution), and the last component is not followed – a symlink is moved as the
//     link itself. The rename runs relative to a descriptor of the resolved parent that is opened
//     without following a link and re-checked (/proc/self/fd) after it is open, so a directory swapped
//     for a symlink between the check and the rename is caught;
//   - nothing is overwritten: the move and the restore use renameat2(RENAME_NOREPLACE), and a restore
//     whose original path is occupied is refused.
package trash

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"time"

	"golang.org/x/sys/unix"
)

// Trash is one agent's trash.
type Trash struct {
	// Dir is the trash directory (created on first use, 0700).
	Dir string
	// Roots are the directories trash_move may take paths from (and trash_restore put them back into).
	Roots []string
	// Protected are directories nothing may be taken from and that may not be moved themselves (the
	// agent's state dir).
	Protected []string
	// Now is the clock (tests).
	Now func() time.Time
}

// Entry is one trashed item.
type Entry struct {
	ID           string `json:"id"`
	OriginalPath string `json:"original_path"`
	MovedAt      string `json:"moved_at"`
	Type         string `json:"type"` // file | dir | symlink | other
	Bytes        int64  `json:"bytes"`
	TrashPath    string `json:"trash_path,omitempty"`
}

const (
	itemName = "item"
	metaName = "meta.json"
)

var reID = regexp.MustCompile(`^[0-9]{6}-[0-9]{6}-[0-9a-f]{8}$`)

func (t *Trash) now() time.Time {
	if t.Now != nil {
		return t.Now()
	}
	return time.Now()
}

func newID(now time.Time) string {
	b := make([]byte, 4)
	_, _ = rand.Read(b)
	return now.Format("060102-150405") + "-" + hex.EncodeToString(b)
}

// within reports whether p lies strictly inside dir (both clean, absolute, resolved).
func within(p, dir string) bool {
	rel, err := filepath.Rel(dir, p)
	return err == nil && rel != "." && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) && !filepath.IsAbs(rel)
}

func resolvedDirs(dirs []string) []string {
	var out []string
	for _, d := range dirs {
		if !filepath.IsAbs(d) {
			continue
		}
		if r, err := filepath.EvalSymlinks(d); err == nil {
			out = append(out, filepath.Clean(r))
		}
	}
	return out
}

// target is a checked path: its resolved parent and its last component.
type target struct {
	parent, base, path string
}

// check validates a user path against the boundaries and returns its resolved parent and base.
func (t *Trash) check(path string) (target, error) {
	if !filepath.IsAbs(path) {
		return target{}, fmt.Errorf("refused: %q is not an absolute path", path)
	}
	if slices.Contains(strings.Split(path, string(filepath.Separator)), "..") {
		return target{}, fmt.Errorf("refused: %q has a `..` segment", path)
	}
	clean := filepath.Clean(path)
	base := filepath.Base(clean)
	if base == string(filepath.Separator) || base == "." {
		return target{}, fmt.Errorf("refused: %q names no file", path)
	}
	parent, err := filepath.EvalSymlinks(filepath.Dir(clean))
	if err != nil {
		return target{}, fmt.Errorf("parent of %s: %w", clean, err)
	}
	real := filepath.Join(parent, base)
	roots := resolvedDirs(t.Roots)
	if len(roots) == 0 {
		return target{}, errors.New("refused: no allowed roots are configured (trash_roots)")
	}
	inRoot := false
	for _, r := range roots {
		if real == r || within(r, real) {
			return target{}, fmt.Errorf("refused: %s is an allowed root or contains one", real)
		}
		if within(real, r) {
			inRoot = true
		}
	}
	if !inRoot {
		return target{}, fmt.Errorf("refused: %s (resolved from %s) is outside the allowed roots %v", real, clean, roots)
	}
	for _, p := range resolvedDirs(append(slices.Clone(t.Protected), t.Dir)) {
		if real == p || within(real, p) || within(p, real) {
			return target{}, fmt.Errorf("refused: %s is or touches the protected dir %s", real, p)
		}
	}
	return target{parent: parent, base: base, path: real}, nil
}

// openDir opens dir without following a link and proves (via /proc/self/fd) that the descriptor is
// that directory, so a component swapped for a symlink after the check is caught.
func openDir(dir string) (int, error) {
	fd, err := unix.Open(dir, unix.O_PATH|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		return -1, fmt.Errorf("open %s: %w", dir, err)
	}
	got, err := os.Readlink("/proc/self/fd/" + strconv.Itoa(fd))
	if err != nil || filepath.Clean(got) != filepath.Clean(dir) {
		_ = unix.Close(fd)
		return -1, fmt.Errorf("refused: %s changed while it was being opened (now %q, %v)", dir, got, err)
	}
	return fd, nil
}

func kindOf(fi fs.FileInfo) string {
	switch m := fi.Mode(); {
	case m&fs.ModeSymlink != 0:
		return "symlink"
	case m.IsDir():
		return "dir"
	case m.IsRegular():
		return "file"
	}
	return "other"
}

func treeSize(p string) int64 {
	var n int64
	_ = filepath.WalkDir(p, func(_ string, d fs.DirEntry, err error) error {
		if err == nil {
			if fi, err := d.Info(); err == nil {
				n += fi.Size()
			}
		}
		return nil
	})
	return n
}

func (t *Trash) ensureDir() (string, error) {
	if err := os.MkdirAll(t.Dir, 0o700); err != nil {
		return "", err
	}
	fi, err := os.Lstat(t.Dir)
	if err != nil {
		return "", err
	}
	if !fi.IsDir() {
		return "", fmt.Errorf("trash dir %s is not a real directory", t.Dir)
	}
	return filepath.EvalSymlinks(t.Dir)
}

func renameNoReplace(fromFd int, from string, toFd int, to string) error {
	err := unix.Renameat2(fromFd, from, toFd, to, unix.RENAME_NOREPLACE)
	if errors.Is(err, unix.EXDEV) {
		return errors.New("refused: the path is on another filesystem than the trash (a move would be a copy)")
	}
	return err
}

// Move puts path into the trash and returns its entry.
func (t *Trash) Move(path string) (Entry, error) {
	tg, err := t.check(path)
	if err != nil {
		return Entry{}, err
	}
	fi, err := os.Lstat(tg.path)
	if err != nil {
		return Entry{}, err
	}
	dir, err := t.ensureDir()
	if err != nil {
		return Entry{}, fmt.Errorf("trash dir: %w", err)
	}
	now := t.now()
	e := Entry{ID: newID(now), OriginalPath: tg.path, MovedAt: now.UTC().Format(time.RFC3339), Type: kindOf(fi)}
	if e.Type == "dir" {
		e.Bytes = treeSize(tg.path)
	} else {
		e.Bytes = fi.Size()
	}
	edir := filepath.Join(dir, e.ID)
	if err := os.Mkdir(edir, 0o700); err != nil {
		return Entry{}, err
	}
	fail := func(err error) (Entry, error) {
		_ = os.RemoveAll(edir) // only the meta file: the item was not moved
		return Entry{}, err
	}
	// The meta is written first: an item never sits in the trash without its original path.
	b, _ := json.MarshalIndent(e, "", "  ")
	if err := os.WriteFile(filepath.Join(edir, metaName), append(b, '\n'), 0o600); err != nil {
		return fail(err)
	}
	pfd, err := openDir(tg.parent)
	if err != nil {
		return fail(err)
	}
	defer unix.Close(pfd)
	efd, err := openDir(edir)
	if err != nil {
		return fail(err)
	}
	defer unix.Close(efd)
	if err := renameNoReplace(pfd, tg.base, efd, itemName); err != nil {
		return fail(fmt.Errorf("move %s: %w", tg.path, err))
	}
	e.TrashPath = filepath.Join(edir, itemName)
	return e, nil
}

// readMeta reads <entry>/meta.json without following a link or blocking.
func readMeta(edir string) (Entry, error) {
	var e Entry
	f, err := os.OpenFile(filepath.Join(edir, metaName), os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if err != nil {
		return e, err
	}
	defer f.Close()
	if fi, err := f.Stat(); err != nil || !fi.Mode().IsRegular() {
		return e, fmt.Errorf("%s is not a regular file", filepath.Join(edir, metaName))
	}
	b, err := io.ReadAll(io.LimitReader(f, 64<<10))
	if err != nil {
		return e, err
	}
	return e, json.Unmarshal(b, &e)
}

// Restore puts an entry back at its original path. It refuses when that path is occupied, when its
// parent is gone, or when the path is no longer inside an allowed root.
func (t *Trash) Restore(id string) (Entry, error) {
	if !reID.MatchString(id) {
		return Entry{}, fmt.Errorf("trash id %q is malformed", id)
	}
	dir, err := t.ensureDir()
	if err != nil {
		return Entry{}, fmt.Errorf("trash dir: %w", err)
	}
	edir := filepath.Join(dir, id)
	fi, err := os.Lstat(edir)
	if errors.Is(err, os.ErrNotExist) {
		return Entry{}, fmt.Errorf("trash entry %s not found", id)
	}
	if err != nil {
		return Entry{}, err
	}
	if !fi.IsDir() {
		return Entry{}, fmt.Errorf("trash entry %s is not a directory", id)
	}
	e, err := readMeta(edir)
	if err != nil {
		return Entry{}, fmt.Errorf("trash entry %s: %w", id, err)
	}
	if e.ID != id {
		return Entry{}, fmt.Errorf("trash entry %s: meta names id %q", id, e.ID)
	}
	tg, err := t.check(e.OriginalPath)
	if err != nil {
		return Entry{}, fmt.Errorf("restore to %s: %w", e.OriginalPath, err)
	}
	if _, err := os.Lstat(tg.path); err == nil {
		return Entry{}, fmt.Errorf("refused: %s exists; move it away first (nothing is overwritten)", tg.path)
	}
	efd, err := openDir(edir)
	if err != nil {
		return Entry{}, err
	}
	defer unix.Close(efd)
	pfd, err := openDir(tg.parent)
	if err != nil {
		return Entry{}, err
	}
	defer unix.Close(pfd)
	if err := renameNoReplace(efd, itemName, pfd, tg.base); err != nil {
		if errors.Is(err, unix.EEXIST) {
			return Entry{}, fmt.Errorf("refused: %s exists; nothing is overwritten", tg.path)
		}
		return Entry{}, fmt.Errorf("restore %s: %w", tg.path, err)
	}
	if err := os.Remove(filepath.Join(edir, metaName)); err == nil {
		_ = os.Remove(edir)
	}
	e.TrashPath = ""
	return e, nil
}

// List returns the trash entries, newest first, at most limit.
func (t *Trash) List(limit int) ([]Entry, error) {
	ents, err := os.ReadDir(t.Dir)
	if errors.Is(err, os.ErrNotExist) {
		return []Entry{}, nil
	}
	if err != nil {
		return nil, err
	}
	out := []Entry{}
	for _, d := range ents {
		if !d.IsDir() || !reID.MatchString(d.Name()) {
			continue
		}
		e, err := readMeta(filepath.Join(t.Dir, d.Name()))
		if err != nil || e.ID != d.Name() {
			continue
		}
		e.TrashPath = filepath.Join(t.Dir, d.Name(), itemName)
		out = append(out, e)
	}
	slices.SortFunc(out, func(a, b Entry) int { return strings.Compare(b.ID, a.ID) })
	if limit > 0 && len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}
