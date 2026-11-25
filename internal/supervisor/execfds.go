package supervisor

import (
	"errors"
	"os"
	"strconv"

	"golang.org/x/sys/unix"
)

// Only the trusted bootstrap is running here, with no concurrent file opens
// except those made close-on-exec by the Go runtime. Do not cap the FD number.
func markFDDirectory(path string) error {
	dir, err := os.Open(path)
	if err != nil {
		return err
	}
	defer dir.Close()
	// Darwin's /dev/fd entries can have no directory-entry type. Read just
	// the numbers: asking for metadata on an fd slot can return EBADF.
	names, err := dir.Readdirnames(-1)
	if err != nil {
		return err
	}
	for _, name := range names {
		fd, err := strconv.Atoi(name)
		if err != nil || fd < 3 {
			continue
		}
		// The kernel can list an empty slot.
		if _, err := unix.FcntlInt(uintptr(fd), unix.F_SETFD, unix.FD_CLOEXEC); err != nil && !errors.Is(err, unix.EBADF) {
			return err
		}
	}
	return nil
}
