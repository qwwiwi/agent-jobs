package trash

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"

	"golang.org/x/sys/unix"
)

// foldCase: Linux filesystems are case-sensitive.
const foldCase = false

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

func renameNoReplace(fromFd int, from string, toFd int, to string) error {
	err := unix.Renameat2(fromFd, from, toFd, to, unix.RENAME_NOREPLACE)
	if errors.Is(err, unix.EXDEV) {
		return errors.New("refused: the path is on another filesystem than the trash (a move would be a copy)")
	}
	return err
}
