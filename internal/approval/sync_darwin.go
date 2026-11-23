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
	fd := int(f.Fd())
	// os.File.Sync is F_FULLFSYNC on macOS, so it cannot be this fallback.
	if _, err := unix.FcntlInt(uintptr(fd), unix.F_FULLFSYNC, 0); err != nil {
		if ferr := unix.Fsync(fd); ferr != nil { // syscall.Fsync
			return "", ferr
		}
		return flushSync, nil
	}
	return flushFull, nil
}
