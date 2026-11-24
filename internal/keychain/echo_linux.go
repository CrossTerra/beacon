package keychain

import (
	"io"
	"os"

	"golang.org/x/sys/unix"
)

func noEcho(r io.Reader) func() {
	f, ok := r.(*os.File)
	if !ok {
		return func() {}
	}
	fd := int(f.Fd())
	old, err := unix.IoctlGetTermios(fd, unix.TCGETS)
	if err != nil {
		return func() {}
	}
	quiet := *old
	quiet.Lflag &^= unix.ECHO
	if err := unix.IoctlSetTermios(fd, unix.TCSETS, &quiet); err != nil {
		return func() {}
	}
	return func() { _ = unix.IoctlSetTermios(fd, unix.TCSETS, old) }
}
