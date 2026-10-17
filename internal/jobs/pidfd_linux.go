package jobs

import (
	"syscall"

	"golang.org/x/sys/unix"
)

// pidfdOpen and pidfdSend are the kernel calls; variables so tests can stand in for them.
var (
	pidfdOpen = func(pid int) (int, error) { return unix.PidfdOpen(pid, 0) }
	pidfdSend = func(fd int, sig syscall.Signal) error { return unix.PidfdSendSignal(fd, sig, nil, 0) }
)
