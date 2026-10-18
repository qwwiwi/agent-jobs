package jobs

import (
	"fmt"
	"net"
	"os"

	"golang.org/x/sys/unix"
)

// peerPID returns the pid of the process at the other end of a unix socket connection
// (LOCAL_PEERPID, taken at connect time), refusing a peer of another user (LOCAL_PEERCRED).
func peerPID(conn net.Conn) (int, error) {
	uc, ok := conn.(*net.UnixConn)
	if !ok {
		return 0, fmt.Errorf("peer credentials: not a unix socket connection")
	}
	raw, err := uc.SyscallConn()
	if err != nil {
		return 0, fmt.Errorf("peer credentials: %w", err)
	}
	var pid int
	var xu *unix.Xucred
	var serr error
	if err := raw.Control(func(fd uintptr) {
		if pid, serr = unix.GetsockoptInt(int(fd), unix.SOL_LOCAL, unix.LOCAL_PEERPID); serr == nil {
			xu, serr = unix.GetsockoptXucred(int(fd), unix.SOL_LOCAL, unix.LOCAL_PEERCRED)
		}
	}); err != nil {
		return 0, fmt.Errorf("peer credentials: %w", err)
	}
	if serr != nil {
		return 0, fmt.Errorf("peer credentials: %w", serr)
	}
	if int(xu.Uid) != os.Getuid() {
		return 0, fmt.Errorf("peer credentials: peer uid %d is not this user's", xu.Uid)
	}
	if pid <= 0 {
		return 0, fmt.Errorf("peer credentials: no peer pid")
	}
	return pid, nil
}
