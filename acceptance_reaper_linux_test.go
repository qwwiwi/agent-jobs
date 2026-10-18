//go:build linux

package agentjobs_test

import "syscall"

const prSetChildSubreaper = 36

// setChildSubreaper makes this process a child subreaper: orphans of its descendants become its children.
func setChildSubreaper() error {
	if _, _, e := syscall.RawSyscall(syscall.SYS_PRCTL, prSetChildSubreaper, 1, 0); e != 0 {
		return e
	}
	return nil
}
