package supervisor

import (
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
	var st unix.Stat_t
	if unix.Fstat(int(f.Fd()), &st) != nil || st.Uid != uint32(os.Getuid()) {
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

func deviceBits(st unix.Stat_t) uint64 { return st.Dev }
