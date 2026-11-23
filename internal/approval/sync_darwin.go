package approval

import (
	"os"

	"golang.org/x/sys/unix"
)

func syncJournal(f *os.File) error {
	if err := f.Sync(); err != nil {
		return err
	}
	_, err := unix.FcntlInt(f.Fd(), unix.F_FULLFSYNC, 0)
	return err
}

func syncParent(f *os.File) (string, error) {
	return flushDir(f, func(fd int) error {
		_, err := unix.FcntlInt(uintptr(fd), unix.F_FULLFSYNC, 0)
		return err
	}, unix.Fsync)
}
