package supervisor

import (
	"bytes"
	"encoding/binary"
	"os"
	"runtime"
	"unsafe"

	"golang.org/x/sys/unix"
)

func processes() (processTable, error) {
	entries, err := unix.SysctlKinfoProcSlice("kern.proc.uid", os.Getuid())
	if err != nil {
		return nil, err
	}
	table := make(processTable, len(entries))
	for _, k := range entries {
		if p := fromKinfo(&k); p.pid > 1 {
			table[p.pid] = p
		}
	}
	return table, nil
}

func lookupProcess(pid int) (process, bool) {
	if pid <= 1 {
		return process{}, false
	}
	k, err := unix.SysctlKinfoProc("kern.proc.pid", pid)
	if err != nil || k.Eproc.Ucred.Uid != uint32(os.Getuid()) || int(k.Proc.P_pid) != pid {
		return process{}, false
	}
	return fromKinfo(k), true
}

func fromKinfo(k *unix.KinfoProc) process {
	birth := uint64(k.Proc.P_starttime.Sec)*1_000_000 + uint64(k.Proc.P_starttime.Usec)
	const szomb = 5 // sys/proc.h
	return process{int(k.Proc.P_pid), int(k.Eproc.Ppid), int(k.Eproc.Pgid), birth, k.Proc.P_stat == szomb}
}

func markerOf(pid int, prefix string) uint64 {
	// KERN_PROCARGS2 has no named sysctl or x/sys wrapper on Darwin. The buffer is private
	// to this call. XNU can deny this query or omit the environment of platform binaries.
	mib := [3]int32{1, 49, int32(pid)} // CTL_KERN, KERN_PROCARGS2, pid
	var size uintptr
	if procArgs(&mib, nil, &size) != nil || size < 4 || size > 1<<20 {
		return 0
	}
	b := make([]byte, size)
	if procArgs(&mib, &b[0], &size) != nil || size > uintptr(len(b)) {
		return 0
	}
	return findMarker(darwinEnv(b[:size]), prefix)
}

func procArgs(mib *[3]int32, buf *byte, n *uintptr) error {
	_, _, errno := unix.Syscall6(unix.SYS___SYSCTL, uintptr(unsafe.Pointer(mib)), uintptr(len(mib)),
		uintptr(unsafe.Pointer(buf)), uintptr(unsafe.Pointer(n)), 0, 0)
	runtime.KeepAlive(mib)
	runtime.KeepAlive(buf)
	runtime.KeepAlive(n)
	if errno != 0 {
		return errno
	}
	return nil
}

func darwinEnv(b []byte) []byte {
	if len(b) < 4 {
		return nil
	}
	argc := binary.NativeEndian.Uint32(b)
	_, tail, ok := bytes.Cut(b[4:], []byte{0}) // executable path, then NUL padding
	if !ok {
		return nil
	}
	tail = bytes.TrimLeft(tail, "\x00")
	for range argc {
		_, tail, ok = bytes.Cut(tail, []byte{0})
		if !ok {
			return nil
		}
	}
	// Stop at the first empty entry: the kernel may follow it with auxiliary strings.
	if end := bytes.Index(tail, []byte{0, 0}); end >= 0 {
		return tail[:end+1]
	}
	return tail
}

func deviceBits(st unix.Stat_t) uint64 { return uint64(uint32(st.Dev)) }
