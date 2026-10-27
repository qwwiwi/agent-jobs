package jobs

import "golang.org/x/sys/unix"

// renameNoReplace renames from (under fromFd) to to (under toFd), failing if to exists.
func renameNoReplace(fromFd int, from string, toFd int, to string) error {
	return unix.Renameat2(fromFd, from, toFd, to, unix.RENAME_NOREPLACE)
}

// mountID returns the id of the mount the open file fd lies on (statx STATX_MNT_ID, Linux 5.8), or 0
// where the kernel does not report it.
func mountID(fd int) uint64 {
	var stx unix.Statx_t
	if err := unix.Statx(fd, "", unix.AT_EMPTY_PATH, unix.STATX_MNT_ID, &stx); err != nil || stx.Mask&unix.STATX_MNT_ID == 0 {
		return 0
	}
	return stx.Mnt_id
}
