package supervisor

import (
	"encoding/binary"
	"errors"
	"os"
	"syscall"

	"golang.org/x/sys/unix"
)

// Only the service and reaper hold these endpoints. Each prepare with a directory
// sends one descriptor on this channel before the matching control frame. The
// wire lock keeps both sends in order. Neither end waits on a missing parcel.
func descriptorPair() (service, reaper *os.File, err error) {
	syscall.ForkLock.RLock()
	defer syscall.ForkLock.RUnlock()
	fds, err := unix.Socketpair(unix.AF_UNIX, unix.SOCK_DGRAM, 0)
	if err != nil {
		return nil, nil, err
	}
	for _, fd := range fds {
		unix.CloseOnExec(fd)
		if err := unix.SetNonblock(fd, true); err != nil {
			unix.Close(fds[0])
			unix.Close(fds[1])
			return nil, nil, err
		}
	}
	return os.NewFile(uintptr(fds[0]), "checkout sender"), os.NewFile(uintptr(fds[1]), "checkout receiver"), nil
}

func sendDirectory(socket *os.File, ticket uint64, dir *Directory) error {
	if dir.file == nil {
		return ErrLaunch
	}
	conn, err := socket.SyscallConn()
	if err != nil {
		return err
	}
	var body [8]byte
	binary.BigEndian.PutUint64(body[:], ticket)
	fd := int(dir.file.Fd()) // caller holds the pin for the duration of the request
	if err := conn.Control(func(sock uintptr) {
		for {
			n, sendErr := unix.SendmsgN(int(sock), body[:], unix.UnixRights(fd), nil, unix.MSG_DONTWAIT)
			if errors.Is(sendErr, unix.EINTR) {
				continue
			}
			if sendErr != nil || n != len(body) {
				err = ErrReaper
			}
			return
		}
	}); err != nil {
		return err
	}
	return err
}

func receiveDirectory(socket *os.File, ticket uint64) (*os.File, error) {
	var body [8]byte
	oob := make([]byte, unix.CmsgSpace(4))
	var n, cn, flags int
	var err error
	fd := int(socket.Fd()) // only the reaper event loop uses this endpoint
	for {
		n, cn, flags, _, err = unix.Recvmsg(fd, body[:], oob, unix.MSG_DONTWAIT)
		if !errors.Is(err, unix.EINTR) {
			break
		}
	}
	if err != nil {
		return nil, ErrReaper
	}
	msgs, err := unix.ParseSocketControlMessage(oob[:cn])
	var received []int
	for _, m := range msgs {
		rights, e := unix.ParseUnixRights(&m)
		if e != nil {
			err = e
		}
		received = append(received, rights...)
	}
	if n != len(body) || flags&(unix.MSG_TRUNC|unix.MSG_CTRUNC) != 0 || err != nil ||
		len(received) != 1 || binary.BigEndian.Uint64(body[:]) != ticket {
		for _, fd := range received {
			unix.Close(fd)
		}
		return nil, ErrReaper
	}
	// No other thread in the reaper spawns children.
	unix.CloseOnExec(received[0])
	return os.NewFile(uintptr(received[0]), "pinned checkout"), nil
}
