package jobs

import (
	"bufio"
	"errors"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"

	"github.com/qwwiwi/agent-jobs/internal/store"
)

// The job's temp dir after the job (#105): what its tools leave there (go-build dirs, the binaries a
// `go test` builds, a judge's ~1 GB of caches) is removed in the tick that marks the job cleaned, once
// the containment has settled and no process uses it any more; the report, the logs and the stream
// stay in the folder until prune. Kept, and retried on later ticks, while anything still holds it.

// deletedSuffix is what the kernel appends to a /proc link whose file was unlinked.
const deletedSuffix = " (deleted)"

// pathVariants returns dir cleaned and, when it differs, with its symlinks resolved: /proc names the
// real path.
func pathVariants(dir string) []string {
	out := []string{filepath.Clean(dir)}
	if r, err := filepath.EvalSymlinks(dir); err == nil && r != out[0] {
		out = append(out, r)
	}
	return out
}

// inDir reports whether p (a /proc link target, " (deleted)" allowed) is one of roots or lies under one.
func inDir(p string, roots []string) bool {
	p = strings.TrimSuffix(p, deletedSuffix)
	if !filepath.IsAbs(p) {
		return false // pipe:[…], socket:[…], anon_inode:…
	}
	p = filepath.Clean(p)
	for _, r := range roots {
		if p == r || strings.HasPrefix(p, r+string(filepath.Separator)) {
			return true
		}
	}
	return false
}

// procLink reads the /proc/<pid>/<name> link ("" when unreadable).
func procLink(pid int, name string) string {
	p, err := os.Readlink(filepath.Join(procRoot, strconv.Itoa(pid), name))
	if err != nil {
		return ""
	}
	return p
}

// tmpBinaryJob returns the id of the job whose temp dir (<root>/<id>/tmp/…, root one of the variants
// of this agent's jobs dir) holds the executable pid runs: its /proc exe link (also once the file is
// deleted) or an absolute argv[0]. "" when none.
func tmpBinaryJob(jobsRoots []string, pid int, argv []string) string {
	cands := []string{strings.TrimSuffix(procLink(pid, "exe"), deletedSuffix)}
	if len(argv) > 0 && filepath.IsAbs(argv[0]) {
		cands = append(cands, argv[0])
	}
	for _, root := range jobsRoots {
		for _, p := range cands {
			if !filepath.IsAbs(p) {
				continue
			}
			rel, err := filepath.Rel(root, filepath.Clean(p))
			if err != nil {
				continue
			}
			parts := strings.Split(rel, string(filepath.Separator))
			if len(parts) >= 3 && parts[0] != ".." && parts[0] != "." && parts[1] == JobTmpDir {
				return parts[0]
			}
		}
	}
	return ""
}

// procServiceUnit returns the systemd service unit pid runs in (the last element of its cgroup v2
// path when it is a .service), or "".
func procServiceUnit(pid int) string {
	cg, err := procCgroup(pid)
	if err != nil {
		return ""
	}
	if b := path.Base(cg); strings.HasSuffix(b, ".service") {
		return Printable(b, 120)
	}
	return ""
}

// tmpHolders returns the live processes that use dir, the temp dir of job jobID: their executable,
// working or root directory, an open file or a mapping lies under it, or their environment carries
// the job's marker or points a temp variable (tempEnvKeys) into it. A process whose /proc entries
// this user cannot read (another user's, or a non-dumpable one) is judged by what is readable. Only
// a failure to list /proc is an error.
func tmpHolders(dir, jobID string) ([]int, error) {
	roots := pathVariants(dir)
	ents, err := os.ReadDir(procRoot)
	if err != nil {
		return nil, err
	}
	var out []int
	for _, e := range ents {
		pid, err := strconv.Atoi(e.Name())
		if err != nil || pid <= 1 {
			continue
		}
		if p, err := procStatFull(pid); err != nil || p.state == "Z" {
			continue
		}
		if procUsesDir(pid, jobID, roots) {
			out = append(out, pid)
		}
	}
	return out, nil
}

// procUsesDir is tmpHolders' test of one process.
func procUsesDir(pid int, jobID string, roots []string) bool {
	for _, l := range []string{"exe", "cwd", "root"} {
		if inDir(procLink(pid, l), roots) {
			return true
		}
	}
	base := filepath.Join(procRoot, strconv.Itoa(pid))
	if fds, err := os.ReadDir(filepath.Join(base, "fd")); err == nil {
		for _, fd := range fds {
			if inDir(procLink(pid, filepath.Join("fd", fd.Name())), roots) {
				return true
			}
		}
	}
	if f, err := os.Open(filepath.Join(base, "maps")); err == nil {
		sc := bufio.NewScanner(f)
		sc.Buffer(make([]byte, 0, 64<<10), 1<<20)
		for sc.Scan() {
			// address perms offset dev inode path: the path starts at the sixth field.
			if fields := strings.SplitN(sc.Text(), " ", 6); len(fields) == 6 && inDir(strings.TrimSpace(fields[5]), roots) {
				f.Close()
				return true
			}
		}
		f.Close()
	}
	if b, err := os.ReadFile(filepath.Join(base, "environ")); err == nil {
		for _, kv := range strings.Split(string(b), "\x00") {
			k, v, _ := strings.Cut(kv, "=")
			if (k == JobEnvVar && v == jobID) || (v != "" && slices.Contains(tempEnvKeys, k) && inDir(v, roots)) {
				return true
			}
		}
	}
	return false
}

// removeJobTmp removes the temp dir of finished job j (tmpCleanupDue). It is kept (an error, so it is
// retried) while the job's process group or acceptance group is alive, its sandbox
// containers are not removed yet (they use it as their home), /proc cannot be listed, or any process
// still uses it (tmpHolders). Without /proc (not Linux) nothing proves it unused: it is kept for prune,
// without an error. A non-directory in its place (a link the job planted) is left alone.
func (m *Manager) removeJobTmp(j *store.Job) error {
	jobDir := m.JobDir(j.ID)
	dir := filepath.Join(jobDir, JobTmpDir)
	fi, err := os.Lstat(dir)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("job temp dir: %w", err)
	}
	if !fi.IsDir() {
		return nil
	}
	if tmpRemovalGOOS != "linux" || !procExists(os.Getpid()) {
		return nil
	}
	if m.spawnedGroupAlive(j) {
		return fmt.Errorf("keeping %s: process group %d of the job is still alive", dir, j.PGID)
	}
	if m.acceptanceGroupAlive(j) {
		return fmt.Errorf("keeping %s: acceptance process group %d of the job is still alive", dir, j.AcceptancePGID)
	}
	if _, err := os.Lstat(filepath.Join(jobDir, containersPendingFile)); !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("keeping %s: the job's sandbox containers are not removed yet", dir)
	}
	holders, err := tmpHolders(dir, j.ID)
	if err != nil {
		return fmt.Errorf("keeping %s: cannot check its users: %w", dir, err)
	}
	if len(holders) > 0 {
		pids := make([]string, 0, len(holders))
		for _, p := range holders {
			pids = append(pids, strconv.Itoa(p))
		}
		return fmt.Errorf("keeping %s: still used by pids %s (see doctor procs)", dir, Printable(strings.Join(pids, ", "), 200))
	}
	if err := removeTree(dir); err != nil {
		return fmt.Errorf("remove %s: %w", dir, err)
	}
	m.Log.Printf("job %s: temp dir removed", j.ID)
	m.recordDirSize(j.ID)
	return nil
}

// tmpCleanupDue removes the temp dir of every cleaned job that still has one (#105), each tick. It is a
// pass of its own, so a kept temp dir never holds up the cleanup mark that prune and the log archive go
// by. A temp dir kept (removeJobTmp failed) is retried with the cleanup's back-off: the first
// cleanupBackoffAfter tries on consecutive ticks, then 2, 4, 8 … ticks apart (at most
// cleanupBackoffMax); the first failure and the one that starts the back-off are logged.
func (m *Manager) tmpCleanupDue() error {
	if tmpRemovalGOOS != "linux" {
		return nil
	}
	dirs, err := filepath.Glob(filepath.Join(m.StateDir, "jobs", "*", JobTmpDir))
	if err != nil {
		return err
	}
	now := m.Now()
	seen := map[string]bool{}
	for _, d := range dirs {
		id := filepath.Base(filepath.Dir(d))
		seen[id] = true
		j, err := m.Store.Get(id)
		if err != nil || j.CleanedAt == nil || !finished(j.Status) || j.ScopePending {
			continue // not this agent's, not cleaned yet, resumed, or its scope not stopped yet
		}
		f := m.tmpFails[id]
		if now.Before(f.next) {
			continue
		}
		if err := m.removeJobTmp(j); err != nil {
			f.n++
			if f.n >= cleanupBackoffAfter {
				wait := cleanupBackoffMax
				if k := f.n - cleanupBackoffAfter + 1; k < 16 {
					wait = min(TickInterval<<k, cleanupBackoffMax)
				}
				f.next = now.Add(wait)
			}
			switch f.n {
			case 1:
				m.Log.Printf("job %s: temp dir not removed (retried every tick): %s", id, RedactText(err.Error()))
			case cleanupBackoffAfter:
				m.Log.Printf("job %s: temp dir not removed: %s; kept %d times in a row, backing off (retries up to every %s, not logged until it is removed)",
					id, RedactText(err.Error()), f.n, cleanupBackoffMax)
			}
			if m.tmpFails == nil {
				m.tmpFails = make(map[string]cleanupFail)
			}
			m.tmpFails[id] = f
			continue
		}
		delete(m.tmpFails, id)
	}
	for id := range m.tmpFails {
		if !seen[id] {
			delete(m.tmpFails, id) // removed by prune, or the folder is gone
		}
	}
	return nil
}

// tmpRemovalGOOS is runtime.GOOS; a test swaps it to see the non-Linux path keep the temp dir.
var tmpRemovalGOOS = runtime.GOOS
