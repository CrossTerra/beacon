package actsocket

import (
	"errors"
	"net"
)

var errPeer = errors.New("proposal socket peer is not this user")

// peerPID compares the uid from the connected socket, not a pid or uid supplied in a request.
func peerPID(c *net.UnixConn, wantUID int) (int, error) {
	raw, err := c.SyscallConn()
	if err != nil {
		return 0, err
	}
	var uid, pid int
	var credErr error
	if err := raw.Control(func(fd uintptr) { uid, pid, credErr = credentials(int(fd)) }); err != nil {
		return 0, err
	}
	if credErr != nil || uid != wantUID || pid <= 0 {
		return 0, errPeer
	}
	return pid, nil
}
