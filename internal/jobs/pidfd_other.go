//go:build !linux

package jobs

import "syscall"

// pidfdOpen and pidfdSend: no pidfds outside Linux, so nothing is ever signalled (the sweep reports
// the members as survivors, doctor procs reports and kills nothing).
var (
	pidfdOpen = func(int) (int, error) { return -1, errPidfdUnsupported }
	pidfdSend = func(int, syscall.Signal) error { return errPidfdUnsupported }
)
