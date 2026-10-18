//go:build !linux && !darwin

package jobs

import (
	"errors"
	"net"
)

// peerPID: no peer credentials here, so the proxy refuses every call (fail closed).
func peerPID(net.Conn) (int, error) {
	return 0, errors.New("peer credentials are not supported on this system")
}
