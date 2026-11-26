//go:build !linux

package jobs

import "golang.org/x/sys/unix"

// openDirBeneathOS opens the directory name under dirfd without following a symlink. There is no
// openat2 here: the caller's device comparison is the mount boundary.
func openDirBeneathOS(dirfd int, name string) (int, error) {
	return unix.Openat(dirfd, name, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
}
