package actsocket

import (
	"os"
	"strconv"

	"golang.org/x/sys/unix"
)

func credentials(fd int) (uid, pid int, err error) {
	c, err := unix.GetsockoptUcred(fd, unix.SOL_SOCKET, unix.SO_PEERCRED)
	if err != nil {
		return 0, 0, err
	}
	return int(c.Uid), int(c.Pid), nil
}

func executable(pid int) string {
	path, _ := os.Readlink("/proc/" + strconv.Itoa(pid) + "/exe")
	return path
}
