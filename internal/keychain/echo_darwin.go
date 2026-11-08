//go:build darwin

package keychain

import (
	"io"
	"os"
	"syscall"
	"unsafe"
)

// noEcho turns terminal echo off when r is a terminal, and returns a function that restores it.
func noEcho(r io.Reader) func() {
	f, ok := r.(*os.File)
	if !ok {
		return func() {}
	}
	fd := f.Fd()
	var old syscall.Termios
	if _, _, e := syscall.Syscall(syscall.SYS_IOCTL, fd, syscall.TIOCGETA, uintptr(unsafe.Pointer(&old))); e != 0 {
		return func() {}
	}
	quiet := old
	quiet.Lflag &^= syscall.ECHO
	if _, _, e := syscall.Syscall(syscall.SYS_IOCTL, fd, syscall.TIOCSETA, uintptr(unsafe.Pointer(&quiet))); e != 0 {
		return func() {}
	}
	return func() {
		_, _, _ = syscall.Syscall(syscall.SYS_IOCTL, fd, syscall.TIOCSETA, uintptr(unsafe.Pointer(&old)))
	}
}
