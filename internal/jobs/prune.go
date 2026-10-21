package jobs

import (
	"bytes"
	"context"
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
// and the job reports the error). Archived months older than the retention are removed first (archive roots with the
// marker only). Each job is pruned under the lifecycle lock, taken per job and its row re-read
// under it, so prune never races a tick's cleanup or a resume, and a long prune (the daily run)
// never holds up the ticks between jobs.
func (m *Manager) Prune(dryRun bool) (PruneReport, error) {
	return m.PruneContext(context.Background(), dryRun)
}

// PruneContext is Prune bounded by ctx: the archive retention runs first, then once ctx is done no
// further job is started (the one in progress finishes) and the error says so; the report covers
// what was done.
func (m *Manager) PruneContext(ctx context.Context, dryRun bool) (PruneReport, error) {
	rep := PruneReport{DryRun: dryRun, Jobs: []PruneJob{}, ArchiveExpired: []PruneEntry{}}
	cutoff := m.Now().Add(-PruneAge)
	var ids []string
	for _, status := range pruneStatuses {
		list, err := m.Store.ListByStatus(status)
		if err != nil {
			return rep, err
		}
		for _, j := range list {
			if j.FinishedAt != nil && !j.FinishedAt.After(cutoff) {
				ids = append(ids, j.ID)
			}
		}
	}
	// Archive retention runs first, so a per-job pass cut short by ctx never starves it.
	var errs []error
	unlock, err := m.lock()
	if err != nil {
		return rep, err
	}
	rep.ArchiveExpired, rep.RetentionOff, err = m.expireArchive(dryRun)
	unlock()
	if err != nil {
		errs = append(errs, fmt.Errorf("archive retention: %w", err))
	}
	for _, id := range ids {
		if err := ctx.Err(); err != nil {
			errs = append(errs, fmt.Errorf("prune stopped before job %s: %w", id, err))
			break
		}
		pj, ok, err := m.pruneOne(id, cutoff, dryRun)
		if err != nil {
			return rep, errors.Join(append(errs, err)...)
		}
		if !ok {
			continue
		}
		if pj.Error != "" {
			errs = append(errs, fmt.Errorf("job %s: %s", id, pj.Error))
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
	if !dryRun {
		m.Log.Printf("prune: %d job folders, %d bytes removed, %d bytes moved to the archive, %d archived months expired",
			len(rep.Jobs), rep.Bytes, rep.ArchivedBytes, len(rep.ArchiveExpired))
	}
	return rep, errors.Join(errs...)
}

// pruneStatuses are the statuses prune cleans (stopped jobs are resumable and stay as they are).
var pruneStatuses = []string{store.StatusSucceeded, store.StatusFailed, store.StatusTimeout, store.StatusCancelled}

// pruneOne prunes one job under the lifecycle lock, after re-reading its row: a job resumed or
// otherwise changed since the listing (no longer terminal, or finished after cutoff) is left alone
// (ok false).
func (m *Manager) pruneOne(id string, cutoff time.Time, dryRun bool) (PruneJob, bool, error) {
	unlock, err := m.lock()
	if err != nil {
		return PruneJob{}, false, err
	}
	defer unlock()
	j, err := m.Store.Get(id)
	if err != nil {
		return PruneJob{}, false, err
	}
	if !slices.Contains(pruneStatuses, j.Status) || j.FinishedAt == nil || j.FinishedAt.After(cutoff) {
		return PruneJob{}, false, nil
	}
	pj := m.pruneJob(j, dryRun)
	if pj.Error == "" && (j.RepoPath == "" || j.CleanedAt != nil) {
		m.pruneSessions(j, dryRun, &pj)
	}
	if !dryRun && len(pj.Removed)+len(pj.Archived) > 0 {
		m.recordDirSize(id) // the disk status reads the recorded size
	}
	return pj, true, nil
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
	if m.spawnedGroupAlive(j) {
		pj.Skipped = fmt.Sprintf("process group %d of the job is still alive, not touched", j.PGID)
		return pj
	}
	if m.acceptanceGroupAlive(j) {
		pj.Skipped = fmt.Sprintf("acceptance process group %d of the job is still alive, not touched", j.AcceptancePGID)
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
		if archiveFailed && (slices.Contains(archiveGzip, name) || slices.Contains(archiveCopy, name) || name == attemptsDir) {
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

// pruneSessions removes the claude session files of a pruned job (DEVIATIONS #15): the transcript
// <id>.jsonl and the <id>/ directory of every session the job ran in (each attempt's). The ids are
// the job's own (assigned by the daemon), so nothing else in the claude config dir is touched.
func (m *Manager) pruneSessions(j *store.Job, dryRun bool, pj *PruneJob) {
	ids := []string{j.SessionID}
	if atts, err := m.Store.Attempts(j.ID); err == nil {
		for _, a := range atts {
			ids = append(ids, a.SessionID)
		}
	}
	slices.Sort(ids)
	for _, id := range slices.Compact(ids) {
		if !ValidSessionID(id) {
			continue
		}
		paths, err := SessionPaths(id)
		if err != nil {
			pj.Error = err.Error()
			return
		}
		for _, p := range paths {
			size := treeSize(p)
			if !dryRun {
				if err := removeTree(p); err != nil {
					pj.Error = err.Error()
					return
				}
			}
			pj.Removed = append(pj.Removed, PruneEntry{Path: p, Bytes: size, Reason: "claude session of the job"})
		}
	}
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

// Bounds of one job folder measurement (jobDirSize), which runs synchronously when a job settles: a
// walk stops after this many entries or this long, and the size is then recorded as partial.
var (
	dirWalkMaxEntries = 200_000
	dirWalkBudget     = 2 * time.Second
)

// DirSize is a measured job folder: Bytes in all, TmpBytes of them in its temp dir (JobTmpDir).
// Partial means the bounded walk stopped early: both are lower bounds.
type DirSize struct {
	Bytes    int64
	TmpBytes int64
	Partial  bool
}

// dirWalk is one bounded walk: every directory is opened relative to its parent's descriptor with
// O_NOFOLLOW and every entry is lstat'ed through it, so no symlink is followed, not even a directory
// swapped for one mid-walk.
type dirWalk struct {
	left     int
	deadline time.Time
	partial  bool
}

// each calls fn with the lstat of every entry of the open directory fd (closed on return), until the
// walk runs out of entries or time.
func (w *dirWalk) each(fd int, fn func(name string, st *unix.Stat_t)) {
	f := os.NewFile(uintptr(fd), "dir")
	defer f.Close()
	for !w.partial {
		names, err := f.Readdirnames(512)
		for _, name := range names {
			if w.left--; w.left < 0 || time.Now().After(w.deadline) {
				w.partial = true
				return
			}
			var st unix.Stat_t
			if unix.Fstatat(fd, name, &st, unix.AT_SYMLINK_NOFOLLOW) == nil {
				fn(name, &st)
			}
		}
		if err != nil {
			return // io.EOF, or a directory that cannot be read further
		}
	}
}

// size sums the sizes of everything in the directory name under dirfd (symlinks counted as links).
// A directory that cannot be opened counts as its entry alone.
func (w *dirWalk) size(dirfd int, name string, depth int) int64 {
	if depth > maxChmodDepth {
		w.partial = true
		return 0
	}
	fd, err := openDirNoFollow(dirfd, name)
	if err != nil {
		return 0
	}
	var n int64
	w.each(fd, func(c string, st *unix.Stat_t) {
		n += st.Size
		if st.Mode&unix.S_IFMT == unix.S_IFDIR {
			n += w.size(fd, c, depth+1)
		}
	})
	return n
}

// jobDirSize measures job j's folder dir, the worktree of a repo job left out (it is the repository's
// checkout, removed by cleanup), within dirWalkMaxEntries and dirWalkBudget. The job can write its
// folder: a dir that is not a real directory (a symlink) is refused, and nothing under it is followed.
func jobDirSize(j *store.Job, dir string) (DirSize, error) {
	fd, err := openDirNoFollow(unix.AT_FDCWD, dir)
	if err != nil {
		return DirSize{}, err
	}
	w := &dirWalk{left: dirWalkMaxEntries, deadline: time.Now().Add(dirWalkBudget)}
	var s DirSize
	w.each(fd, func(name string, st *unix.Stat_t) {
		if name == "work" && j.RepoPath != "" {
			return
		}
		n := st.Size
		isDir := st.Mode&unix.S_IFMT == unix.S_IFDIR
		if isDir {
			n += w.size(fd, name, 1)
		}
		s.Bytes += n
		if name == JobTmpDir && isDir {
			s.TmpBytes = n
		}
	})
	s.Partial = w.partial
	return s, nil
}

// recordDirSize measures the folder of a job (when its attempt ended, again after a prune) and records
// it on the row (the stats row, the job-done line and the disk status show it). A failure is logged,
// never fatal.
func (m *Manager) recordDirSize(id string) {
	j, err := m.Store.Get(id)
	if err == nil {
		var s DirSize
		if s, err = jobDirSize(j, m.JobDir(id)); err == nil {
			err = m.Store.SetDirBytes(id, s.Bytes, s.TmpBytes, s.Partial)
		}
	}
	if err != nil {
		m.Log.Printf("job %s: record job dir size: %v", id, err)
	}
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
