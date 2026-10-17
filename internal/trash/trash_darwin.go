package trash

import (
	"errors"
	"fmt"

	"golang.org/x/sys/unix"
)

// foldCase: APFS and HFS+ are case-insensitive by default.
const foldCase = true

// openDir opens dir without following a link and proves (fstat device and inode against an lstat
// of dir) that the descriptor is that directory, so a last component swapped for a symlink after
// the check is caught. macOS has no /proc/self/fd; the lstat follows the same intermediate
// components as the open, so unlike on Linux a swapped intermediate component is not detected here
// (check resolves the parent just before).
func openDir(dir string) (int, error) {
	fd, err := unix.Open(dir, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		return -1, fmt.Errorf("open %s: %w", dir, err)
	}
	var got, want unix.Stat_t
	errF, errL := unix.Fstat(fd, &got), unix.Lstat(dir, &want)
	if errF != nil || errL != nil || got.Dev != want.Dev || got.Ino != want.Ino {
		_ = unix.Close(fd)
		return -1, fmt.Errorf("refused: %s changed while it was being opened (%v, %v)", dir, errF, errL)
	}
	return fd, nil
}

func renameNoReplace(fromFd int, from string, toFd int, to string) error {
	err := unix.RenameatxNp(fromFd, from, toFd, to, unix.RENAME_EXCL)
	if errors.Is(err, unix.EXDEV) {
		return errors.New("refused: the path is on another filesystem than the trash (a move would be a copy)")
	}
	return err
}
