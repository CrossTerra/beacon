package actsocket

import (
	"unsafe"

	"golang.org/x/sys/unix"
)

func credentials(fd int) (uid, pid int, err error) {
	c, err := unix.GetsockoptXucred(fd, unix.SOL_LOCAL, unix.LOCAL_PEERCRED)
	if err != nil {
		return 0, 0, err
	}
	if c.Version != 0 { // XUCRED_VERSION in sys/ucred.h
		return 0, 0, errPeer
	}
	pid, err = unix.GetsockoptInt(fd, unix.SOL_LOCAL, unix.LOCAL_PEERPID)
	return int(c.Uid), pid, err
}

// Ask for just the executable path (the proc_pidpath query), not the process's args or environment.
// It is a best-effort label: the peer can exit or exec between the credential read and this query.
func executable(pid int) string {
	const (
		procInfoCallPIDInfo = 2  // PROC_INFO_CALL_PIDINFO in XNU
		procPIDPathInfo     = 11 // PROC_PIDPATHINFO in sys/proc_info.h
	)
	var path [4096]byte // PROC_PIDPATHINFO_MAXSIZE
	//lint:ignore SA1019 x/sys has no proc_pidpath wrapper; this one query keeps the service cgo-independent.
	_, _, err := unix.Syscall6(unix.SYS_PROC_INFO, procInfoCallPIDInfo, uintptr(pid), procPIDPathInfo,
		0, uintptr(unsafe.Pointer(&path[0])), uintptr(len(path)))
	if err != 0 {
		return ""
	}
	return unix.ByteSliceToString(path[:])
}
