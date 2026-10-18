package jobs

import (
	"compress/gzip"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"syscall"
	"time"

	"github.com/qwwiwi/agent-jobs/internal/store"
)

// Log archive: when prune cleans a job folder, the job's records for later analysis move into
// <archive>/<YYYY-MM>/<id>/ (stream.jsonl, stderr.log and stream-resume-failed.jsonl gzipped,
// prompt.md, report.md, diffstat.txt copied, meta.json = the job row; for a resumed job also every
// earlier attempt's attempts/<n>/ run files and report.attempt-<n>.md). The archive lies outside the job folders, so cleaning jobs never
// touches it; prune deletes archived months older than log_archive_days.
var (
	// archiveGzip are moved: gzipped into the archive, then removed from the job folder.
	archiveGzip = []string{"stream.jsonl", "stderr.log", "stream-resume-failed.jsonl"}
	// archiveCopy are copied: the job folder keeps them (report.md and diffstat.txt are prune records).
	archiveCopy = []string{"prompt.md", "report.md", "diffstat.txt"}
	// archiveAttemptGzip / archiveAttemptCopy are the same for each earlier attempt's attempts/<n>/.
	archiveAttemptGzip = []string{"stream.jsonl", "stderr.log", "stream-resume-failed.jsonl"}
	archiveAttemptCopy = []string{resumePromptFile, fallbackPromptFile}
	reAttemptReport    = regexp.MustCompile(`^report\.attempt-[0-9]+\.md$`)
	reMonth            = regexp.MustCompile(`^[0-9]{4}-[0-9]{2}$`)
)

// archiveMarker is written into an archive root the archive created. Retention and the 0700 chmod
// act only on a root that holds it: an existing folder the owner pointed log_archive_dir at (a
// synced folder with its own month dirs) is archived into but never expired or chmodded.
const archiveMarker = ".agent-jobs-archive"

// syncFile fsyncs an archived file before it is closed (a variable so tests can see the calls).
var syncFile = (*os.File).Sync

// ArchiveJob is what the archive does to one job.
type ArchiveJob struct {
	ID    string       `json:"id"`
	Dest  string       `json:"dest"`
	Moved []PruneEntry `json:"moved"` // files gzipped into the archive and removed from the job folder
	// Already is set when the job was archived by an earlier pass (only leftovers are removed).
	Already bool   `json:"already,omitempty"`
	Error   string `json:"error,omitempty"`
}

// ArchiveReport is the result of one archive pass (`agent-jobs archive`, also part of prune).
type ArchiveReport struct {
	DryRun  bool         `json:"dry_run"`
	Dir     string       `json:"dir"`
	Days    int          `json:"days"` // retention; 0 = forever
	Jobs    []ArchiveJob `json:"jobs"`
	Expired []PruneEntry `json:"expired"` // archived months past the retention
	// RetentionOff says why no month is ever expired although days > 0 (a root without the marker).
	RetentionOff string `json:"retention_off,omitempty"`
}

// ArchiveDir returns the configured archive root (default <state>/archive).
func (m *Manager) ArchiveDir() string {
	if m.Cfg.LogArchiveDir != "" {
		return m.Cfg.LogArchiveDir
	}
	return m.defaultArchiveDir()
}

// defaultArchiveDir is <state>/archive: agent-jobs owns it, whoever created it.
func (m *Manager) defaultArchiveDir() string {
	return filepath.Join(m.StateDir, "archive")
}

// archiveMonth is the archive month of a job: the UTC month it was created, so every attempt of a
// job maps to the same dir.
func archiveMonth(j *store.Job) string {
	return j.CreatedAt.UTC().Format("2006-01")
}

// archiveJobDir returns <archive>/<YYYY-MM>/<id>: the existing archive of the job, else the dir a new
// archive goes to (the created month). Earlier builds keyed the month by finished_at; such an
// archive is still found.
func (m *Manager) archiveJobDir(j *store.Job) string {
	d, _ := m.existingArchiveDir(j)
	return d
}

// existingArchiveDir returns the archive dir of a job and whether something exists there (any
// entry: even a non-directory blocks a new archive). Without one it returns the created-month dir.
func (m *Manager) existingArchiveDir(j *store.Job) (string, bool) {
	dirs := []string{filepath.Join(m.ArchiveDir(), archiveMonth(j), j.ID)}
	if j.FinishedAt != nil {
		if legacy := filepath.Join(m.ArchiveDir(), j.FinishedAt.UTC().Format("2006-01"), j.ID); legacy != dirs[0] {
			dirs = append(dirs, legacy)
		}
	}
	for _, d := range dirs {
		if _, err := os.Lstat(d); err == nil {
			return d, true
		}
	}
	return dirs[0], false
}

// errArchived refuses a resume of a job whose records prune already moved into the archive: a
// later attempt's records could not be archived next to them, so the job is over.
func (m *Manager) errArchived(j *store.Job) error {
	if d, ok := m.existingArchiveDir(j); ok {
		return fmt.Errorf("job %s: its records were archived by prune (%s); start a new job instead: %w", j.ID, d, store.ErrImmutable)
	}
	return nil
}

// realDir checks that p is a real directory (lstat: a symlink is refused, never followed). With
// create, a missing p is made (0700, parents included). ok=false when p is missing and not created.
func realDir(p string, create bool) (ok bool, err error) {
	fi, err := os.Lstat(p)
	if errors.Is(err, os.ErrNotExist) && create {
		if err := os.MkdirAll(p, 0o700); err != nil {
			return false, err
		}
		fi, err = os.Lstat(p)
	}
	switch {
	case errors.Is(err, os.ErrNotExist):
		return false, nil
	case err != nil:
		return false, err
	case fi.Mode()&os.ModeSymlink != 0:
		return false, fmt.Errorf("%s is a symlink: refusing it as an archive path (point log_archive_dir at the real directory)", p)
	case !fi.IsDir():
		return false, fmt.Errorf("%s is not a directory (%s)", p, fi.Mode().Type())
	}
	return true, nil
}

// archiveRoot checks the archive root (a real directory) and keeps a root of ours private: the logs
// carry tool output, so it is 0700. With mkdir a missing root is made with the marker. With write a
// root of ours is chmodded, and the default root <state>/archive gets a missing marker back (left
// without one by an earlier build, or by a marker write that failed after the mkdir). owned reports
// the marker; any other existing root without it is used as it is (never chmodded).
func (m *Manager) archiveRoot(mkdir, write bool) (ok, owned bool, err error) {
	root := m.ArchiveDir()
	_, err = os.Lstat(root)
	fresh := errors.Is(err, os.ErrNotExist) && mkdir
	if fresh {
		if err := os.MkdirAll(root, 0o700); err != nil {
			return false, false, err
		}
	}
	if ok, err = realDir(root, false); err != nil || !ok {
		return ok, false, err
	}
	marker := filepath.Join(root, archiveMarker)
	fi, err := os.Lstat(marker)
	if errors.Is(err, os.ErrNotExist) && (fresh || write && filepath.Clean(root) == m.defaultArchiveDir()) {
		if err := createSynced(marker, []byte("Log archive of agent-jobs: prune expires the <YYYY-MM> dirs here.\n")); err != nil {
			return true, false, err
		}
		fi, err = os.Lstat(marker)
	}
	owned = err == nil && fi.Mode().IsRegular()
	if write && owned {
		if fi, err := os.Lstat(root); err == nil && fi.Mode().Perm() != 0o700 {
			if err := os.Chmod(root, 0o700); err != nil {
				return false, owned, err
			}
		}
	}
	return true, owned, nil
}

// Archive moves the records of every job prune would clean now into the archive and enforces the
// archive retention. With dryRun nothing changes. Runs under the lifecycle lock.
func (m *Manager) Archive(dryRun bool) (ArchiveReport, error) {
	unlock, err := m.lock()
	if err != nil {
		return ArchiveReport{}, err
	}
	defer unlock()
	rep := ArchiveReport{DryRun: dryRun, Dir: m.ArchiveDir(), Days: m.Cfg.ArchiveDays(), Jobs: []ArchiveJob{}}
	var errs []error
	err = m.eachPrunable(func(j *store.Job, dir string) {
		aj := m.archiveJob(j, dir, dryRun)
		if aj.Error != "" {
			errs = append(errs, fmt.Errorf("job %s: %s", j.ID, aj.Error))
		}
		if len(aj.Moved) > 0 || aj.Error != "" || !aj.Already {
			rep.Jobs = append(rep.Jobs, aj)
		}
	})
	if err != nil {
		return rep, err
	}
	if rep.Expired, rep.RetentionOff, err = m.expireArchive(dryRun); err != nil {
		errs = append(errs, err)
	}
	return rep, errors.Join(errs...)
}

// eachPrunable calls f for every terminal job finished more than PruneAge ago whose folder prune may
// clean: a real directory, and for a repo job a worktree cleanup that has run.
func (m *Manager) eachPrunable(f func(j *store.Job, dir string)) error {
	cutoff := m.Now().Add(-PruneAge)
	list, err := m.Store.ListTerminal()
	if err != nil {
		return err
	}
	for _, j := range list {
		// A stopped job is never pruned: its records are what a resume continues from.
		if j.Status == store.StatusStopped || j.FinishedAt == nil || j.FinishedAt.After(cutoff) ||
			(j.RepoPath != "" && j.CleanedAt == nil) {
			continue
		}
		dir := m.JobDir(j.ID)
		if fi, err := os.Lstat(dir); err != nil || !fi.IsDir() {
			continue
		}
		f(j, dir)
	}
	return nil
}

// archiveSet lists, relative to a job folder, what its archive moves gzipped (gz) and copies (cp):
// the top-level records, the reports of earlier attempts and their attempts/<n>/ run files (only
// n below the job's current attempt: a higher n the job made itself is not an attempt).
func archiveSet(dir string, attempt int) (gz, cp []string) {
	gz = slices.Clone(archiveGzip)
	cp = slices.Clone(archiveCopy)
	if ents, err := os.ReadDir(dir); err == nil {
		for _, e := range ents {
			if reAttemptReport.MatchString(e.Name()) {
				cp = append(cp, e.Name())
			}
		}
	}
	for _, n := range attemptNumbers(dir, attempt) {
		for _, name := range archiveAttemptGzip {
			gz = append(gz, filepath.Join(attemptsDir, n, name))
		}
		for _, name := range archiveAttemptCopy {
			cp = append(cp, filepath.Join(attemptsDir, n, name))
		}
	}
	return gz, cp
}

// archiveJob copies a job's records into <archive>/<YYYY-MM>/<id>/ (built in a temporary dir and
// renamed into place, so a half-written archive is never taken for a done one), then removes the
// gzipped originals from the job folder. A job archived by an earlier pass is not archived again;
// leftovers of the moved files (a pass interrupted after the rename) are removed.
func (m *Manager) archiveJob(j *store.Job, dir string, dryRun bool) ArchiveJob {
	aj := ArchiveJob{ID: j.ID, Dest: m.archiveJobDir(j), Moved: []PruneEntry{}}
	gz, _ := archiveSet(dir, j.Attempt)
	moved := func(reason string) {
		for _, name := range gz {
			p := filepath.Join(dir, name)
			if fi, err := os.Lstat(p); err == nil && fi.Mode().IsRegular() {
				aj.Moved = append(aj.Moved, PruneEntry{Path: p, Bytes: fi.Size(), Reason: reason})
			}
		}
	}
	if !dryRun {
		if err := writableJobDir(dir); err != nil {
			aj.Error = err.Error()
			return aj
		}
	}
	if _, _, err := m.archiveRoot(!dryRun, !dryRun); err != nil {
		aj.Error = err.Error()
		return aj
	}
	monthDir := filepath.Dir(aj.Dest)
	if _, err := realDir(monthDir, false); err != nil {
		aj.Error = err.Error()
		return aj
	}
	switch done, err := realDir(aj.Dest, false); {
	case err != nil:
		aj.Error = err.Error()
		return aj
	case done:
		// The rename is the completeness signal, but the dir may have been recreated or emptied
		// since (a sync tool, a partial restore): an original goes only when its copy is there.
		if err := archivedCopies(dir, aj.Dest, j.Attempt); err != nil {
			aj.Error = err.Error()
			return aj
		}
		aj.Already = true
		moved("already archived in " + aj.Dest + ", leftover removed")
	case dryRun:
		moved("would move to " + aj.Dest + " (gzip)")
		return aj
	default:
		// The summary row is taken while the stream is still in the job folder (jobs that ended
		// before job_stats existed get theirs here).
		if _, err := m.Store.GetStats(j.ID); errors.Is(err, store.ErrNotFound) {
			m.recordStatsOf(j.ID)
		}
		if err := m.writeArchive(j, dir, aj.Dest); err != nil {
			aj.Error = err.Error()
			return aj
		}
		moved("moved to " + aj.Dest + " (gzip)")
	}
	if dryRun {
		return aj
	}
	for _, e := range aj.Moved {
		if err := os.Remove(e.Path); err != nil && !errors.Is(err, os.ErrNotExist) {
			aj.Error = err.Error()
			return aj
		}
	}
	return aj
}

// writableJobDir makes a job folder the job made read-only 0700 again: its entries cannot be
// removed from a read-only dir.
func writableJobDir(dir string) error {
	fi, err := os.Lstat(dir)
	if err != nil {
		return err
	}
	if fi.Mode().Perm()&0o700 != 0o700 {
		return os.Chmod(dir, 0o700)
	}
	return nil
}

// archivedCopies checks that an existing archive dest is complete for the originals still in the
// job folder: dest/meta.json and dest/<name>.gz for every one of them, as regular files.
func archivedCopies(dir, dest string, attempt int) error {
	need := []string{"meta.json"}
	gz, _ := archiveSet(dir, attempt)
	for _, name := range gz {
		if fi, err := os.Lstat(filepath.Join(dir, name)); err == nil && fi.Mode().IsRegular() {
			need = append(need, name+".gz")
		}
	}
	for _, name := range need {
		fi, err := os.Lstat(filepath.Join(dest, name))
		if err != nil || !fi.Mode().IsRegular() {
			return fmt.Errorf("%s exists but has no %s: originals kept (move the incomplete dir aside to archive again)", dest, name)
		}
	}
	return nil
}

// syncDir fsyncs a directory, so its entries (a rename into it) are durable before the originals
// go. A filesystem that cannot sync a directory (EINVAL, ENOTSUP) is accepted as it is.
func syncDir(p string) error {
	f, err := os.Open(p)
	if err != nil {
		return err
	}
	err = f.Sync()
	if errors.Is(err, syscall.EINVAL) || errors.Is(err, syscall.ENOTSUP) {
		err = nil
	}
	return errors.Join(err, f.Close())
}

// writeArchive builds the archive of one job in a temporary dir next to dest and renames it into
// place. Temporary dirs of this job left by an interrupted pass are removed first; the temporary dir
// and the month dir are fsynced, so the archive is durable before the caller removes the originals.
func (m *Manager) writeArchive(j *store.Job, dir, dest string) error {
	monthDir := filepath.Dir(dest)
	if _, err := realDir(monthDir, true); err != nil {
		return err
	}
	stale, err := filepath.Glob(filepath.Join(monthDir, "."+j.ID+".tmp-*"))
	if err != nil {
		return err
	}
	for _, p := range stale {
		if err := removeTree(p); err != nil {
			return err
		}
	}
	tmp, err := os.MkdirTemp(monthDir, "."+j.ID+".tmp-")
	if err != nil {
		return err
	}
	ok := false
	defer func() {
		if !ok {
			_ = removeTree(tmp)
		}
	}()
	gz, cp := archiveSet(dir, j.Attempt)
	for _, n := range attemptNumbers(dir, j.Attempt) {
		if err := os.MkdirAll(filepath.Join(tmp, attemptsDir, n), 0o700); err != nil {
			return err
		}
	}
	for _, name := range gz {
		if err := archiveFile(filepath.Join(dir, name), filepath.Join(tmp, name+".gz"), true); err != nil {
			return err
		}
	}
	for _, name := range cp {
		if err := archiveFile(filepath.Join(dir, name), filepath.Join(tmp, name), false); err != nil {
			return err
		}
	}
	meta, err := json.MarshalIndent(metaOf(j), "", "  ")
	if err != nil {
		return err
	}
	if err := createSynced(filepath.Join(tmp, "meta.json"), append(meta, '\n')); err != nil {
		return err
	}
	if err := os.Chmod(tmp, 0o700); err != nil {
		return err
	}
	if err := syncDir(tmp); err != nil {
		return err
	}
	if err := os.Rename(tmp, dest); err != nil {
		return err
	}
	ok = true
	// The month dir holds the rename; the root holds a month dir this pass may have created.
	return errors.Join(syncDir(monthDir), syncDir(filepath.Dir(monthDir)))
}

// openRegular opens a regular file without following a link or blocking on a FIFO. ok=false (no
// error) when the file is missing or is not a regular file: such an entry is never archived.
func openRegular(p string) (f *os.File, ok bool, err error) {
	f, err = os.OpenFile(p, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if errors.Is(err, os.ErrNotExist) || errors.Is(err, syscall.ELOOP) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, err
	}
	if fi, err := f.Stat(); err != nil || !fi.Mode().IsRegular() {
		_ = f.Close()
		return nil, false, err
	}
	return f, true, nil
}

// createSynced writes data into the new file p (O_EXCL, never through a link), fsynced before close.
func createSynced(p string, data []byte) error {
	f, err := os.OpenFile(p, os.O_WRONLY|os.O_CREATE|os.O_EXCL|syscall.O_NOFOLLOW, 0o600)
	if err != nil {
		return err
	}
	_, err = f.Write(data)
	return errors.Join(err, syncFile(f), f.Close())
}

// archiveFile copies src (a regular file of the job folder, never followed) to dst, gzipped when
// gz. A missing or non-regular src is skipped.
func archiveFile(src, dst string, gz bool) error {
	in, ok, err := openRegular(src)
	if err != nil || !ok {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_EXCL|syscall.O_NOFOLLOW, 0o600)
	if err != nil {
		return err
	}
	var w io.Writer = out
	var zw *gzip.Writer
	if gz {
		zw = gzip.NewWriter(out)
		w = zw
	}
	_, err = io.Copy(w, in)
	if zw != nil {
		err = errors.Join(err, zw.Close())
	}
	err = errors.Join(err, syncFile(out), out.Close())
	if err != nil {
		return fmt.Errorf("archive %s: %w", src, err)
	}
	return nil
}

// expireArchive removes archived months whose last day is older than the retention (none with 0).
// Only top-level <YYYY-MM> entries are considered; a symlink is removed as a link, never followed.
// A root without the marker (not created by the archive) is never expired; off then says so.
func (m *Manager) expireArchive(dryRun bool) (out []PruneEntry, off string, err error) {
	out = []PruneEntry{}
	days := m.Cfg.ArchiveDays()
	if days == 0 {
		return out, "", nil
	}
	root := m.ArchiveDir()
	switch ok, owned, err := m.archiveRoot(false, !dryRun); {
	case err != nil || !ok:
		return out, "", err
	case !owned:
		return out, "retention off: no marker in " + root, nil
	}
	cutoff := m.Now().Add(-time.Duration(days) * 24 * time.Hour)
	ents, err := os.ReadDir(root)
	if err != nil {
		return out, "", err
	}
	var errs []error
	for _, e := range ents {
		if !reMonth.MatchString(e.Name()) {
			continue
		}
		start, err := time.Parse("2006-01", e.Name())
		if err != nil || !start.AddDate(0, 1, 0).Before(cutoff) {
			continue
		}
		p := filepath.Join(root, e.Name())
		size := treeSize(p)
		if !dryRun {
			if err := removeTree(p); err != nil {
				errs = append(errs, err)
				continue
			}
		}
		out = append(out, PruneEntry{Path: p, Bytes: size, Reason: fmt.Sprintf("archived month older than %d days", days)})
	}
	return out, "", errors.Join(errs...)
}

// jobMeta is meta.json of an archived job: the job row (without the launch token).
type jobMeta struct {
	ID             string     `json:"id"`
	Agent          string     `json:"agent"`
	Title          string     `json:"title"`
	Kind           string     `json:"kind"`
	Status         string     `json:"status"`
	Model          string     `json:"model"`
	Complexity     string     `json:"complexity,omitempty"`
	Runner         string     `json:"runner"`
	Billing        string     `json:"billing"`
	RepoPath       string     `json:"repo_path,omitempty"`
	BaseCommit     string     `json:"base_commit,omitempty"`
	MaxTurns       int        `json:"max_turns"`
	MaxBudgetUSD   float64    `json:"max_budget_usd"`
	TimeoutMinutes int        `json:"timeout_minutes"`
	Turns          int        `json:"turns"`
	CostUSD        float64    `json:"cost_usd"`
	DurationSec    int        `json:"duration_sec"`
	CreatedAt      time.Time  `json:"created_at"`
	StartedAt      *time.Time `json:"started_at,omitempty"`
	FinishedAt     *time.Time `json:"finished_at,omitempty"`
	CleanedAt      *time.Time `json:"cleaned_at,omitempty"`
	ResumedFrom    string     `json:"resumed_from,omitempty"`
	ScheduleID     string     `json:"schedule_id,omitempty"`
	Error          string     `json:"error,omitempty"`
	Attempt        int        `json:"attempt"`
}

func metaOf(j *store.Job) jobMeta {
	return jobMeta{
		ID: j.ID, Agent: j.Agent, Title: j.Title, Kind: j.Kind, Status: j.Status, Model: j.Model,
		Complexity: j.Complexity, Runner: j.Runner, Billing: j.Billing, RepoPath: j.RepoPath, BaseCommit: j.BaseCommit,
		MaxTurns: j.MaxTurns, MaxBudgetUSD: j.MaxBudgetUSD, TimeoutMinutes: j.TimeoutMinutes, Turns: j.Turns,
		CostUSD: j.CostUSD, DurationSec: j.DurationSec, CreatedAt: j.CreatedAt, StartedAt: j.StartedAt,
		FinishedAt: j.FinishedAt, CleanedAt: j.CleanedAt, ResumedFrom: j.ResumedFrom, ScheduleID: j.ScheduleID,
		Error: j.Error, Attempt: j.Attempt,
	}
}
