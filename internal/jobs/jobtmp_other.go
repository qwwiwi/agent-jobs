//go:build !linux

package jobs

import "errors"

// renameNoReplace: the temp dir is removed only on Linux (removeJobTmp), so this is never reached.
func renameNoReplace(int, string, int, string) error {
	return errors.New("rename without replace: Linux only")
}

// mountID: no mount ids outside Linux (the device alone tells the file systems apart).
func mountID(int) uint64 { return 0 }
