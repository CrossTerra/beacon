package supervisor

import (
	"bytes"
	"strconv"
)

const markerKey = "BEACON_SUPERVISOR"

// A PID by itself neither identifies a descendant across snapshots nor authorizes a signal.
type process struct {
	pid, parent, group int
	birth              uint64
	zombie             bool
}

type processTable map[int]process

// Only the supervisor's own marker is interpreted or retained. In particular, none of the other
// environment entries (or, on macOS, arguments) may enter a log, an error, or an attribution label.
func findMarker(env []byte, prefix string) uint64 {
	for len(env) != 0 {
		entry, rest, ok := bytes.Cut(env, []byte{0})
		if !ok {
			break
		}
		if tag, ok := bytes.CutPrefix(entry, []byte(prefix)); ok && len(tag) <= 20 {
			if id, err := strconv.ParseUint(string(tag), 10, 64); err == nil && id != 0 {
				return id
			}
		}
		env = rest
	}
	return 0
}
