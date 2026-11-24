//go:build !darwin && !linux

package main

import (
	"errors"
	"net"
)

func listenerPID(*net.UnixConn) (int, error) {
	return 0, errors.New("the proposal socket's listener pid is only available on macOS and Linux")
}
