package supervisor

import (
	"errors"
	"io"
	"os"
	"strconv"
	"strings"

	"golang.org/x/sys/unix"
)

func processes() (processTable, error) {
	entries, err := os.ReadDir("/proc")
	if err != nil {
		return nil, err
	}
	table := make(processTable)
	for _, entry := range entries {
		if pid, err := strconv.Atoi(entry.Name()); err == nil {
			if p, ok := lookupProcess(pid); ok {
				table[pid] = p
			}
		}
	}
	return table, nil
}

func lookupProcess(pid int) (process, bool) {
	if pid <= 1 {
		return process{}, false
	}
	path := "/proc/" + strconv.Itoa(pid) + "/stat"
	f, err := os.Open(path)
	if err != nil {
		return process{}, false
	}
	defer f.Close()
	// stat can be root-owned for a non-dumpable process of this user. The UID in
	// status is the credential; a failed lookup is not treated as death.
	if !sameUser(pid) {
		return process{}, false
	}
	b, err := io.ReadAll(f)
	if err != nil {
		return process{}, false
	}
	return parseStat(pid, string(b))
}

func parseStat(pid int, stat string) (process, bool) {
	// comm can contain spaces, newlines and closing parens. The last ')' ends field 2.
	i := strings.LastIndexByte(stat, ')')
	if i == -1 {
		return process{}, false
	}
	fields := strings.Fields(stat[i+1:])
	if len(fields) < 20 {
		return process{}, false
	}
	parent, e1 := strconv.Atoi(fields[1])
	group, e2 := strconv.Atoi(fields[2])
	birth, e3 := strconv.ParseUint(fields[19], 10, 64) // field 22, in ticks since boot
	return process{pid, parent, group, birth, fields[0] == "Z" || fields[0] == "X"}, e1 == nil && e2 == nil && e3 == nil
}

func markerOf(pid int, prefix string) uint64 {
	f, err := os.Open("/proc/" + strconv.Itoa(pid) + "/environ")
	if err != nil {
		return 0
	}
	defer f.Close()
	// Linux bounds argv plus env to at most 6 MiB on supported architectures.
	b, err := io.ReadAll(io.LimitReader(f, 6<<20))
	if err != nil {
		return 0
	}
	return findMarker(b, prefix)
}

func sameUser(pid int) bool {
	b, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/status")
	if err != nil {
		return false
	}
	uid, ok := statusUID(string(b))
	return ok && uid == uint32(os.Getuid())
}

func statusUID(status string) (uint32, bool) {
	for _, line := range strings.Split(status, "\n") {
		if !strings.HasPrefix(line, "Uid:") {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) < 2 {
			return 0, false
		}
		u, err := strconv.ParseUint(fields[1], 10, 32)
		return uint32(u), err == nil
	}
	return 0, false
}

// leaderDead is true only when the process table or a non-reaping wait shows the leader has
// exited. A failed /proc lookup is not death: a non-dumpable process can hide its stat file.
func leaderDead(pid int, table processTable) bool {
	if p, ok := table[pid]; ok {
		return p.zombie
	}
	return confirmedExit(pid)
}

func confirmedExit(pid int) bool {
	_, ok := childExitStatus(pid)
	return ok // ECHILD is not evidence that a leader exited
}

// processGone confirms that the process recorded with this start time no longer exists. Only
// the reaper's own children can be confirmed by waitid; anything else (a detached descendant, an
// orphan from a startup sweep) is reaped by its own parent or init, so ESRCH from kill is its
// death. A PID that exists with another start time has been reused. A process that exists but
// whose stat cannot be read is not dead.
func processGone(pid int, birth uint64) bool {
	if p, ok := lookupProcess(pid); ok {
		return p.zombie || p.birth != birth
	}
	if pid <= 1 {
		return false
	}
	if confirmedExit(pid) {
		return true
	}
	if err := unix.Kill(pid, 0); errors.Is(err, unix.ESRCH) {
		return true
	} else if err != nil && !errors.Is(err, unix.EPERM) {
		return false
	}
	// The PID exists, possibly for another user. stat is world-readable even when status is not.
	b, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/stat")
	if err != nil {
		return os.IsNotExist(err) && errors.Is(unix.Kill(pid, 0), unix.ESRCH)
	}
	p, ok := parseStat(pid, string(b))
	return ok && (p.zombie || p.birth != birth)
}

func deviceBits(st unix.Stat_t) uint64 { return st.Dev }
