package approval

import (
	"errors"
	"os"
	"path/filepath"
	"sync"
	"syscall"

	"golang.org/x/sys/unix"
)

var (
	ErrPrivate = errors.New("approval state must be owner-only and not a symlink")
	ErrLocked  = errors.New("another process holds the approval instance lock")
)

// CheckPrivate checks the path itself, not its symlink target. System ancestors can include aliases
// such as macOS /var; the state directory and files managed in it may not be aliases.
func CheckPrivate(path string, mode os.FileMode) (os.FileInfo, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return nil, err
	}
	st := info.Sys().(*syscall.Stat_t)
	if int(st.Uid) != os.Getuid() || info.Mode() != mode || (mode.IsRegular() && st.Nlink != 1) {
		return nil, ErrPrivate
	}
	return info, nil
}

func PrivateDir(path string) error {
	if err := os.Mkdir(path, 0o700); err != nil && !errors.Is(err, os.ErrExist) {
		return err
	}
	_, err := CheckPrivate(path, os.ModeDir|0o700)
	return err
}

// OpenPrivate opens only a regular 0600 file owned by this user. O_NONBLOCK avoids blocking on a FIFO
// before fstat; O_NOFOLLOW protects the final path component.
func OpenPrivate(path string, flags int) (*os.File, error) {
	f, err := os.OpenFile(path, flags|unix.O_NOFOLLOW|unix.O_NONBLOCK, 0o600)
	if err != nil {
		return nil, err
	}
	var st unix.Stat_t
	if err := unix.Fstat(int(f.Fd()), &st); err != nil {
		f.Close()
		return nil, err
	}
	if int(st.Uid) != os.Getuid() || st.Mode != unix.S_IFREG|0o600 || st.Nlink != 1 {
		f.Close()
		return nil, ErrPrivate
	}
	return f, nil
}

// heldLocks records instance locks this process already owns. A second flock in this process fails, so
// in-process registration must notice the journal's lock instead of taking another.
var heldLocks sync.Map

// LockInstance holds the same lock future offline setup and migration commands must take. Do not remove the
// lock file: doing so would allow different processes to lock different inodes at the same path.
func LockInstance(dir string) (*os.File, error) {
	if _, err := CheckPrivate(dir, os.ModeDir|0o700); err != nil {
		return nil, err
	}
	f, err := OpenPrivate(filepath.Join(dir, "instance.lock"), os.O_RDWR|os.O_CREATE)
	if err != nil {
		return nil, err
	}
	if err := unix.Flock(int(f.Fd()), unix.LOCK_EX|unix.LOCK_NB); err != nil {
		f.Close()
		if errors.Is(err, unix.EWOULDBLOCK) {
			return nil, ErrLocked
		}
		return nil, err
	}
	heldLocks.Store(filepath.Clean(dir), f)
	return f, nil
}

// HoldsInstance reports whether this process already owns the instance lock.
func HoldsInstance(dir string) bool {
	_, ok := heldLocks.Load(filepath.Clean(dir))
	return ok
}

// CloseInstance releases this process's record of the lock and the lock itself.
func CloseInstance(dir string, f *os.File) error {
	if f == nil {
		return nil
	}
	heldLocks.Delete(filepath.Clean(dir))
	return f.Close()
}

// DropHeld forgets this process's record of the lock without unlocking.
// A test uses it to simulate another process holding instance.lock.
func DropHeld(dir string) { heldLocks.Delete(filepath.Clean(dir)) }
