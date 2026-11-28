package localstate

import (
	"errors"
	"os"

	"github.com/CrossTerra/beacon/internal/approval"
	"golang.org/x/sys/unix"
)

var (
	errCollectorSetupBusy = errors.New("another command is setting up the collector for this instance")
	errCollectorSetupLock = errors.New("could not lock the private state directory for collector setup")
)

// LockCollectorSetup serializes legacy init with paired Signal setup in this state directory.
// It is independent of instance.lock: a running legacy approval service does not own it.
// Lock the directory itself so a refused operation does not leave a new file behind.
func LockCollectorSetup(dir string) (*os.File, error) {
	fd, err := unix.Open(dir, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, errCollectorSetupLock
	}
	f := os.NewFile(uintptr(fd), dir)
	if err := unix.Flock(fd, unix.LOCK_EX|unix.LOCK_NB); err != nil {
		f.Close()
		if errors.Is(err, unix.EWOULDBLOCK) {
			return nil, errCollectorSetupBusy
		}
		return nil, errCollectorSetupLock
	}
	info, err := f.Stat()
	current, pathErr := approval.CheckPrivate(dir, os.ModeDir|0o700)
	if err != nil || pathErr != nil || !os.SameFile(info, current) {
		f.Close()
		return nil, errCollectorSetupLock
	}
	return f, nil
}
