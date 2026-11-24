package supervisor

import (
	"encoding/binary"
	"errors"
	"os"
	"strconv"
	"strings"
	"time"
	"unsafe"

	"golang.org/x/sys/unix"
)

type childEvents struct{ pid int }

func watchChild(pid int) (*childEvents, error) { return &childEvents{pid}, nil }

// Read only after silent EOF on the CLOEXEC pipe. Linux sets PF_EXITING before
// closing files on death. A non-exiting child therefore lost the writer at exec,
// not by dying. The unreaped child still owns its PID. A very fast normal exit
// also proves exec if the status is not one the bootstrap or its runtime can
// return. With a signal or an ambiguous status, don't manufacture a launch.
func (c *childEvents) confirmExec(_ time.Time) bool {
	const pfExiting = 0x00000004 // Linux task flags in /proc/PID/stat, include/linux/sched.h
	if b, err := os.ReadFile("/proc/" + strconv.Itoa(c.pid) + "/stat"); err == nil {
		if i := strings.LastIndexByte(string(b), ')'); i >= 0 {
			fields := strings.Fields(string(b[i+1:]))
			if len(fields) > 6 {
				flags, err := strconv.ParseUint(fields[6], 10, 32)
				if err == nil && flags&pfExiting == 0 && fields[0] != "Z" && fields[0] != "X" {
					return true
				}
			}
		}
	}
	if status, ok := c.exitStatus(); ok && status.Exited() {
		switch status.ExitStatus() {
		case bootstrapFailed, 2, 66: // refused exec, Go fatal error, race-detector failure
			return false
		default:
			return true
		}
	}
	return false
}

func (c *childEvents) exitStatus() (unix.WaitStatus, bool) { return childExitStatus(c.pid) }

func (*childEvents) close() {}

func childExitStatus(pid int) (unix.WaitStatus, bool) {
	// Waitid consults the child's wait status without reaping, so the group still
	// owns its PGID. Use the kernel siginfo_t's 128-byte buffer and native alignment.
	const wnowait = 0x01000000
	var storage [16]uint64
	info := (*unix.Siginfo)(unsafe.Pointer(&storage[0]))
	for {
		err := unix.Waitid(unix.P_PID, pid, info, unix.WEXITED|unix.WNOHANG|wnowait, nil)
		if errors.Is(err, unix.EINTR) {
			continue
		}
		if err != nil || info.Signo != int32(unix.SIGCHLD) {
			return 0, false
		}
		break
	}
	off := 12 // union containing si_pid, si_uid, si_status
	if unsafe.Sizeof(uintptr(0)) == 8 {
		off = 16
	}
	b := unsafe.Slice((*byte)(unsafe.Pointer(info)), off+12)
	if binary.NativeEndian.Uint32(b[off:]) != uint32(pid) {
		return 0, false
	}
	status := unix.WaitStatus(binary.NativeEndian.Uint32(b[off+8:]))
	switch info.Code {
	case 1: // CLD_EXITED
		return status << 8, true
	case 2: // CLD_KILLED
		return status, true
	case 3: // CLD_DUMPED
		return status | 0x80, true
	}
	return 0, false
}
