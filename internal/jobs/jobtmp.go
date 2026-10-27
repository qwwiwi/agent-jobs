package jobs

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"time"

	"golang.org/x/sys/unix"

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

// tmpStagePrefix starts the name a temp dir is renamed to just before it is removed
// (<job>/.tmp-removing-<unix>): from then on no process finds it by the job's path. One that an
// interrupted removal left, or that could not be renamed back, is removed by a later tick.
const tmpStagePrefix = ".tmp-removing-"

// pfKthread is the kernel-thread bit of the flags in /proc/<pid>/stat.
const pfKthread = 0x00200000

// maxTmpEntries bounds the entries a temp dir check walks; a bigger tree is kept for prune.
const maxTmpEntries = 1 << 20

// procReader is how the temp dir check reads /proc; a test swaps tmpProc to make a process unreadable.
type procReader struct {
	readlink func(string) (string, error)
	readFile func(string) ([]byte, error)
	readDir  func(string) ([]os.DirEntry, error)
	// stat follows a /proc link to the file it names: its device and inode, whatever its path.
	stat func(string) (os.FileInfo, error)
}

var tmpProc = procReader{readlink: os.Readlink, readFile: os.ReadFile, readDir: os.ReadDir, stat: os.Stat}

// tmpRemovalHook is called at each stage of a removal: "scanned" (/proc checked, nothing renamed yet)
// and "renamed" (the dir is under its staging name, not checked again yet), with the dir's current
// path; a test makes a process take the dir there.
var tmpRemovalHook = func(stage, dir string) {}

// devIno identifies a file by device and inode.
type devIno struct{ dev, ino uint64 }

// tmpTree is what a process must not use for a temp dir to be removed: its paths (the job's tmp and,
// once renamed, the staging name, each also with symlinks resolved, as /proc names it), the job's
// marker, and the device and inode of every entry of the tree (a process in another mount namespace,
// a sandbox, names it by another path; a working dir that was renamed is still the same inode).
// private: the tree's top is this user's and closed to group and others (0700), and so is every
// entry this user's. before: processes that started before it (the job's creation, less attrSlack)
// predate the job (zero: none is taken as such). selfFd: the descriptor the removal holds on the
// tree, not a use.
type tmpTree struct {
	roots   []string
	jobID   string
	inodes  map[devIno]bool
	private bool
	uid     int
	before  time.Time
	selfFd  int
	fs      fsID // the file system of the tree's top: the walk does not cross into another
}

// newTmpTree walks the directory fd for tmpTree.
func newTmpTree(fd int, roots []string, jobID string, before time.Time) (*tmpTree, error) {
	t := &tmpTree{roots: roots, jobID: jobID, inodes: map[devIno]bool{}, uid: os.Geteuid(), before: before, selfFd: fd}
	if err := t.walk(fd, ".", 0); err != nil {
		return nil, err
	}
	return t, nil
}

// walk adds directory name (under dirfd) and everything in it to t. Each directory is opened relative
// to its parent with O_NOFOLLOW, so nothing outside the tree is visited; one that cannot be listed is
// an error (the tree is not fully known).
func (t *tmpTree) walk(dirfd int, name string, depth int) error {
	if depth > maxChmodDepth {
		return fmt.Errorf("deeper than %d levels", maxChmodDepth)
	}
	fd, err := openDirNoFollow(dirfd, name)
	if err != nil {
		return err
	}
	f := os.NewFile(uintptr(fd), name)
	defer f.Close()
	var st unix.Stat_t
	if err := unix.Fstat(fd, &st); err != nil {
		return err
	}
	id, err := fsOf(fd)
	if err != nil {
		return err
	}
	switch {
	case depth == 0:
		t.private = int64(st.Uid) == int64(t.uid) && st.Mode&0o077 == 0
		t.fs = id
	case id != t.fs:
		return fmt.Errorf("%s is a mount point (another file system in the tree)", name)
	}
	t.add(&st)
	names, err := f.Readdirnames(-1)
	if err != nil {
		return fmt.Errorf("list %s: %w", name, err)
	}
	for _, n := range names {
		if len(t.inodes) >= maxTmpEntries {
			return fmt.Errorf("more than %d entries", maxTmpEntries)
		}
		if err := unix.Fstatat(fd, n, &st, unix.AT_SYMLINK_NOFOLLOW); err != nil {
			if errors.Is(err, unix.ENOENT) {
				continue
			}
			return fmt.Errorf("stat %s: %w", n, err)
		}
		if st.Mode&unix.S_IFMT == unix.S_IFDIR {
			if err := t.walk(fd, n, depth+1); err != nil {
				return err
			}
			continue
		}
		t.add(&st)
	}
	return nil
}

func (t *tmpTree) add(st *unix.Stat_t) {
	t.inodes[devIno{uint64(st.Dev), uint64(st.Ino)}] = true
	if int64(st.Uid) != int64(t.uid) {
		t.private = false
	}
}

// usesLink reports whether the /proc link p names a file in t, by its path or (followed) by device
// and inode. A link to no path (pipe:[…], socket:[…], anon_inode:…) uses nothing.
func (t *tmpTree) usesLink(p string) (bool, error) {
	target, err := tmpProc.readlink(p)
	if err != nil {
		return false, err
	}
	if inDir(target, t.roots) {
		return true, nil
	}
	if !filepath.IsAbs(target) {
		return false, nil
	}
	fi, err := tmpProc.stat(p)
	if err != nil {
		return false, err
	}
	st, ok := fi.Sys().(*syscall.Stat_t)
	if !ok {
		return false, fmt.Errorf("%s: no device and inode", p)
	}
	return t.inodes[devIno{uint64(st.Dev), uint64(st.Ino)}], nil
}

// usesMaps reports whether a mapping in /proc/<pid>/maps content b is of a file in t, by its path
// or by device and inode.
func (t *tmpTree) usesMaps(b []byte) bool {
	for _, line := range strings.Split(string(b), "\n") {
		// address perms offset dev inode path: the path starts at the sixth field.
		f := strings.SplitN(line, " ", 6)
		if len(f) < 5 {
			continue
		}
		if len(f) == 6 && inDir(strings.TrimSpace(f[5]), t.roots) {
			return true
		}
		ino, err := strconv.ParseUint(f[4], 10, 64)
		major, minor, ok := strings.Cut(f[3], ":")
		if err != nil || ino == 0 || !ok {
			continue
		}
		ma, err1 := strconv.ParseUint(major, 16, 32)
		mi, err2 := strconv.ParseUint(minor, 16, 32)
		if err1 == nil && err2 == nil && t.inodes[devIno{unix.Mkdev(uint32(ma), uint32(mi)), ino}] {
			return true
		}
	}
	return false
}

// usesEnv reports whether the environment b (/proc/<pid>/environ) carries the job's marker or points
// a temp variable (tempEnvKeys) into t.
func (t *tmpTree) usesEnv(b []byte) bool {
	for _, kv := range strings.Split(string(b), "\x00") {
		k, v, _ := strings.Cut(kv, "=")
		if (k == JobEnvVar && v == t.jobID) || (v != "" && slices.Contains(tempEnvKeys, k) && inDir(v, t.roots)) {
			return true
		}
	}
	return false
}

// tmpUse is what the temp dir check found of one process.
type tmpUse int

const (
	useNone   tmpUse = iota
	useHolds         // it uses the tree
	useUnsure        // part of its /proc entry could not be read and the rule does not rule it out
)

// tmpUsers returns the live processes that use tree t (holders) and those that may (unsure). A
// process uses it when its executable, working or root directory, an open file or a mapping is in
// it (by path or by device and inode), or its environment carries the job's marker or points a temp
// variable into it. It fails closed: a process of which any of this could not be read (another
// user's, a non-dumpable one) is unsure, except
//   - one of another uid (from status) while the tree is private: a 0700 dir whose entries are all
//     this user's is one that process never could enter. A process that a 0700 dir does not stop
//     (root, or one with CAP_DAC_OVERRIDE) and that this user cannot read is therefore taken as not
//     using it: a job runs as this user and cannot start one;
//   - one that started before the job was created (t.before): it did not inherit the dir from the
//     job, and nothing of the job started it. This is what lets the dir go at all: this user's
//     `systemd --user` and its `(sd-pam)` are non-dumpable, so never readable, and always there.
//
// Only a failure to list /proc (or to read the boot time) is an error.
func tmpUsers(t *tmpTree) (holders, unsure []int, err error) {
	ents, err := tmpProc.readDir(procRoot)
	if err != nil {
		return nil, nil, err
	}
	boot, err := bootTime()
	if err != nil {
		return nil, nil, fmt.Errorf("boot time: %w", err)
	}
	self := os.Getpid()
	for _, e := range ents {
		pid, err := strconv.Atoi(e.Name())
		if err != nil || pid <= 1 {
			continue
		}
		switch procTmpUse(pid, pid == self, boot, t) {
		case useHolds:
			holders = append(holders, pid)
		case useUnsure:
			unsure = append(unsure, pid)
		}
	}
	return holders, unsure, nil
}

// procTmpUse is tmpUsers' test of one process (self: this process, whose descriptor t.selfFd on the
// tree is not a use).
func procTmpUse(pid int, self bool, boot time.Time, t *tmpTree) tmpUse {
	base := filepath.Join(procRoot, strconv.Itoa(pid))
	state, start := procTmpState(base)
	switch state {
	case procGone:
		return useNone
	case procUnknown:
		return useUnsure
	}
	doubt := false
	for _, l := range []string{"exe", "cwd", "root"} {
		holds, err := t.usesLink(filepath.Join(base, l))
		if holds {
			return useHolds
		}
		doubt = doubt || err != nil
	}
	// A thread can have a working or root dir of its own (unshare(CLONE_FS)).
	if tasks, err := tmpProc.readDir(filepath.Join(base, "task")); err != nil {
		doubt = true
	} else {
		for _, task := range tasks {
			if task.Name() == strconv.Itoa(pid) {
				continue
			}
			for _, l := range []string{"cwd", "root"} {
				holds, err := t.usesLink(filepath.Join(base, "task", task.Name(), l))
				if holds {
					return useHolds
				}
				doubt = doubt || (err != nil && !errors.Is(err, fs.ErrNotExist)) // the thread ended
			}
		}
	}
	if fds, err := tmpProc.readDir(filepath.Join(base, "fd")); err != nil {
		doubt = true
	} else {
		for _, fd := range fds {
			if self && fd.Name() == strconv.Itoa(t.selfFd) {
				continue
			}
			holds, err := t.usesLink(filepath.Join(base, "fd", fd.Name()))
			if holds {
				return useHolds
			}
			doubt = doubt || (err != nil && !errors.Is(err, fs.ErrNotExist)) // closed since the listing
		}
	}
	if b, err := tmpProc.readFile(filepath.Join(base, "maps")); err != nil {
		doubt = true
	} else if t.usesMaps(b) {
		return useHolds
	}
	if b, err := tmpProc.readFile(filepath.Join(base, "environ")); err != nil {
		doubt = true
	} else if t.usesEnv(b) {
		return useHolds
	}
	if !doubt {
		return useNone
	}
	switch now, _ := procTmpState(base); {
	case now == procGone:
		return useNone // it ended while it was read
	case t.private && procOtherUID(base, t.uid):
		return useNone
	case !t.before.IsZero() && boot.Add(time.Duration(start)*time.Second/userHZ).Before(t.before):
		return useNone
	}
	return useUnsure
}

// procState is what /proc/<pid>/stat says of a process for the temp dir check.
type procState int

const (
	procLive    procState = iota
	procGone              // ended, a zombie or a kernel thread: uses nothing
	procUnknown           // stat unreadable or unparsable
)

// procTmpState reads the state of the process at base and its start (clock ticks since boot).
func procTmpState(base string) (procState, uint64) {
	b, err := tmpProc.readFile(filepath.Join(base, "stat"))
	if errors.Is(err, fs.ErrNotExist) || errors.Is(err, syscall.ESRCH) {
		return procGone, 0
	}
	if err != nil {
		return procUnknown, 0
	}
	s := string(b)
	i := strings.LastIndexByte(s, ')')
	if i < 0 {
		return procUnknown, 0
	}
	f := strings.Fields(s[i+1:]) // state ppid pgrp session tty tpgid flags … starttime (index 19)
	if len(f) < 20 {
		return procUnknown, 0
	}
	flags, err := strconv.ParseUint(f[6], 10, 64)
	start, err2 := strconv.ParseUint(f[19], 10, 64)
	switch {
	case err != nil || err2 != nil:
		return procUnknown, 0
	case flags&pfKthread != 0:
		return procGone, 0
	case f[0] == "Z":
		// A leader that exited while other threads run: its fds, working dir and mappings are theirs
		// now and not listed under it.
		if tasks, err := tmpProc.readDir(filepath.Join(base, "task")); err == nil && len(tasks) <= 1 {
			return procGone, 0
		}
		return procUnknown, 0
	}
	return procLive, start
}

// procOtherUID reports whether /proc/<pid>/status names uids (real, effective, saved, file system)
// none of which is uid; false when unreadable (the uid is unknown).
func procOtherUID(base string, uid int) bool {
	b, err := tmpProc.readFile(filepath.Join(base, "status"))
	if err != nil {
		return false
	}
	for _, line := range strings.Split(string(b), "\n") {
		rest, ok := strings.CutPrefix(line, "Uid:")
		if !ok {
			continue
		}
		ids := strings.Fields(rest)
		if len(ids) == 0 {
			return false
		}
		for _, s := range ids {
			if n, err := strconv.Atoi(s); err != nil || n == uid {
				return false
			}
		}
		return true
	}
	return false
}

// checkTmpUnused returns an error naming the processes that use or may use the directory fd (tmpUsers;
// shown is its path in the message), nil when none does.
func checkTmpUnused(fd int, roots []string, jobID string, before time.Time, shown string) error {
	t, err := newTmpTree(fd, roots, jobID, before)
	if err != nil {
		return fmt.Errorf("keeping %s: cannot walk it: %w", shown, err)
	}
	holders, unsure, err := tmpUsers(t)
	if err != nil {
		return fmt.Errorf("keeping %s: cannot check its users: %w", shown, err)
	}
	var why []string
	if len(holders) > 0 {
		why = append(why, "still used by pids "+pidList(holders)+" (see doctor procs)")
	}
	if len(unsure) > 0 {
		why = append(why, "cannot tell whether pids "+pidList(unsure)+" use it (their /proc entries are unreadable)")
	}
	if len(why) > 0 {
		return fmt.Errorf("keeping %s: %s", shown, strings.Join(why, "; "))
	}
	return nil
}

func pidList(pids []int) string {
	s := make([]string, 0, len(pids))
	for _, p := range pids {
		s = append(s, strconv.Itoa(p))
	}
	return Printable(strings.Join(s, ", "), 200)
}

// removeJobTmp removes the temp dir of finished job j (tmpCleanupDue), and a staging dir an earlier
// try left. It is kept (an error, so it is retried) while the job's process group or acceptance group
// is alive, its sandbox containers are not removed yet (they use it as their home), /proc cannot be
// listed, or any process uses it or may use it (tmpUsers). Without /proc (not Linux) nothing proves
// it unused: it is kept for prune, without an error. A non-directory in its place (a link the job
// planted) is left alone.
//
// Every step goes through descriptors: the jobs root and the job dir are opened without following a
// symlink (a symlinked job dir is refused); the temp dir is checked, renamed relative to the job dir
// to its staging name, checked again under both names and only then removed relative to the job dir.
// A process found the second time gets it back under its own name.
func (m *Manager) removeJobTmp(j *store.Job) error {
	if tmpRemovalGOOS != "linux" || !procExists(os.Getpid()) {
		return nil
	}
	jobDir := m.JobDir(j.ID)
	dir := filepath.Join(jobDir, JobTmpDir)
	jobFd, err := openJobDirPinned(m.StateDir, j.ID)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("keeping %s: job dir: %w", dir, err)
	}
	defer unix.Close(jobFd)
	names, err := tmpEntries(jobFd)
	if err != nil {
		return fmt.Errorf("keeping %s: list the job dir: %w", dir, err)
	}
	if len(names) == 0 {
		return nil
	}
	if m.spawnedGroupAlive(j) {
		return fmt.Errorf("keeping %s: process group %d of the job is still alive", dir, j.PGID)
	}
	if m.acceptanceGroupAlive(j) {
		return fmt.Errorf("keeping %s: acceptance process group %d of the job is still alive", dir, j.AcceptancePGID)
	}
	var st unix.Stat_t
	if err := unix.Fstatat(jobFd, containersPendingFile, &st, unix.AT_SYMLINK_NOFOLLOW); !errors.Is(err, unix.ENOENT) {
		return fmt.Errorf("keeping %s: the job's sandbox containers are not removed yet", dir)
	}
	var errs []error
	removed := false
	for _, n := range names {
		ok, err := removeTmpEntry(jobFd, jobDir, n, j.ID, j.CreatedAt.Add(-attrSlack))
		if err != nil {
			errs = append(errs, err)
		}
		removed = removed || ok
	}
	if removed {
		m.Log.Printf("job %s: temp dir removed", j.ID)
		m.recordDirSize(j.ID)
	}
	return errors.Join(errs...)
}

// openJobDirPinned opens <state>/jobs/<id> as a directory without following a symlink at the jobs
// root or at the job dir: a symlinked job dir (or jobs root) is refused.
func openJobDirPinned(stateDir, id string) (int, error) {
	if id == "" || id == "." || id == ".." || strings.ContainsRune(id, filepath.Separator) {
		return -1, fmt.Errorf("job id %q is not a single path element", id)
	}
	sfd, err := unix.Open(stateDir, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC, 0)
	if err != nil {
		return -1, &os.PathError{Op: "open", Path: stateDir, Err: err}
	}
	defer unix.Close(sfd)
	rootFd, err := openDirNoFollow(sfd, "jobs")
	if err != nil {
		return -1, err
	}
	defer unix.Close(rootFd)
	return openDirNoFollow(rootFd, id)
}

// tmpEntries lists the temp dir and staging dirs (real directories only) in the job dir jobFd.
func tmpEntries(jobFd int) ([]string, error) {
	fd, err := openDirNoFollow(jobFd, ".")
	if err != nil {
		return nil, err
	}
	f := os.NewFile(uintptr(fd), "job dir")
	defer f.Close()
	all, err := f.Readdirnames(-1)
	if err != nil {
		return nil, err
	}
	var out []string
	for _, n := range all {
		var st unix.Stat_t
		if (n == JobTmpDir || strings.HasPrefix(n, tmpStagePrefix)) &&
			unix.Fstatat(jobFd, n, &st, unix.AT_SYMLINK_NOFOLLOW) == nil && st.Mode&unix.S_IFMT == unix.S_IFDIR {
			out = append(out, n)
		}
	}
	slices.Sort(out)
	return out, nil
}

// removeTmpEntry removes the directory name (the temp dir or a staging dir) of the job dir jobFd
// (path jobDir) once no process uses it: checked, renamed (the temp dir), checked again, removed.
// Processes started before (the job's creation) predate it (tmpUsers).
func removeTmpEntry(jobFd int, jobDir, name, jobID string, before time.Time) (bool, error) {
	shown := filepath.Join(jobDir, name)
	fd, err := openDirNoFollow(jobFd, name)
	if errors.Is(err, fs.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("keeping %s: %w", shown, err)
	}
	defer unix.Close(fd)
	jobPath := jobDir
	if a, err := filepath.Abs(jobDir); err == nil {
		jobPath = a
	}
	roots := func(names ...string) []string {
		var out []string
		for _, v := range pathVariants(jobPath) {
			for _, n := range names {
				out = append(out, filepath.Join(v, n))
			}
		}
		return out
	}
	if err := checkTmpUnused(fd, roots(JobTmpDir, name), jobID, before, shown); err != nil {
		return false, err
	}
	tmpRemovalHook("scanned", shown)
	staged := name
	if name == JobTmpDir {
		staged = tmpStagePrefix + strconv.FormatInt(time.Now().Unix(), 10)
		if err := renameNoReplace(jobFd, name, jobFd, staged); err != nil {
			return false, fmt.Errorf("keeping %s: rename to %s: %w", shown, staged, err)
		}
		if !sameDirAt(jobFd, staged, fd) { // swapped since it was opened: not the dir checked
			_ = renameNoReplace(jobFd, staged, jobFd, name)
			return false, fmt.Errorf("keeping %s: it changed while it was checked", shown)
		}
		tmpRemovalHook("renamed", filepath.Join(jobDir, staged))
		if err := checkTmpUnused(fd, roots(JobTmpDir, staged), jobID, before, shown); err != nil {
			if rerr := renameNoReplace(jobFd, staged, jobFd, name); rerr != nil {
				err = fmt.Errorf("%w; left as %s, removed by a later tick once unused (%v)", err, staged, rerr)
			}
			return false, err
		}
	}
	// The contents go through the descriptor of the dir that was checked; its name only for the last
	// rmdir, which fails on anything put there in its place but an empty dir.
	top, err := fsOf(fd)
	if err != nil {
		return false, fmt.Errorf("keeping %s: %w", shown, err)
	}
	if err := removeContents(fd, top, 0); err != nil {
		return false, fmt.Errorf("remove %s: %w", filepath.Join(jobDir, staged), err)
	}
	if !sameDirAt(jobFd, staged, fd) {
		return false, fmt.Errorf("keeping %s: it changed while it was removed", shown)
	}
	if err := unix.Unlinkat(jobFd, staged, unix.AT_REMOVEDIR); err != nil {
		return false, fmt.Errorf("remove %s: %w", filepath.Join(jobDir, staged), err)
	}
	return true, nil
}

// fsID identifies the file system (device) and, where the kernel reports it, the mount of a file:
// a bind mount of a dir of the same device is another mount.
type fsID struct{ dev, mnt uint64 }

// fsOf returns the fsID of the open file fd.
func fsOf(fd int) (fsID, error) {
	var st unix.Stat_t
	if err := unix.Fstat(fd, &st); err != nil {
		return fsID{}, err
	}
	return fsID{dev: uint64(st.Dev), mnt: mountID(fd)}, nil
}

// sameDirAt reports whether name under dirfd (not followed) is the directory open as fd.
func sameDirAt(dirfd int, name string, fd int) bool {
	var a, b unix.Stat_t
	return unix.Fstatat(dirfd, name, &a, unix.AT_SYMLINK_NOFOLLOW) == nil && unix.Fstat(fd, &b) == nil &&
		a.Mode&unix.S_IFMT == unix.S_IFDIR && a.Dev == b.Dev && a.Ino == b.Ino
}

// removeContents removes everything in the directory dirfd. Each directory is opened relative to its
// parent without following a symlink (a symlink inside is removed as a link) and made 0700 first, so
// a read-only one (a go module cache) goes too; one on another file system than top (a mount point,
// a FUSE mount the job left) is not entered and fails the removal.
func removeContents(dirfd int, top fsID, depth int) error {
	if depth > maxChmodDepth {
		return fmt.Errorf("deeper than %d levels", maxChmodDepth)
	}
	_ = unix.Fchmod(dirfd, 0o700)
	fd, err := openDirNoFollow(dirfd, ".")
	if err != nil {
		return err
	}
	f := os.NewFile(uintptr(fd), "dir")
	names, err := f.Readdirnames(-1)
	f.Close()
	if err != nil {
		return err
	}
	for _, n := range names {
		err := unix.Unlinkat(dirfd, n, 0)
		if err != nil && !errors.Is(err, unix.ENOENT) {
			var st unix.Stat_t
			if unix.Fstatat(dirfd, n, &st, unix.AT_SYMLINK_NOFOLLOW) == nil && st.Mode&unix.S_IFMT == unix.S_IFDIR {
				err = removeDirAt(dirfd, n, top, depth+1)
			}
		}
		if err != nil && !errors.Is(err, unix.ENOENT) {
			return fmt.Errorf("%s: %w", n, err)
		}
	}
	return nil
}

// removeDirAt removes the directory name under dirfd (not followed) with everything in it.
func removeDirAt(dirfd int, name string, top fsID, depth int) error {
	fd, err := openDirNoFollow(dirfd, name)
	if err != nil {
		return err
	}
	if id, err := fsOf(fd); err != nil || id != top {
		unix.Close(fd)
		return fmt.Errorf("%s is a mount point (another file system in the tree): not entered", name)
	}
	err = removeContents(fd, top, depth)
	unix.Close(fd)
	if err != nil {
		return err
	}
	return unix.Unlinkat(dirfd, name, unix.AT_REMOVEDIR)
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
	var dirs []string
	for _, pat := range []string{JobTmpDir, tmpStagePrefix + "*"} {
		d, err := filepath.Glob(filepath.Join(m.StateDir, "jobs", "*", pat))
		if err != nil {
			return err
		}
		dirs = append(dirs, d...)
	}
	now := m.Now()
	seen := map[string]bool{}
	for _, d := range dirs {
		id := filepath.Base(filepath.Dir(d))
		if seen[id] {
			continue
		}
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
