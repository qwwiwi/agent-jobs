package jobs

import (
	"fmt"
	"net"
	"os"

	"golang.org/x/sys/unix"
)

// peerPID returns the pid of the process at the other end of a unix socket connection (SO_PEERCRED,
// taken by the kernel at connect time), refusing a peer of another user.
func peerPID(conn net.Conn) (int, error) {
	uc, ok := conn.(*net.UnixConn)
	if !ok {
		return 0, fmt.Errorf("peer credentials: not a unix socket connection")
	}
	raw, err := uc.SyscallConn()
	if err != nil {
		return 0, fmt.Errorf("peer credentials: %w", err)
	}
	var cred *unix.Ucred
	var serr error
	if err := raw.Control(func(fd uintptr) {
		cred, serr = unix.GetsockoptUcred(int(fd), unix.SOL_SOCKET, unix.SO_PEERCRED)
	}); err != nil {
		return 0, fmt.Errorf("peer credentials: %w", err)
	}
	if serr != nil {
		return 0, fmt.Errorf("peer credentials: %w", serr)
	}
	if int(cred.Uid) != os.Getuid() {
		return 0, fmt.Errorf("peer credentials: peer uid %d is not this user's", cred.Uid)
	}
	if cred.Pid <= 0 {
		return 0, fmt.Errorf("peer credentials: no peer pid")
	}
	return int(cred.Pid), nil
}
