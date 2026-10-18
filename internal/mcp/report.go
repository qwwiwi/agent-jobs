package mcp

import (
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"syscall"

	"golang.org/x/sys/unix"
)

// errNotRegular marks a job file that is not a regular file of this user with one link (FIFO, device,
// socket, directory, symlink, hard link, another owner), or a job dir reached through a symlink.
var errNotRegular = errors.New("not a regular file")

// getuid is os.Getuid; a test swaps it to see a file of another owner refused.
var getuid = os.Getuid

// ReadJobReport reads at most max bytes of <stateDir>/jobs/<id>/report.md; truncated reports whether
// there was more. The job dir is writable by the job, so the walk never leaves the state dir: jobs
// and <id> are opened relative to the descriptor before them with O_NOFOLLOW|O_DIRECTORY (a symlinked
// directory is refused, not followed), report.md with O_NOFOLLOW|O_NONBLOCK (a FIFO would otherwise
// hang the open), and the opened file must be a regular file owned by this user with exactly one
// link (a hard link to an outside file is refused). Only then is it read.
func ReadJobReport(stateDir, id string, max int) (string, bool, error) {
	if id == "" || id == "." || id == ".." || strings.ContainsAny(id, "/\x00") {
		return "", false, fmt.Errorf("job id %q: %w (not a job dir name)", id, errNotRegular)
	}
	sfd, err := unix.Open(stateDir, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC, 0)
	if err != nil {
		return "", false, &os.PathError{Op: "open", Path: stateDir, Err: err}
	}
	defer unix.Close(sfd)
	jfd, err := openDirAt(sfd, "jobs")
	if err != nil {
		return "", false, err
	}
	defer unix.Close(jfd)
	dfd, err := openDirAt(jfd, id)
	if err != nil {
		return "", false, err
	}
	defer unix.Close(dfd)
	rfd, err := unix.Openat(dfd, "report.md", unix.O_RDONLY|unix.O_NOFOLLOW|unix.O_NONBLOCK|unix.O_CLOEXEC, 0)
	if err != nil {
		if errors.Is(err, syscall.ELOOP) {
			return "", false, fmt.Errorf("report.md: %w (symlink)", errNotRegular)
		}
		return "", false, &os.PathError{Op: "open", Path: "report.md", Err: err}
	}
	f := os.NewFile(uintptr(rfd), "report.md")
	defer f.Close()
	var st unix.Stat_t
	if err := unix.Fstat(rfd, &st); err != nil {
		return "", false, &os.PathError{Op: "fstat", Path: "report.md", Err: err}
	}
	switch {
	case st.Mode&unix.S_IFMT != unix.S_IFREG:
		return "", false, fmt.Errorf("report.md: %w (mode %o)", errNotRegular, st.Mode&unix.S_IFMT)
	case uint64(st.Nlink) != 1:
		return "", false, fmt.Errorf("report.md: %w (%d links)", errNotRegular, st.Nlink)
	case int64(st.Uid) != int64(getuid()):
		return "", false, fmt.Errorf("report.md: %w (owner uid %d)", errNotRegular, st.Uid)
	}
	b, err := io.ReadAll(io.LimitReader(f, int64(max)+1))
	if err != nil {
		return "", false, err
	}
	if len(b) > max {
		return string(b[:max]), true, nil
	}
	return string(b), false, nil
}

// openDirAt opens the directory name under dirfd without following a symlink.
func openDirAt(dirfd int, name string) (int, error) {
	fd, err := unix.Openat(dirfd, name, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	switch {
	case errors.Is(err, syscall.ELOOP), errors.Is(err, syscall.ENOTDIR):
		return -1, fmt.Errorf("%s: %w (symlink or not a directory)", name, errNotRegular)
	case err != nil:
		return -1, &os.PathError{Op: "open", Path: name, Err: err}
	}
	return fd, nil
}
