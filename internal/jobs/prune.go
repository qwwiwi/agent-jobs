package jobs

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"syscall"
	"time"
	"unicode/utf8"

	"golang.org/x/sys/unix"

	"github.com/qwwiwi/agent-jobs/internal/store"
)

// Prune (reference, «Как мы убираем»): «Раз в сутки jobs prune чистит папки джоб, закончившихся
// больше 48 часов назад. Записи остаются (отчёт, патчи, base_commit) и маленькие текстовые файлы
// верхнего уровня; бинарники, кеши и сборки уходят.»
const (
	// PruneAge is how long after its finish a job's folder is left alone.
	PruneAge = 48 * time.Hour
	// PruneSmallTextBytes is the largest top-level text file that is kept besides the records.
	PruneSmallTextBytes = 1 << 20
)

// pruneRecords are always kept (whatever their size or content): the report and what the job changed.
var pruneRecords = []string{"report.md", "base_commit", "diffstat.txt", "commits.patch", "staged.patch",
	"uncommitted.patch", "untracked.tar"}

// PruneEntry is one path prune removes (or would remove, on a dry run).
type PruneEntry struct {
	Path   string `json:"path"`
	Bytes  int64  `json:"bytes"`
	Reason string `json:"reason"`
}

// PruneJob is what prune does to one job folder.
type PruneJob struct {
	ID      string       `json:"id"`
	Status  string       `json:"status"`
	Removed []PruneEntry `json:"removed"`
	// Archived are the files moved (gzipped) into the log archive at ArchiveDest.
	Archived    []PruneEntry `json:"archived"`
	ArchiveDest string       `json:"archive_dest,omitempty"`
	Kept        []string     `json:"kept"`
	Skipped     string       `json:"skipped,omitempty"` // why the folder (or its work dir) was left alone
	Error       string       `json:"error,omitempty"`
}

// PruneReport is the result of one prune pass.
type PruneReport struct {
	DryRun bool       `json:"dry_run"`
	Jobs   []PruneJob `json:"jobs"`
	Bytes  int64      `json:"bytes"` // total size of what was (or would be) removed
	// ArchivedBytes is the size of the files moved into the log archive (before gzip).
	ArchivedBytes int64 `json:"archived_bytes"`
	// ArchiveExpired are the archived months removed by the archive retention (log_archive_days).
	ArchiveExpired []PruneEntry `json:"archive_expired"`
	// RetentionOff says why no archived month is ever expired although log_archive_days > 0.
	RetentionOff string `json:"retention_off,omitempty"`
}

// Prune cleans the folders of jobs that finished more than PruneAge ago. Records (pruneRecords) and
// small top-level text files stay; every subdirectory, symlink, special file, binary and large file
// at the top level goes. A repo job whose worktree cleanup has not run yet is skipped entirely (its
// patches are not saved), and a worktree that cleanup deliberately kept for a human (repository
// drivers) is not touched. Symlinks are removed as links, never followed. With dryRun nothing is
// removed. Before anything is removed, the job's records for analysis move into the log archive
// (archiveJob); a failed archive keeps the files it would have moved or copied (the rest is pruned
// and the job reports the error). Archived months older than the retention are removed (archive roots with the
// marker only). Runs under the lifecycle lock, so it never races a tick's cleanup.
func (m *Manager) Prune(dryRun bool) (PruneReport, error) {
	rep := PruneReport{DryRun: dryRun, Jobs: []PruneJob{}, ArchiveExpired: []PruneEntry{}}
	unlock, err := m.lock()
	if err != nil {
		return rep, err
	}
	defer unlock()
	cutoff := m.Now().Add(-PruneAge)
	var errs []error
	for _, status := range []string{store.StatusSucceeded, store.StatusFailed, store.StatusTimeout, store.StatusCancelled} {
		list, err := m.Store.ListByStatus(status)
		if err != nil {
			return rep, err
		}
		for _, j := range list {
			if j.FinishedAt == nil || j.FinishedAt.After(cutoff) {
				continue
			}
			pj := m.pruneJob(j, dryRun)
			if pj.Error != "" {
				errs = append(errs, fmt.Errorf("job %s: %s", j.ID, pj.Error))
			}
			if len(pj.Removed) == 0 && len(pj.Archived) == 0 && pj.Error == "" && pj.Skipped == "" && pj.ArchiveDest == "" {
				continue // nothing left to prune
			}
			for _, e := range pj.Removed {
				rep.Bytes += e.Bytes
			}
			for _, e := range pj.Archived {
				rep.ArchivedBytes += e.Bytes
			}
			rep.Jobs = append(rep.Jobs, pj)
		}
	}
	if rep.ArchiveExpired, rep.RetentionOff, err = m.expireArchive(dryRun); err != nil {
		errs = append(errs, fmt.Errorf("archive retention: %w", err))
	}
	if !dryRun {
		m.Log.Printf("prune: %d job folders, %d bytes removed, %d bytes moved to the archive, %d archived months expired",
			len(rep.Jobs), rep.Bytes, rep.ArchivedBytes, len(rep.ArchiveExpired))
	}
	return rep, errors.Join(errs...)
}

func (m *Manager) pruneJob(j *store.Job, dryRun bool) PruneJob {
	pj := PruneJob{ID: j.ID, Status: j.Status, Removed: []PruneEntry{}, Archived: []PruneEntry{}, Kept: []string{}}
	dir := m.JobDir(j.ID)
	fi, err := os.Lstat(dir)
	switch {
	case errors.Is(err, os.ErrNotExist):
		return pj
	case err != nil:
		pj.Error = err.Error()
		return pj
	case !fi.IsDir():
		pj.Skipped = "job folder is not a real directory (" + fi.Mode().Type().String() + "), not touched"
		return pj
	}
	if j.RepoPath != "" && j.CleanedAt == nil {
		pj.Skipped = "worktree cleanup has not run yet (patches not saved), not touched"
		return pj
	}
	if !dryRun {
		// The job could chmod its own folder; its entries cannot be removed from a read-only dir.
		if err := writableJobDir(dir); err != nil {
			pj.Error = err.Error()
			return pj
		}
	}
	aj := m.archiveJob(j, dir, dryRun)
	// A failed archive keeps the files it would have moved or copied; the rest is still pruned,
	// so a full or missing archive disk does not stop prune from freeing space.
	archiveFailed := aj.Error != ""
	fail := func(err string) PruneJob {
		if archiveFailed {
			err = "log archive: " + aj.Error + "; " + err
		}
		pj.Error = err
		return pj
	}
	if archiveFailed {
		pj.Error = "log archive: " + aj.Error
	} else {
		pj.Archived = aj.Moved
		if !aj.Already {
			pj.ArchiveDest = aj.Dest
		}
	}
	ents, err := os.ReadDir(dir)
	if err != nil {
		return fail(err.Error())
	}
	for _, e := range ents {
		name := e.Name()
		p := filepath.Join(dir, name)
		if slices.ContainsFunc(pj.Archived, func(a PruneEntry) bool { return a.Path == p }) {
			continue // dry run: listed as moved to the archive
		}
		if archiveFailed && (slices.Contains(archiveGzip, name) || slices.Contains(archiveCopy, name)) {
			pj.Kept = append(pj.Kept, name)
			continue
		}
		keep, reason, err := pruneDecide(p, name, j)
		if err != nil {
			return fail(err.Error())
		}
		if keep {
			if reason != "" && pj.Skipped == "" {
				pj.Skipped = reason
			}
			pj.Kept = append(pj.Kept, name)
			continue
		}
		size := treeSize(p)
		if !dryRun {
			if err := removeTree(p); err != nil {
				return fail(err.Error())
			}
		}
		pj.Removed = append(pj.Removed, PruneEntry{Path: p, Bytes: size, Reason: reason})
	}
	return pj
}

// pruneDecide says whether a top-level entry of a job folder stays. A kept entry may carry a reason
// worth reporting (a worktree kept for a human); a removed one always says why it goes.
func pruneDecide(p, name string, j *store.Job) (keep bool, reason string, err error) {
	fi, err := os.Lstat(p)
	if err != nil {
		return false, "", err
	}
	mode := fi.Mode()
	switch {
	case mode&os.ModeSymlink != 0:
		return false, "symlink (removed as a link, not followed)", nil
	case mode.IsDir():
		if name == "work" && j.RepoPath != "" {
			if _, err := os.Lstat(filepath.Join(p, ".git")); err == nil {
				return true, "work/ is still a git worktree (cleanup kept it for a human), not touched", nil
			}
		}
		return false, "directory (work dir, caches, builds)", nil
	case !mode.IsRegular():
		return false, "not a regular file (" + mode.Type().String() + ")", nil
	}
	if slices.Contains(pruneRecords, name) {
		return true, "", nil
	}
	if fi.Size() > PruneSmallTextBytes {
		return false, fmt.Sprintf("large file (%d bytes > %d)", fi.Size(), PruneSmallTextBytes), nil
	}
	text, err := isTextFile(p)
	if err != nil {
		return false, "", err
	}
	if !text {
		return false, "binary file", nil
	}
	return true, "", nil
}

// isTextFile reads the head of a regular file without following a link or blocking: valid UTF-8
// without NUL bytes counts as text.
func isTextFile(p string) (bool, error) {
	f, err := os.OpenFile(p, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if err != nil {
		return false, err
	}
	defer f.Close()
	if fi, err := f.Stat(); err != nil || !fi.Mode().IsRegular() {
		return false, nil
	}
	b, err := io.ReadAll(io.LimitReader(f, 8<<10))
	if err != nil {
		return false, err
	}
	if bytes.IndexByte(b, 0) >= 0 {
		return false, nil
	}
	// A multi-byte rune cut at the read limit is not a reason to call the file binary.
	for i := 0; i < utf8.UTFMax && len(b) > 0 && !utf8.Valid(b); i++ {
		b = b[:len(b)-1]
	}
	return utf8.Valid(b), nil
}

// treeSize sums the sizes of everything under p (symlinks counted as links, never followed).
func treeSize(p string) int64 {
	var n int64
	_ = filepath.WalkDir(p, func(_ string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if fi, err := d.Info(); err == nil {
			n += fi.Size()
		}
		return nil
	})
	return n
}

// removeTree removes p and everything under it without following symlinks. A job may leave
// read-only directories behind (and the job dir holds a 0500 no-hooks/), so on a failure every real
// directory under p is made writable and the removal is retried once.
func removeTree(p string) error {
	if err := os.RemoveAll(p); err == nil {
		return nil
	}
	chmodTree(unix.AT_FDCWD, p, 0)
	return os.RemoveAll(p)
}

// maxChmodDepth bounds chmodTree's recursion.
const maxChmodDepth = 256

// chmodTree makes the directory name (relative to dirfd) and every directory under it 0700. Each
// directory is opened relative to its parent's descriptor with O_NOFOLLOW and changed through its
// own descriptor, so a directory swapped for a symlink mid-walk (a ghost still writing in the job
// dir) is never followed: the chmod cannot reach outside the tree. A directory that cannot be
// opened for reading is left as it is (the removal then reports the error).
func chmodTree(dirfd int, name string, depth int) {
	if depth > maxChmodDepth {
		return
	}
	fd, err := unix.Openat(dirfd, name, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		return
	}
	f := os.NewFile(uintptr(fd), name)
	defer f.Close()
	_ = unix.Fchmod(fd, 0o700)
	names, _ := f.Readdirnames(-1)
	for _, n := range names {
		var st unix.Stat_t
		if unix.Fstatat(fd, n, &st, unix.AT_SYMLINK_NOFOLLOW) == nil && st.Mode&unix.S_IFMT == unix.S_IFDIR {
			chmodTree(fd, n, depth+1)
		}
	}
}
