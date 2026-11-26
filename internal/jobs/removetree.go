package jobs

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"golang.org/x/sys/unix"
)

// ErrMountBoundary is returned (wrapped, with the path) when a removal meets a mount point or a
// directory on another filesystem: it is not entered and not removed.
var ErrMountBoundary = errors.New("mount boundary")

// errRemovalBudget ends a removal that ran out of entries or time: what is left goes on the next call.
var errRemovalBudget = errors.New("removal budget used up, the rest is removed on the next pass")

// Seams and bounds of the bounded remover (tests replace them).
var (
	// openDirBeneath is the only way a removal or a size walk enters a directory.
	openDirBeneath = openDirBeneathOS
	// dirDevice is the device of an open directory (the second, portable mount check).
	dirDevice = func(fd int) (uint64, error) {
		var st unix.Stat_t
		if err := unix.Fstat(fd, &st); err != nil {
			return 0, err
		}
		return uint64(st.Dev), nil //nolint:unconvert // int32 on darwin
	}
	removeMaxEntries = 2_000_000
	removeBudget     = 60 * time.Second
)

// maxRemoveErrors bounds the errors one removal reports (a tree of unremovable entries would
// otherwise produce an error text as large as the tree).
const maxRemoveErrors = 8

// MountBoundaryError names the mount point a removal left in place; errors.Is(err, ErrMountBoundary).
type MountBoundaryError struct{ Path string }

func (e *MountBoundaryError) Error() string {
	return "mount point or other filesystem at " + e.Path + ": not entered, not removed"
}

// Is makes the error match ErrMountBoundary.
func (e *MountBoundaryError) Is(target error) bool { return target == ErrMountBoundary }

// remover is one bounded removal: every directory is entered through openDirBeneath relative to its
// parent's descriptor and must be on the device the removal started on.
type remover struct {
	rootDev  uint64
	left     int
	deadline time.Time
	bytes    int64 // st_size of everything unlinked
	errs     []error
	dropped  int // errors past maxRemoveErrors
	stopped  bool
}

// removeTree removes p and everything under it. It never follows a symlink, never steps out of p and
// never enters or removes a mount point: a directory is entered only through openDirBeneath
// (openat2 with RESOLVE_NO_XDEV|RESOLVE_BENEATH on Linux) and only when it is on the same device as
// p's parent. A mount point and its ancestors stay, their siblings go, and the error wraps
// ErrMountBoundary. Any other failure to open a directory safely (openat2 missing included) fails
// closed. Read-only directories (a job's leftovers, the job dir's 0500 no-hooks/) are made writable
// through their own descriptor. The walk is bounded (removeMaxEntries, removeBudget, maxChmodDepth
// levels); past the bound it returns errRemovalBudget and a later call continues.
func removeTree(p string) error {
	_, err := removeTreeSized(p)
	return err
}

// removeTreeSized is removeTree that also says how many bytes it freed (also on an error: what was
// removed before it).
func removeTreeSized(p string) (int64, error) {
	p = filepath.Clean(p)
	parent, err := unix.Open(filepath.Dir(p), unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		if errors.Is(err, unix.ENOENT) {
			return 0, nil
		}
		return 0, &os.PathError{Op: "open", Path: filepath.Dir(p), Err: err}
	}
	defer unix.Close(parent)
	dev, err := dirDevice(parent)
	if err != nil {
		return 0, &os.PathError{Op: "fstat", Path: filepath.Dir(p), Err: err}
	}
	r := &remover{rootDev: dev, left: removeMaxEntries, deadline: time.Now().Add(removeBudget)}
	ok := r.removeAt(parent, filepath.Base(p), p, 0)
	if r.dropped > 0 {
		r.errs = append(r.errs, fmt.Errorf("and %d more errors", r.dropped))
	}
	if !ok && len(r.errs) == 0 {
		r.errs = append(r.errs, fmt.Errorf("remove %s: not removed", p)) // unreachable: every failure records one
	}
	return r.bytes, errors.Join(r.errs...)
}

// fail records an error of the removal; false for the caller to return.
func (r *remover) fail(err error) bool {
	if len(r.errs) < maxRemoveErrors || errors.Is(err, ErrMountBoundary) || errors.Is(err, errRemovalBudget) {
		r.errs = append(r.errs, err)
	} else {
		r.dropped++
	}
	return false
}

// removeAt removes the entry name of the open directory dfd (path is its full name, for errors) and
// says whether it is gone. Siblings of a failed entry are still removed; a directory with anything
// left below it stays.
func (r *remover) removeAt(dfd int, name, path string, depth int) bool {
	if r.stopped {
		return false
	}
	if r.left--; r.left < 0 || time.Now().After(r.deadline) {
		r.stopped = true
		return r.fail(errRemovalBudget)
	}
	var st unix.Stat_t
	if err := unix.Fstatat(dfd, name, &st, unix.AT_SYMLINK_NOFOLLOW); err != nil {
		if errors.Is(err, unix.ENOENT) {
			return true
		}
		return r.fail(&os.PathError{Op: "lstat", Path: path, Err: err})
	}
	if st.Mode&unix.S_IFMT != unix.S_IFDIR {
		return r.unlink(dfd, name, path, 0, st.Size) // a symlink goes as a link
	}
	if depth > maxChmodDepth {
		return r.fail(fmt.Errorf("remove %s: deeper than %d levels, not removed", path, maxChmodDepth))
	}
	fd, err := openDirBeneath(dfd, name)
	switch {
	case errors.Is(err, unix.EXDEV):
		return r.fail(&MountBoundaryError{Path: path})
	case errors.Is(err, unix.EACCES):
		// An unreadable directory cannot be entered, but an empty one needs no descriptor: rmdir takes
		// it (and refuses one with content, and a mount point).
		if unix.Unlinkat(dfd, name, unix.AT_REMOVEDIR) == nil {
			r.bytes += st.Size
			return true
		}
		return r.fail(&os.PathError{Op: "open", Path: path, Err: err})
	case err != nil:
		return r.fail(&os.PathError{Op: "open", Path: path, Err: err})
	}
	f := os.NewFile(uintptr(fd), path)
	defer f.Close()
	if dev, err := dirDevice(fd); err != nil {
		return r.fail(&os.PathError{Op: "fstat", Path: path, Err: err})
	} else if dev != r.rootDev {
		return r.fail(&MountBoundaryError{Path: path})
	}
	var own unix.Stat_t
	if err := unix.Fstat(fd, &own); err != nil {
		return r.fail(&os.PathError{Op: "fstat", Path: path, Err: err})
	}
	if own.Mode&0o700 != 0o700 {
		if err := unix.Fchmod(fd, 0o700); err != nil {
			return r.fail(&os.PathError{Op: "chmod", Path: path, Err: err})
		}
	}
	names, err := f.Readdirnames(-1)
	if err != nil {
		return r.fail(&os.PathError{Op: "readdir", Path: path, Err: err})
	}
	empty := true
	for _, n := range names {
		if !r.removeAt(fd, n, filepath.Join(path, n), depth+1) {
			empty = false
		}
		if r.stopped {
			return false
		}
	}
	if !empty {
		return false
	}
	return r.unlink(dfd, name, path, unix.AT_REMOVEDIR, own.Size)
}

// unlink removes one entry (a mount point answers EBUSY and stays).
func (r *remover) unlink(dfd int, name, path string, flags int, size int64) bool {
	if err := unix.Unlinkat(dfd, name, flags); err != nil && !errors.Is(err, unix.ENOENT) {
		return r.fail(&os.PathError{Op: "remove", Path: path, Err: err})
	}
	r.bytes += size
	return true
}
