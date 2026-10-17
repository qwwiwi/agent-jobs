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
	"syscall"
	"time"

	"github.com/qwwiwi/agent-jobs/internal/store"
)

// Log archive: when prune cleans a job folder, the job's records for later analysis move into
// <archive>/<YYYY-MM>/<id>/ (stream.jsonl and stderr.log gzipped, prompt.md, report.md, diffstat.txt
// copied, meta.json = the job row). The archive lies outside the job folders, so cleaning jobs never
// touches it; prune deletes archived months older than log_archive_days.
var (
	// archiveGzip are moved: gzipped into the archive, then removed from the job folder.
	archiveGzip = []string{"stream.jsonl", "stderr.log"}
	// archiveCopy are copied: the job folder keeps them (report.md and diffstat.txt are prune records).
	archiveCopy = []string{"prompt.md", "report.md", "diffstat.txt"}
	reMonth     = regexp.MustCompile(`^[0-9]{4}-[0-9]{2}$`)
)

// archiveMarker is written into an archive root the archive created. Retention and the 0700 chmod
// act only on a root that holds it: an existing folder the owner pointed log_archive_dir at (a
// synced folder with its own month dirs) is archived into but never expired or chmodded.
const archiveMarker = ".agent-jobs-archive"

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
}

// ArchiveDir returns the configured archive root (default <state>/archive).
func (m *Manager) ArchiveDir() string {
	if m.Cfg.LogArchiveDir != "" {
		return m.Cfg.LogArchiveDir
	}
	return filepath.Join(m.StateDir, "archive")
}

// archiveMonth is the archive month of a job: the UTC month it finished (created when unfinished).
func archiveMonth(j *store.Job) string {
	t := j.CreatedAt
	if j.FinishedAt != nil {
		t = *j.FinishedAt
	}
	return t.UTC().Format("2006-01")
}

// archiveJobDir returns <archive>/<YYYY-MM>/<id>.
func (m *Manager) archiveJobDir(j *store.Job) string {
	return filepath.Join(m.ArchiveDir(), archiveMonth(j), j.ID)
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

// archiveRoot checks the archive root (a real directory, created on demand with the marker) and
// keeps a root of ours private: the logs carry tool output, so it is 0700. owned reports the marker;
// an existing root without it is used as it is (never chmodded).
func (m *Manager) archiveRoot(create bool) (ok, owned bool, err error) {
	root := m.ArchiveDir()
	if _, err := os.Lstat(root); errors.Is(err, os.ErrNotExist) && create {
		if err := os.MkdirAll(root, 0o700); err != nil {
			return false, false, err
		}
		f, err := os.OpenFile(filepath.Join(root, archiveMarker), os.O_WRONLY|os.O_CREATE|os.O_EXCL|syscall.O_NOFOLLOW, 0o600)
		if err != nil {
			return false, false, err
		}
		_, err = f.WriteString("Log archive of agent-jobs: prune expires the <YYYY-MM> dirs here.\n")
		if err := errors.Join(err, f.Close()); err != nil {
			return false, false, err
		}
	}
	if ok, err = realDir(root, false); err != nil || !ok {
		return ok, false, err
	}
	fi, err := os.Lstat(filepath.Join(root, archiveMarker))
	owned = err == nil && fi.Mode().IsRegular()
	if create && owned {
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
	if rep.Expired, err = m.expireArchive(dryRun); err != nil {
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
		if j.FinishedAt == nil || j.FinishedAt.After(cutoff) || (j.RepoPath != "" && j.CleanedAt == nil) {
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

// archiveJob copies a job's records into <archive>/<YYYY-MM>/<id>/ (built in a temporary dir and
// renamed into place, so a half-written archive is never taken for a done one), then removes the
// gzipped originals from the job folder. A job archived by an earlier pass is not archived again;
// leftovers of the moved files (a pass interrupted after the rename) are removed.
func (m *Manager) archiveJob(j *store.Job, dir string, dryRun bool) ArchiveJob {
	aj := ArchiveJob{ID: j.ID, Dest: m.archiveJobDir(j), Moved: []PruneEntry{}}
	moved := func(reason string) {
		for _, name := range archiveGzip {
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
	if _, _, err := m.archiveRoot(!dryRun); err != nil {
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
		if err := archivedCopies(dir, aj.Dest); err != nil {
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
func archivedCopies(dir, dest string) error {
	need := []string{"meta.json"}
	for _, name := range archiveGzip {
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
	for _, name := range archiveGzip {
		if err := archiveFile(filepath.Join(dir, name), filepath.Join(tmp, name+".gz"), true); err != nil {
			return err
		}
	}
	for _, name := range archiveCopy {
		if err := archiveFile(filepath.Join(dir, name), filepath.Join(tmp, name), false); err != nil {
			return err
		}
	}
	meta, err := json.MarshalIndent(metaOf(j), "", "  ")
	if err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(tmp, "meta.json"), append(meta, '\n'), 0o600); err != nil {
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
	err = errors.Join(err, out.Sync(), out.Close())
	if err != nil {
		return fmt.Errorf("archive %s: %w", src, err)
	}
	return nil
}

// expireArchive removes archived months whose last day is older than the retention (none with 0).
// Only top-level <YYYY-MM> entries are considered; a symlink is removed as a link, never followed.
// A root without the marker (not created by the archive) is never expired.
func (m *Manager) expireArchive(dryRun bool) ([]PruneEntry, error) {
	out := []PruneEntry{}
	days := m.Cfg.ArchiveDays()
	if days == 0 {
		return out, nil
	}
	root := m.ArchiveDir()
	if ok, owned, err := m.archiveRoot(false); err != nil || !ok || !owned {
		return out, err
	}
	cutoff := m.Now().Add(-time.Duration(days) * 24 * time.Hour)
	ents, err := os.ReadDir(root)
	if err != nil {
		return out, err
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
	return out, errors.Join(errs...)
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
}

func metaOf(j *store.Job) jobMeta {
	return jobMeta{
		ID: j.ID, Agent: j.Agent, Title: j.Title, Kind: j.Kind, Status: j.Status, Model: j.Model,
		Complexity: j.Complexity, Runner: j.Runner, Billing: j.Billing, RepoPath: j.RepoPath, BaseCommit: j.BaseCommit,
		MaxTurns: j.MaxTurns, MaxBudgetUSD: j.MaxBudgetUSD, TimeoutMinutes: j.TimeoutMinutes, Turns: j.Turns,
		CostUSD: j.CostUSD, DurationSec: j.DurationSec, CreatedAt: j.CreatedAt, StartedAt: j.StartedAt,
		FinishedAt: j.FinishedAt, CleanedAt: j.CleanedAt, ResumedFrom: j.ResumedFrom, ScheduleID: j.ScheduleID,
		Error: j.Error,
	}
}
