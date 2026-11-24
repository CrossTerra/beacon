package supervisor

import (
	"context"
	"errors"
	"os"
	"os/exec"
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

func TestDrainCancelSkipsKill(t *testing.T) {
	// The startup sweep must not pass the service lifetime here: cancellation returns
	// before KILL. sweepPrevious uses context.Background so a dying serve cannot do that.
	const pid = 1_000_000_001
	e := newEngine("", sessionPrefix("fixture"), timing{grace: time.Second, killWait: time.Second}, nil)
	e.list = func() (processTable, error) {
		return processTable{pid: {pid: pid, group: pid}}, nil
	}
	e.jobs[pid] = &job{pid: pid, tag: 1}
	var signals []unix.Signal
	e.signal = func(target int, sig unix.Signal) {
		if target == -pid {
			signals = append(signals, sig)
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := e.drain(ctx); !errors.Is(err, ErrUnconfirmed) {
		t.Fatalf("cancelled drain: %v", err)
	}
	if slices.Contains(signals, unix.SIGKILL) {
		t.Fatal("cancelled drain sent KILL")
	}
}

func TestDrainReportsExpiredRunImmediately(t *testing.T) {
	const pid = 1_000_000_002
	var reported []message
	e := newEngine("", sessionPrefix("fixture"), timing{grace: time.Second, killWait: time.Second}, func(m message) error {
		reported = append(reported, m)
		return nil
	})
	e.list = func() (processTable, error) {
		return processTable{pid: {pid: pid, group: pid}}, nil
	}
	e.signal = func(int, unix.Signal) {}
	e.jobs[pid] = &job{pid: pid, tag: 1, ticket: 7, killAt: time.Now().Add(-time.Hour)}
	e.runs[7] = e.jobs[pid]
	start := time.Now()
	err := e.drain(context.Background())
	if !errors.Is(err, ErrUnconfirmed) {
		t.Fatalf("gave up without keeping the session: %v", err)
	}
	if time.Since(start) > 200*time.Millisecond {
		t.Fatal("drain granted another full grace before reporting")
	}
	if len(reported) != 1 || reported[0].Finished != 7 || reported[0].Error != codeRecovery {
		t.Fatalf("expired run report: %+v", reported)
	}
	if _, ok := e.jobs[pid]; !ok {
		t.Fatal("expired run was dropped before its process was confirmed gone")
	}
}

func TestSweepRequiresTwoEmptyCensuses(t *testing.T) {
	var n int
	e := newEngine("", sessionPrefix("fixture"), timing{grace: time.Second, killWait: time.Second}, nil)
	e.signal = func(int, unix.Signal) {}
	e.list = func() (processTable, error) {
		n++
		return processTable{}, nil
	}
	if err := e.drain(context.Background()); err != nil {
		t.Fatal(err)
	}
	if n < 2 {
		t.Fatalf("confirmed cleanup after %d empty census", n)
	}
}

func TestDrainKeepsMarkerWhenRunExpiresMidSweep(t *testing.T) {
	const pid = 1_000_000_009
	var reported []message
	e := newEngine("", sessionPrefix("fixture"), timing{grace: time.Second, killWait: 20 * time.Millisecond}, func(m message) error {
		reported = append(reported, m)
		return nil
	})
	e.signal = func(int, unix.Signal) {}
	start := time.Now()
	e.jobs[pid] = &job{pid: pid, tag: 1, ticket: 9, killAt: start.Add(40 * time.Millisecond)}
	e.runs[9] = e.jobs[pid]
	e.list = func() (processTable, error) {
		if time.Since(start) < 250*time.Millisecond {
			return processTable{pid: {pid: pid, group: pid}}, nil
		}
		return processTable{}, nil
	}
	if err := e.drain(context.Background()); !errors.Is(err, ErrUnconfirmed) {
		t.Fatalf("cleared the marker after a run expired during drain: %v", err)
	}
	if len(reported) != 1 || reported[0].Finished != 9 || reported[0].Error != codeRecovery {
		t.Fatalf("expiry during drain: %+v", reported)
	}
}

func TestExpiryStopsAfterReplyFailure(t *testing.T) {
	var n int
	e := newEngine("", sessionPrefix("fixture"), timing{grace: time.Second, killWait: time.Millisecond}, func(message) error {
		n++
		return errors.New("reply pipe")
	})
	past := time.Now().Add(-time.Hour)
	for _, ticket := range []uint64{1, 2} {
		pid := int(ticket) + 1_000_000_100
		e.jobs[pid] = &job{pid: pid, tag: ticket, ticket: ticket, killAt: past}
	}
	if !e.reportExpired(time.Now()) || n != 1 || e.emit != nil {
		t.Fatalf("kept notifying after a reply failure: n=%d emit=%v", n, e.emit != nil)
	}
}

func TestReleaseDeadlineStaysInsideRequestWait(t *testing.T) {
	now := time.Now()
	req, wait := releaseRequest(1, now)
	until := time.Unix(0, req.Until)
	if !until.Before(now.Add(wait)) {
		t.Fatal("exec deadline is not inside the request wait")
	}
	if got := releaseDeadline(now, until); got.After(until) || !got.Before(now.Add(wait)) {
		t.Fatalf("reaper deadline exceeds the request timeout: %s", got.Sub(now))
	}
	// A delayed start must not grant a fresh wireTimeout past the caller.
	if got := releaseDeadline(now.Add(wireTimeout), until); got.After(until) {
		t.Fatalf("late release extended past the request: %s", got.Sub(until))
	}
	if got := releaseDeadline(now, time.Time{}); !got.Equal(now.Add(wireTimeout)) {
		t.Fatalf("unbounded release: %s", got.Sub(now))
	}
}

func TestLeaderExitWaitsForFreshCensus(t *testing.T) {
	const leader, child = 1_000_000_003, 1_000_000_004
	var census int
	e := newEngine("", sessionPrefix("fixture"), timing{grace: time.Second, killWait: time.Second}, nil)
	e.signal = func(int, unix.Signal) {}
	e.list = func() (processTable, error) {
		census++
		table := processTable{leader: {pid: leader, group: leader, zombie: true, birth: 1}}
		if census >= 2 {
			table[child] = process{pid: child, parent: leader, group: leader, birth: 2}
		}
		return table, nil
	}
	e.jobs[leader] = &job{pid: leader, tag: 1}
	if _, err := e.refresh(false); err != nil {
		t.Fatal(err)
	}
	if _, ok := e.jobs[leader]; !ok {
		t.Fatal("released the group on the census that first saw the leader exit")
	}
	if e.tracked[child].job != nil {
		t.Fatal("child was visible before the fresh census")
	}
	if _, err := e.refresh(false); err != nil {
		t.Fatal(err)
	}
	if e.tracked[child].job != e.jobs[leader] {
		t.Fatal("fresh census missed the child forked as the leader exited")
	}
	if _, ok := e.jobs[leader]; !ok {
		t.Fatal("released the group while the new child was still live")
	}
}

func TestAttributeRejectsUnreadablePeer(t *testing.T) {
	e := newEngine("", sessionPrefix("fixture"), timing{}, nil)
	if id, err := e.attribute(os.Getpid()); err != nil || id != "" {
		t.Fatalf("self is verified non-membership: %q %v", id, err)
	}
	if id, err := e.attribute(1 << 30); err == nil || id != "" {
		t.Fatalf("vanished pid was accepted: %q %v", id, err)
	}
}

func TestAttributeRejectsZombie(t *testing.T) {
	truePath, err := exec.LookPath("true")
	if err != nil {
		t.Fatal(err)
	}
	p, err := os.StartProcess(truePath, []string{"true"}, &os.ProcAttr{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		var status unix.WaitStatus
		_, _ = unix.Wait4(p.Pid, &status, 0, nil)
	})
	e := newEngine("", sessionPrefix("fixture"), timing{}, nil)
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		id, err := e.attribute(p.Pid)
		if err != nil && id == "" {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("zombie or exited peer was accepted as unattributed")
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
