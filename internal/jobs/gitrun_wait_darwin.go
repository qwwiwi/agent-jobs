//go:build darwin

package jobs

import (
	"errors"
	"fmt"
	"time"

	"golang.org/x/sys/unix"
)

// waitExitNoReap blocks until process pid, a child of the daemon, has exited and leaves it a zombie: its
// PID, the ID of its process group, stays allocated until it is reaped. macOS has no waitid in x/sys, so
// the exit is observed through kqueue (EVFILT_PROC NOTE_EXIT), which never reaps. Registering on a child
// that has already exited fails with ESRCH, which is its exit too; and as a guard against a missed event
// the state is re-read through sysctl (p_stat SZOMB) every second. When kqueue fails, the sysctl poll
// alone waits: an error here would leave git unkillable (gitProc.reap).
func waitExitNoReap(pid int) error {
	if err := waitExitKqueue(pid); err == nil || !errors.Is(err, errNoKqueue) {
		return err
	}
	// No kqueue (descriptors exhausted): the sysctl poll alone, so that git stays killable meanwhile.
	for !isZombie(pid) {
		time.Sleep(100 * time.Millisecond)
	}
	return nil
}

// errNoKqueue marks a waitExitKqueue that could not watch the process at all.
var errNoKqueue = errors.New("no kqueue")

// waitExitKqueue is waitExitNoReap through kqueue, errNoKqueue when the watch cannot be set up.
func waitExitKqueue(pid int) error {
	kq, err := unix.Kqueue()
	if err != nil {
		return fmt.Errorf("%w: %w", errNoKqueue, err)
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
			return fmt.Errorf("%w: %w", errNoKqueue, err)
		}
	}
	events := make([]unix.Kevent_t, 1)
	poll := unix.NsecToTimespec(int64(time.Second))
	for {
		n, err := unix.Kevent(kq, nil, events, &poll)
		switch {
		case errors.Is(err, unix.EINTR):
		case err != nil:
			return fmt.Errorf("%w: %w", errNoKqueue, err)
		case n > 0:
			return nil
		}
		if isZombie(pid) {
			return nil
		}
	}
}

// isZombie reports whether sysctl shows process pid as a zombie (p_stat SZOMB).
func isZombie(pid int) bool {
	kp, err := unix.SysctlKinfoProc("kern.proc.pid", pid)
	return err == nil && int(kp.Proc.P_pid) == pid && kp.Proc.P_stat == sZomb
}
