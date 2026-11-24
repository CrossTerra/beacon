package supervisor

import (
	"errors"
	"time"

	"golang.org/x/sys/unix"
)

// Register while the bootstrap is gated. NOTE_EXEC remains available even if the
// new image exits before the reaper reads the close-on-exec pipe.
type childEvents struct {
	fd     int
	exec   bool
	exited bool
	status unix.WaitStatus
}

func watchChild(pid int) (*childEvents, error) {
	fd, err := unix.Kqueue()
	if err != nil {
		return nil, err
	}
	unix.CloseOnExec(fd)
	var change [1]unix.Kevent_t
	unix.SetKevent(&change[0], pid, unix.EVFILT_PROC, unix.EV_ADD|unix.EV_CLEAR)
	change[0].Fflags = unix.NOTE_EXEC | unix.NOTE_EXIT | unix.NOTE_EXITSTATUS
	if _, err := unix.Kevent(fd, change[:], nil, nil); err != nil {
		unix.Close(fd)
		return nil, err
	}
	return &childEvents{fd: fd}, nil
}

func (c *childEvents) read(wait time.Duration) error {
	var event [1]unix.Kevent_t
	timeout := unix.NsecToTimespec(int64(wait))
	if n, err := unix.Kevent(c.fd, nil, event[:], &timeout); err != nil {
		return err
	} else if n == 1 && event[0].Flags&unix.EV_ERROR == 0 {
		flags := event[0].Fflags
		c.exec = c.exec || flags&unix.NOTE_EXEC != 0
		if flags&unix.NOTE_EXITSTATUS != 0 {
			c.exited, c.status = true, unix.WaitStatus(event[0].Data)
		}
	}
	return nil
}

func (c *childEvents) confirmExec(deadline time.Time) bool {
	for !c.exec && !c.exited {
		left := time.Until(deadline)
		if left <= 0 {
			break
		}
		if err := c.read(left); err != nil && !errors.Is(err, unix.EINTR) {
			break
		}
	}
	return c.exec
}

func (c *childEvents) exitStatus() (unix.WaitStatus, bool) {
	for !c.exited && errors.Is(c.read(0), unix.EINTR) {
	}
	return c.status, c.exited
}

func (c *childEvents) close() { _ = unix.Close(c.fd) }
