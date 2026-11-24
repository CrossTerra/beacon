//go:build darwin

package main

import (
	"net"

	"golang.org/x/sys/unix"
)

func listenerPID(conn *net.UnixConn) (int, error) {
	raw, err := conn.SyscallConn()
	if err != nil {
		return 0, err
	}
	var pid int
	var opErr error
	if err := raw.Control(func(fd uintptr) {
		pid, opErr = unix.GetsockoptInt(int(fd), unix.SOL_LOCAL, unix.LOCAL_PEERPID)
	}); err != nil {
		return 0, err
	}
	return pid, opErr
}
