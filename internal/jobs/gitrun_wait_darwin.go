//go:build darwin

package jobs

import (
	"errors"
	"time"

	"golang.org/x/sys/unix"
)

// waitExitNoReap blocks until process pid, a child of the daemon, has exited and leaves it a zombie: its
// PID, the ID of its process group, stays allocated until it is reaped. macOS has no waitid in x/sys, so
// the exit is observed through kqueue (EVFILT_PROC NOTE_EXIT), which never reaps. Registering on a child
// that has already exited fails with ESRCH, which is its exit too; and as a guard against a missed event
// the state is re-read through sysctl (p_stat SZOMB) every second.
func waitExitNoReap(pid int) error {
	kq, err := unix.Kqueue()
	if err != nil {
		return err
	}
	defer unix.Close(kq)
	unix.CloseOnExec(kq)
	change := []unix.Kevent_t{{
		Ident:  uint64(pid),
		Filter: unix.EVFILT_PROC,
		Flags:  unix.EV_ADD | unix.EV_ONESHOT,
		Fflags: unix.NOTE_EXIT,
	}}
	for {
		_, err := unix.Kevent(kq, change, nil, nil)
		if errors.Is(err, unix.ESRCH) {
			return nil
		}
		if err == nil {
			break
		}
		if !errors.Is(err, unix.EINTR) {
			return err
		}
	}
	events := make([]unix.Kevent_t, 1)
	poll := unix.NsecToTimespec(int64(time.Second))
	for {
		n, err := unix.Kevent(kq, nil, events, &poll)
		switch {
		case errors.Is(err, unix.EINTR):
		case err != nil:
			return err
		case n > 0:
			return nil
		}
		if kp, err := unix.SysctlKinfoProc("kern.proc.pid", pid); err == nil && int(kp.Proc.P_pid) == pid && kp.Proc.P_stat == sZomb {
			return nil
		}
	}
}
