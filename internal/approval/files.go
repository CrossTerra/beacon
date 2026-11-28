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

// WritePrivate replaces an owner-only data file by rename and flushes the containing directory.
// Callers serialize writers. It doesn't accept an existing symlink, special file or wider permissions.
func WritePrivate(path string, body []byte) error {
	if _, err := CheckPrivate(path, 0o600); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	dir := filepath.Dir(path)
	f, err := os.CreateTemp(dir, ".write-")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	if _, err := f.Write(body); err != nil {
		f.Close()
		return err
	}
	if err := errors.Join(syncJournal(f), f.Close()); err != nil {
		return err
	}
	if err := os.Rename(f.Name(), path); err != nil {
		return err
	}
	return FlushDirectory(dir)
}

func FlushDirectory(dir string) error {
	parent, err := os.Open(dir)
	if err != nil {
		return err
	}
	defer parent.Close()
	_, err = syncParent(parent)
	return err
}

// SyncPrivateFile flushes a candidate's bytes before its durable name is published.
func SyncPrivateFile(f *os.File) error { return syncJournal(f) }

// heldLocks records instance locks this process already owns. A second flock in this process fails, so
// in-process registration must notice the journal's lock instead of taking another.
var heldLocks sync.Map

const instanceLockName = "instance.lock"

// LockInstance holds the same lock future offline setup and migration commands must take. Do not remove the
// lock file: doing so would allow different processes to lock different inodes at the same path.
func LockInstance(dir string) (*os.File, error) {
	f, err := lockInstance(dir, os.O_RDWR|os.O_CREATE)
	if err == nil {
		heldLocks.Store(filepath.Clean(dir), f)
	}
	return f, err
}

func lockInstance(dir string, flags int) (*os.File, error) {
	if _, err := CheckPrivate(dir, os.ModeDir|0o700); err != nil {
		return nil, err
	}
	f, err := OpenPrivate(filepath.Join(dir, instanceLockName), flags)
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
	return f, nil
}

// OfflineLock is a freshly acquired instance lock, never one borrowed from a live journal.
type OfflineLock struct {
	dir  string
	file *os.File
	base os.FileInfo
}

// LockOffline requires an initialized instance; it does not create a missing lock file.
func LockOffline(dir string) (*OfflineLock, error) {
	base, err := CheckPrivate(dir, os.ModeDir|0o700)
	if err != nil {
		return nil, err
	}
	f, err := lockInstance(dir, os.O_RDWR)
	if err != nil {
		return nil, err
	}
	l := &OfflineLock{dir: dir, file: f, base: base}
	if err := l.Check(); err != nil {
		l.Close()
		return nil, err
	}
	return l, nil
}

func (l *OfflineLock) Dir() string { return l.dir }

// Check also detects replacement of the state directory or lock inode while we were working.
func (l *OfflineLock) Check() error {
	if l == nil || l.file == nil {
		return ErrLocked
	}
	base, err := CheckPrivate(l.dir, os.ModeDir|0o700)
	if err != nil || !os.SameFile(base, l.base) {
		return ErrLocked
	}
	f, err := l.file.Stat()
	current, pathErr := CheckPrivate(filepath.Join(l.dir, instanceLockName), 0o600)
	if err != nil || pathErr != nil || !os.SameFile(f, current) {
		return ErrLocked
	}
	return nil
}

func (l *OfflineLock) Close() error {
	if l.file == nil {
		return nil
	}
	err := l.file.Close()
	l.file = nil
	return err
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
