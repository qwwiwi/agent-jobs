//go:build darwin

package jobs

import (
	"errors"
	"syscall"
	"testing"

	"golang.org/x/sys/unix"
)

// processGone reports whether process pid has ended: it is gone, or a zombie that waits for its parent.
// macOS has no /proc: the state is read through sysctl kern.proc.pid (p_stat SZOMB), and a pid it does
// not know (ESRCH, or the short read x/sys reports as EIO) is gone.
func processGone(t *testing.T, pid int) bool {
	t.Helper()
	kp, err := unix.SysctlKinfoProc("kern.proc.pid", pid)
	if errors.Is(err, syscall.ESRCH) || errors.Is(err, syscall.EIO) {
		return true
	}
	if err != nil {
		t.Fatal(err)
	}
	if int(kp.Proc.P_pid) != pid {
		return true
	}
	return kp.Proc.P_stat == sZomb
}
