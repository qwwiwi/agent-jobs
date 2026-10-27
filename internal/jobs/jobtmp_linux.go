package jobs

import "golang.org/x/sys/unix"

// renameNoReplace renames from (under fromFd) to to (under toFd), failing if to exists.
func renameNoReplace(fromFd int, from string, toFd int, to string) error {
	return unix.Renameat2(fromFd, from, toFd, to, unix.RENAME_NOREPLACE)
}
