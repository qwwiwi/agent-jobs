package jobs

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"syscall"

	"golang.org/x/sys/unix"
)

// The kick (v0.8.2 G12): a process that has just made work for the daemon (Enqueue queued a job, a
// wrapper stored its completion and so freed a slot) sends SIGUSR1 to the daemon named by daemon.json,
// and the daemon ticks now instead of at the next minute tick. A kick is only ever a hint: when it
// cannot be sent (no daemon.json, a dead pid, a pid that is not a daemon, an unreadable argv) nothing is
// signalled and the minute tick picks the work up as before.

// KickSignal is the signal that makes the daemon tick now.
const KickSignal = syscall.SIGUSR1

// IsDaemonCmdline reports an argv of the daemon: `<bin> daemon …`. The binary is matched by structure,
// not by name (it may be installed under any name), as in IsWrapperCmdline.
func IsDaemonCmdline(argv []string) bool {
	return len(argv) > 1 && argv[1] == "daemon"
}

// processArgv reads the argv of a live process: /proc on Linux, sysctl on macOS. A zombie has none.
func processArgv(pid int) ([]string, error) {
	if runtime.GOOS == "linux" {
		return procCmdline(pid)
	}
	info, err := sysctlProc(pid)
	if err != nil {
		return nil, err
	}
	if info.zombie {
		return nil, fmt.Errorf("pid %d is a zombie", pid)
	}
	return info.argv, nil
}

// KickDaemon sends KickSignal to the daemon of stateDir (the pid in its daemon.json). It reports whether
// the signal was sent. Only a process whose argv is the daemon's is signalled: SIGUSR1 ends a process
// that does not handle it, and a stale daemon.json may name a pid that now belongs to anything. On Linux
// the pid is pinned by a pidfd before its argv is read, so the check and the signal hit one process.
// A dead pid, a process that is not a daemon, or an argv that cannot be read is no kick and no error.
func KickDaemon(stateDir string) (bool, error) {
	d, err := readDaemonInfo(stateDir)
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if d.PID <= 1 {
		return false, nil
	}
	fd, err := pidfdOpen(d.PID)
	switch {
	case errors.Is(err, unix.ESRCH):
		return false, nil
	case errors.Is(err, unix.ENOSYS) || errors.Is(err, errPidfdUnsupported):
		fd = -1 // no pidfds (macOS, an old kernel): check, then signal by pid
	case err != nil:
		return false, fmt.Errorf("kick the daemon: pidfd_open %d: %w", d.PID, err)
	default:
		defer unix.Close(fd)
	}
	if argv, err := processArgv(d.PID); err != nil || !IsDaemonCmdline(argv) {
		return false, nil
	}
	if fd >= 0 {
		err = pidfdSend(fd, KickSignal)
	} else {
		err = syscall.Kill(d.PID, KickSignal)
	}
	if errors.Is(err, unix.ESRCH) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("kick the daemon (pid %d): %w", d.PID, err)
	}
	return true, nil
}

// kickDaemon kicks this manager's daemon; a failure is logged, never returned (the minute tick remains).
func (m *Manager) kickDaemon() {
	if _, err := KickDaemon(m.StateDir); err != nil {
		m.Log.Printf("%v (the next minute tick picks the work up)", err)
	}
}

// kickDaemonOf kicks the daemon of the job in jobDir (<state>/jobs/<id>); a failure goes to log.
func kickDaemonOf(jobDir string, log io.Writer) {
	abs, err := filepath.Abs(jobDir)
	if err == nil {
		_, err = KickDaemon(filepath.Dir(filepath.Dir(abs)))
	}
	if err != nil {
		fmt.Fprintln(log, "run-wrapper:", err, "(the daemon's next minute tick collects the job)")
	}
}
