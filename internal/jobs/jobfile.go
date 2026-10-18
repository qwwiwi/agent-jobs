package jobs

import (
	"errors"
	"fmt"
	"os"
	"strings"
	"syscall"

	"golang.org/x/sys/unix"
)

// ErrNotRegular marks a job file that is not a regular file of this user with one link (FIFO, device,
// socket, directory, symlink, hard link, another owner), or a job dir reached through a symlink.
var ErrNotRegular = errors.New("not a regular file")

// OpenJobFile opens <stateDir>/jobs/<id>/<name> for reading and returns it with its size. The job dir
// is writable by the job, so the walk never leaves the state dir: jobs and <id> are opened relative to
// the descriptor before them with O_NOFOLLOW|O_DIRECTORY (a symlinked directory is refused, not
// followed), name with O_NOFOLLOW|O_NONBLOCK (a FIFO would otherwise hang the open), and the opened
// file must be a regular file owned by uid with exactly one link (a hard link to an outside file is
// refused). Refusals wrap ErrNotRegular.
func OpenJobFile(stateDir, id, name string, uid int) (*os.File, int64, error) {
	for _, n := range []string{id, name} {
		if n == "" || n == "." || n == ".." || strings.ContainsAny(n, "/\x00") {
			return nil, 0, fmt.Errorf("%q: %w (not a file name)", n, ErrNotRegular)
		}
	}
	sfd, err := unix.Open(stateDir, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, 0, &os.PathError{Op: "open", Path: stateDir, Err: err}
	}
	defer unix.Close(sfd)
	jfd, err := openDirNoFollow(sfd, "jobs")
	if err != nil {
		return nil, 0, err
	}
	defer unix.Close(jfd)
	dfd, err := openDirNoFollow(jfd, id)
	if err != nil {
		return nil, 0, err
	}
	defer unix.Close(dfd)
	rfd, err := unix.Openat(dfd, name, unix.O_RDONLY|unix.O_NOFOLLOW|unix.O_NONBLOCK|unix.O_CLOEXEC, 0)
	if err != nil {
		if errors.Is(err, syscall.ELOOP) {
			return nil, 0, fmt.Errorf("%s: %w (symlink)", name, ErrNotRegular)
		}
		return nil, 0, &os.PathError{Op: "open", Path: name, Err: err}
	}
	f := os.NewFile(uintptr(rfd), name)
	var st unix.Stat_t
	if err := unix.Fstat(rfd, &st); err != nil {
		_ = f.Close()
		return nil, 0, &os.PathError{Op: "fstat", Path: name, Err: err}
	}
	switch {
	case st.Mode&unix.S_IFMT != unix.S_IFREG:
		err = fmt.Errorf("%s: %w (mode %o)", name, ErrNotRegular, st.Mode&unix.S_IFMT)
	case uint64(st.Nlink) != 1:
		err = fmt.Errorf("%s: %w (%d links)", name, ErrNotRegular, st.Nlink)
	case int64(st.Uid) != int64(uid):
		err = fmt.Errorf("%s: %w (owner uid %d)", name, ErrNotRegular, st.Uid)
	}
	if err != nil {
		_ = f.Close()
		return nil, 0, err
	}
	return f, st.Size, nil
}

// openDirNoFollow opens the directory name under dirfd without following a symlink.
func openDirNoFollow(dirfd int, name string) (int, error) {
	fd, err := unix.Openat(dirfd, name, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	switch {
	case errors.Is(err, syscall.ELOOP), errors.Is(err, syscall.ENOTDIR):
		return -1, fmt.Errorf("%s: %w (symlink or not a directory)", name, ErrNotRegular)
	case err != nil:
		return -1, &os.PathError{Op: "open", Path: name, Err: err}
	}
	return fd, nil
}
