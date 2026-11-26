package jobs

import "golang.org/x/sys/unix"

// openDirBeneathOS opens the directory name under dirfd. The kernel refuses, in the same lookup that
// returns the descriptor, a mount point (EXDEV, bind mounts included), a symlink and any step out of
// dirfd. openat2 needs Linux 5.6; without it (ENOSYS, or EPERM from a seccomp profile) the error is
// returned as it is: the caller fails closed.
func openDirBeneathOS(dirfd int, name string) (int, error) {
	return unix.Openat2(dirfd, name, &unix.OpenHow{
		Flags:   unix.O_RDONLY | unix.O_DIRECTORY | unix.O_NOFOLLOW | unix.O_CLOEXEC,
		Resolve: unix.RESOLVE_NO_XDEV | unix.RESOLVE_BENEATH | unix.RESOLVE_NO_SYMLINKS | unix.RESOLVE_NO_MAGICLINKS,
	})
}
