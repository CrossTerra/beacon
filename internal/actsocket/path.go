package actsocket

import (
	"context"
	"errors"
	"net"
	"os"
	"path/filepath"
	"sync"
	"syscall"
	"time"

	"github.com/CrossTerra/beacon/internal/approval"
)

const socketMode = os.ModeSocket | 0o600

var ErrInUse = errors.New("the proposal socket is already in use")

func paths(dir string) (run, sock string) {
	run = filepath.Join(dir, "run")
	return run, filepath.Join(run, "act.sock")
}

type listener struct {
	*net.UnixListener
	path string
	info os.FileInfo
	once sync.Once
	err  error
}

func listen(dir string) (*listener, error) {
	dir, err := filepath.Abs(dir) // pathname socket, even if a relative directory begins with '@'
	if err != nil {
		return nil, err
	}
	if _, err := approval.CheckPrivate(dir, os.ModeDir|0o700); err != nil {
		return nil, err
	}
	run, path := paths(dir)
	if err := approval.PrivateDir(run); err != nil {
		return nil, err
	}
	if err := removeStale(path); err != nil {
		return nil, err
	}
	l, err := net.ListenUnix("unix", &net.UnixAddr{Name: path, Net: "unix"})
	if err != nil {
		return nil, err
	}
	l.SetUnlinkOnClose(false)
	info, err := os.Lstat(path)
	if err != nil {
		l.Close()
		return nil, err
	}
	s := &listener{UnixListener: l, path: path, info: info}
	if err := os.Chmod(path, 0o600); err != nil {
		s.Close()
		return nil, err
	}
	return s, nil
}

// Only a refused dial to an existing, private socket indicates a stale listener. In particular, a
// timeout (e.g. a full listen backlog) is not permission to unlink, nor is a non-socket or a symlink.
func removeStale(path string) error {
	info, err := approval.CheckPrivate(path, socketMode)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	c, err := net.DialTimeout("unix", path, time.Second)
	if err == nil {
		c.Close()
		return ErrInUse
	}
	if !errors.Is(err, syscall.ECONNREFUSED) {
		return err
	}
	return removeSocket(path, info)
}

func removeSocket(path string, bound os.FileInfo) error {
	current, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) || err == nil && !os.SameFile(current, bound) {
		return nil // never remove a replacement
	}
	if err != nil {
		return err
	}
	return os.Remove(path)
}

func (s *listener) Close() error {
	s.once.Do(func() { s.err = errors.Join(s.UnixListener.Close(), removeSocket(s.path, s.info)) })
	return s.err
}

func dial(ctx context.Context, dir string) (*net.UnixConn, error) {
	dir, err := filepath.Abs(dir)
	if err != nil {
		return nil, err
	}
	run, path := paths(dir)
	for _, parent := range []string{dir, run} {
		if _, err := approval.CheckPrivate(parent, os.ModeDir|0o700); err != nil {
			return nil, err
		}
	}
	if _, err := approval.CheckPrivate(path, socketMode); err != nil {
		return nil, err
	}
	d := net.Dialer{Timeout: frameTimeout}
	c, err := d.DialContext(ctx, "unix", path)
	if err != nil {
		return nil, errors.New("the Go proposal socket is not open")
	}
	u := c.(*net.UnixConn)
	// Also check the connected endpoint. Paths can move between lstat and connect.
	if _, err := peerPID(u, os.Getuid()); err != nil {
		c.Close()
		return nil, err
	}
	return u, nil
}
