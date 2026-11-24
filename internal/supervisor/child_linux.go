package supervisor

import (
	"bytes"
	"encoding/binary"
	"errors"
	"os"
	"strconv"
	"strings"
	"time"
	"unsafe"

	"golang.org/x/sys/unix"
)

type childEvents struct {
	pid    int
	target os.FileInfo
	argv   []byte
}

func watchChild(pid int, argv []string) (*childEvents, error) {
	c := &childEvents{pid: pid}
	if len(argv) > 0 {
		want := []byte(strings.Join(argv, "\x00") + "\x00")
		initial, err := os.ReadFile(c.proc("cmdline"))
		if err == nil && len(initial) > 0 && !bytes.Equal(initial, want) {
			c.target, _ = os.Stat(argv[0]) // let an explicit exec error take precedence over missing evidence
			c.argv = want
		}
	}
	return c, nil
}

func (c *childEvents) proc(name string) string { return "/proc/" + strconv.Itoa(c.pid) + "/" + name }

// CLOEXEC can close the pipe before the new image's argument tables are installed.
// Require both the target inode and the new argv, distinct from the bootstrap's.
// The reaper still owns the PID. If the image isn't observable, no exit code by
// itself proves what ran in it.
func (c *childEvents) confirmExec(deadline time.Time) bool {
	defer func() { c.target, c.argv = nil, nil }()
	for c.target != nil && time.Now().Before(deadline) {
		if exe, err := os.Stat(c.proc("exe")); err == nil && os.SameFile(c.target, exe) {
			if argv, err := os.ReadFile(c.proc("cmdline")); err == nil && bytes.Equal(argv, c.argv) {
				return true
			}
		}
		if _, exited := c.exitStatus(); exited {
			break
		}
		time.Sleep(min(time.Until(deadline), time.Millisecond))
	}
	return false
}

func markNonStdio() error {
	if unix.CloseRange(3, 1<<32-1, unix.CLOSE_RANGE_CLOEXEC) == nil {
		return nil
	}
	return markFDDirectory("/proc/self/fd")
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
