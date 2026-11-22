// Package peer checks the process at the other end of a local Unix socket before any byte is sent to it
// (plan 13.00-D5). The kernel's audit token for the connection identifies that exact process; its code must be
// valid and satisfy the code requirement configured for Beacon.app. A path or user id alone is not enough.
package peer

import (
	"errors"
	"net"
)

// ErrRefused means the peer is not the configured app: another user, invalid code, or a different signature.
var ErrRefused = errors.New("peer: the socket's listener is not the configured Beacon app")

// Check reports whether the peer of conn runs as this user and satisfies requirement.
func Check(conn *net.UnixConn, requirement string) error {
	if requirement == "" {
		return errors.New("peer: no code requirement is configured")
	}
	raw, err := conn.SyscallConn()
	if err != nil {
		return err
	}
	var checkErr error
	if err := raw.Control(func(fd uintptr) { checkErr = check(int(fd), requirement) }); err != nil {
		return err
	}
	return checkErr
}
