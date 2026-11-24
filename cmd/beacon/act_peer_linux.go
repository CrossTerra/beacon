//go:build linux

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
		cred, err := unix.GetsockoptUcred(int(fd), unix.SOL_SOCKET, unix.SO_PEERCRED)
		if err != nil {
			opErr = err
			return
		}
		pid = int(cred.Pid)
	}); err != nil {
		return 0, err
	}
	return pid, opErr
}
