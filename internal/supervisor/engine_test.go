package supervisor

import (
	"context"
	"errors"
	"slices"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

func TestDrainGivesUpAfterKill(t *testing.T) {
	// Model a process stuck in the kernel, without sending any signals to real processes.
	const pid = 1_000_000_000
	for _, unreadable := range []bool{false, true} {
		e := newEngine("", sessionPrefix("fixture"), timing{grace: 50 * time.Millisecond, killWait: 300 * time.Millisecond}, nil)
		e.list = func() (processTable, error) {
			if unreadable {
				return nil, errors.New("cannot read process table")
			}
			return processTable{pid: {pid: pid, group: pid}}, nil
		}
		e.jobs[pid] = &job{pid: pid, tag: 1}
		var signals []unix.Signal
		e.signal = func(target int, signal unix.Signal) {
			if target == -pid {
				signals = append(signals, signal)
			}
		}
		start := time.Now()
		if err := e.drain(context.Background()); !errors.Is(err, ErrUnconfirmed) {
			t.Fatalf("confirmed a stuck process (unreadable=%v): %v", unreadable, err)
		}
		if len(signals) < 2 || signals[0] != unix.SIGTERM || !slices.Contains(signals[1:], unix.SIGKILL) || time.Since(start) > 3*time.Second {
			t.Fatalf("unbounded or wrong signal sequence: %v", signals)
		}
	}
}

func TestMarkerMatchesOnlyTheSessionAndEntry(t *testing.T) {
	const prefix = "BEACON_SUPERVISOR=beacon-test:"
	for _, env := range []string{"OTHER=" + prefix + "1\x00", prefix + "1extra\x00", prefix + "1", "BEACON_SUPERVISOR=beacon-test-more:1\x00"} {
		if tag := findMarker([]byte(env), prefix); tag != 0 {
			t.Fatal("matched an unrelated process")
		}
	}
	if got := findMarker([]byte("OTHER=1\x00"+prefix+"123\x00MORE=2\x00"), prefix); got != 123 {
		t.Fatalf("marker: got %d, want 123", got)
	}
}
