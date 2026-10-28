package jobs

import (
	"errors"

	"golang.org/x/sys/unix"
)

// waitExitNoReap blocks until process pid, a child of the daemon, has exited and leaves it a zombie
// (waitid WNOWAIT): its PID, the ID of its process group, stays allocated until it is reaped.
func waitExitNoReap(pid int) error {
	for {
		var info unix.Siginfo
		err := unix.Waitid(unix.P_PID, pid, &info, unix.WEXITED|unix.WNOWAIT, nil)
		if !errors.Is(err, unix.EINTR) {
			return err
		}
	}
}
