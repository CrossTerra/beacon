package actsocket

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"io"
	"net"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/CrossTerra/beacon/internal/approval"
)

type object = approval.Object

func check(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

func testDir(t *testing.T) string {
	t.Helper()
	// macOS's sockaddr_un has room for only 103 pathname bytes.
	dir, err := os.MkdirTemp("", "ba-")
	check(t, err)
	t.Cleanup(func() { check(t, os.RemoveAll(dir)) })
	return dir
}

func origin(t *testing.T, url, login string) approval.Origin {
	t.Helper()
	o, err := approval.ParseOrigin(url, login)
	check(t, err)
	return o
}

func fixture(t *testing.T, opts Options, lim limits, wrongUID bool) (string, *Server) {
	t.Helper()
	dir := testDir(t)
	if opts.Origin.Host() == "" {
		opts.Origin = origin(t, approval.LocalOrigin, "")
	}
	j, err := approval.OpenJournal(dir, time.Now(), "")
	check(t, err)
	t.Cleanup(func() { check(t, j.Close()) })
	s, err := listenServer(dir, j, opts, lim)
	check(t, err)
	if wrongUID {
		s.uid++ // compare a real kernel credential to a different uid on both platforms
	}
	done := make(chan error, 1)
	go func() { done <- s.Serve(t.Context()) }()
	t.Cleanup(func() {
		check(t, s.Close())
		select {
		case err := <-done:
			check(t, err)
		case <-time.After(5 * time.Second):
			t.Error("socket did not join connections")
		}
	})
	return dir, s
}

type wire struct {
	*net.UnixConn
	r *bufio.Reader
	t *testing.T
}

func connect(t *testing.T, dir string) *wire {
	t.Helper()
	_, path := paths(dir)
	c, err := net.DialUnix("unix", nil, &net.UnixAddr{Name: path, Net: "unix"})
	check(t, err)
	t.Cleanup(func() { c.Close() })
	check(t, c.SetDeadline(time.Now().Add(10*time.Second)))
	return &wire{c, bufio.NewReader(c), t}
}

func (w *wire) send(o object) {
	w.t.Helper()
	b, err := marshalFrame(o, FrameMax)
	check(w.t, err)
	_, err = w.Write(b)
	check(w.t, err)
}

func (w *wire) receive() object {
	w.t.Helper()
	b, err := w.r.ReadBytes('\n')
	check(w.t, err)
	o, err := approval.ParseObject(b)
	check(w.t, err)
	return o
}

func (w *wire) call(o object) object {
	w.t.Helper()
	w.send(o)
	return w.receive()
}

func (w *wire) closed() {
	w.t.Helper()
	if b, err := w.r.ReadBytes('\n'); !errors.Is(err, io.EOF) || len(b) != 0 {
		w.t.Fatalf("want silent close, got %q: %v", b, err)
	}
}

func want(t *testing.T, got, expected object) {
	t.Helper()
	if !reflect.DeepEqual(got, expected) {
		t.Fatalf("got %v; want %v", got, expected)
	}
}

func merge(n int64) object {
	return object{"op": "propose", "type": "github.merge", "reason": "Reviewed", "agent": object{"name": "bosun", "session": "ses_1"},
		"context": object{"subject": "github:pr:crossterra/signal#140"},
		"params":  object{"repo": "CrossTerra/signal", "number": n, "base": "main", "head": strings.Repeat("a", 40), "method": "squash"}}
}

func proposalID(t *testing.T, o object) string {
	t.Helper()
	id, _ := o["id"].(string)
	if !approval.ValidID(id, "apr") || o["status"] != "pending" || o["version"] != int64(1) {
		t.Fatalf("unexpected propose reply: %v", o)
	}
	return id
}

func TestWireProtocolAndKernelLabel(t *testing.T) {
	canonical := origin(t, "https://machine.tail123.ts.net", "person@example.test")
	dir, s := fixture(t, Options{Origin: canonical, Enabled: []approval.Action{approval.Merge}, IssueOpen: func() (string, bool) {
		return "code", true
	}}, socketLimits, false)
	c := connect(t, dir)
	_, path := paths(dir)
	_, err := approval.CheckPrivate(path, socketMode)
	check(t, err)
	want(t, c.call(object{"op": "types"}), object{"types": []any{object{"type": "github.merge", "params": approval.Merge.ParamsSchema()}}})
	got := c.call(merge(140))
	id := proposalID(t, got)
	p, _, err := s.journal.Get(id, time.Now())
	check(t, err)
	if got["params_hash"] != p.ParamsHash || got["review_url"] != canonical.URL()+"/p/"+id || p.Agent != "bosun" || p.Session != "ses_1" {
		t.Fatal("reply did not describe the stored proposal", got, p)
	}
	if p.Peer.PID != os.Getpid() || !filepath.IsAbs(p.Peer.Executable) || p.Peer.RunID != "" {
		t.Fatalf("missing or wrongly trusted peer label: %+v", p.Peer)
	}
	// Another unattributed connection can get and withdraw it. The client does the path and peer checks.
	b, err := Call(t.Context(), dir, object{"op": "get", "id": id}, 0)
	check(t, err)
	o, err := approval.ParseObject(b)
	check(t, err)
	want(t, o, object{"id": id, "version": int64(1), "status": "pending"})
	want(t, connect(t, dir).call(object{"op": "withdraw", "id": id}), object{"id": id, "version": int64(2), "status": "withdrawn"})
	want(t, c.call(object{"op": "withdraw", "id": id}), failure("not_pending"))
	want(t, c.call(object{"op": "open"}), object{"code": "code", "url": canonical.URL() + "/open?code=code"})
}

func TestWrongKernelUIDClosedBeforeReading(t *testing.T) {
	dir, s := fixture(t, Options{Enabled: []approval.Action{approval.Merge}}, socketLimits, true)
	connect(t, dir).closed()
	p, err := s.journal.Pending(time.Now())
	check(t, err)
	if len(p) != 0 {
		t.Fatal("unexpected proposal")
	}
}

func TestUntrustedFramesAndOperations(t *testing.T) {
	dir, s := fixture(t, Options{Enabled: []approval.Action{approval.Merge}}, socketLimits, false)
	w := connect(t, dir)
	for _, bad := range []string{"{", "null", `{"op":"types","op":"types"}`, `{"op":"types","number":1.0}`, `{"op":"types"} {}`, `{"op":"\ud800"}`} {
		_, err := w.Write([]byte(bad + "\n"))
		check(t, err)
		want(t, w.receive(), failure("invalid_frame"))
	}
	for _, op := range []any{"approve", "list", "history", "runners", "decide", "quit", []any{"get"}, object{}} {
		want(t, w.call(object{"op": op}), invalid("/op"))
	}
	want(t, w.call(object{"op": "types", "x": int64(1)}), invalid(""))
	want(t, w.call(object{"op": "open", "x": int64(1)}), invalid(""))
	for _, tc := range []struct {
		key   string
		value any
		field string
	}{
		{"extra", true, "/extra"}, {"reason", "", "/reason"}, {"agent", object{"name": "bosun", "pid": 1}, "/agent/pid"},
		{"proposer", object{"verified": true}, "/proposer"}, {"context", object{"runner_id": "fake"}, "/context/runner_id"},
	} {
		o := merge(1)
		o[tc.key] = tc.value
		want(t, w.call(o), invalid(tc.field))
	}
	slack := merge(1)
	slack["type"] = "slack.post"
	slack["params"] = object{"workspace": "T1", "channel": "C1", "thread_ts": nil, "text": "hi"}
	want(t, w.call(slack), failure("type_disabled"))
	want(t, w.call(object{"op": "get", "id": "wrong"}), invalid("/id"))
	p, err := s.journal.Pending(time.Now())
	check(t, err)
	if len(p) != 0 {
		t.Fatal("a refused frame appended a proposal")
	}
}

const (
	runA = "run_01m6a6ft00e81840g2081040g3"
	runB = "run_01m6a6ft00e81840g2081040g4"
)

func attributed(t *testing.T) (Options, *atomic.Int32) {
	t.Helper()
	var active atomic.Int32
	return Options{Enabled: []approval.Action{approval.Merge}, Attribute: func(pid int) string {
		if pid != os.Getpid() {
			t.Errorf("attribution got PID %d, not the kernel peer's", pid)
		}
		return []string{"", runA, runB}[active.Load()]
	}}, &active
}

func TestAttributionPinsIdentityAtConnect(t *testing.T) {
	opts, active := attributed(t)
	active.Store(1)
	dir, s := fixture(t, opts, socketLimits, false)
	a := connect(t, dir)
	id := proposalID(t, a.call(merge(1)))
	p, _, err := s.journal.Get(id, time.Now())
	check(t, err)
	if p.Peer.RunID != runA {
		t.Fatalf("stored attribution %q", p.Peer.RunID)
	}
	for _, other := range []int32{2, 0} {
		active.Store(other)
		b := connect(t, dir)
		for _, op := range []string{"get", "withdraw", "wait"} {
			o := object{"op": op, "id": id}
			if op == "wait" {
				o["after"], o["timeout"] = 0, 1
			}
			want(t, b.call(o), failure("not_found"))
		}
	}
	want(t, a.call(object{"op": "get", "id": id}), status(p.Proposal))
	active.Store(1)
	if connect(t, dir).call(object{"op": "withdraw", "id": id})["status"] != "withdrawn" {
		t.Fatal("another socket from the same run could not withdraw")
	}
}

func waitRequest(id string, after, seconds int64) object {
	return object{"op": "wait", "id": id, "after": after, "timeout": seconds}
}

func waitSlots(t *testing.T, s *Server, n int) {
	t.Helper()
	limit := time.Now().Add(5 * time.Second)
	for len(s.waits) != n {
		if time.Now().After(limit) {
			t.Fatalf("found %d wait slots, want %d", len(s.waits), n)
		}
		time.Sleep(time.Millisecond)
	}
}

func TestWaitChangeTimeoutAndDisconnect(t *testing.T) {
	lim := socketLimits
	lim.waits = 1
	dir, s := fixture(t, Options{Enabled: []approval.Action{approval.Merge}}, lim, false)
	a, b := connect(t, dir), connect(t, dir)
	id := proposalID(t, a.call(merge(1)))
	current := object{"id": id, "version": int64(1), "status": "pending"}
	want(t, a.call(waitRequest(id, 0, 300)), current)
	want(t, a.call(waitRequest(id, -1, 1)), invalid("/after"))
	want(t, a.call(waitRequest(id, 1, 301)), invalid("/timeout"))
	want(t, a.call(waitRequest(id, 1, 1)), current)
	a.send(waitRequest(id, 1, 300))
	waitSlots(t, s, 1)
	want(t, b.call(waitRequest(id, 1, 300)), failure("busy"))
	want(t, b.call(waitRequest(id, 0, 1)), current) // not a blocking wait
	check(t, a.Close())
	waitSlots(t, s, 0)
	other := connect(t, dir)
	b.send(waitRequest(id, 1, 300))
	waitSlots(t, s, 1)
	b.send(object{"op": "get", "id": id}) // pipelined, must run after the wait
	withdrawn := other.call(object{"op": "withdraw", "id": id})
	if withdrawn["status"] != "withdrawn" {
		t.Fatal(withdrawn)
	}
	want(t, b.receive(), withdrawn)
	want(t, b.receive(), withdrawn)
}

func TestPendingAndHourlyLimitsUseRunOrSharedUnattributed(t *testing.T) {
	opts, active := attributed(t)
	var clock atomic.Int64
	start := time.Now()
	opts.Now = func() time.Time { return start.Add(time.Duration(clock.Load())) }
	lim := socketLimits
	lim.pendingPerPeer, lim.total, lim.perHour = 1, 2, 2
	dir, _ := fixture(t, opts, lim, false)
	x, y := connect(t, dir), connect(t, dir)
	id := proposalID(t, x.call(merge(1)))
	want(t, y.call(merge(2)), failure("busy"))
	active.Store(1)
	a := connect(t, dir)
	proposalID(t, a.call(merge(3)))
	active.Store(2)
	b := connect(t, dir)
	want(t, b.call(merge(4)), failure("busy")) // global, despite a third identity
	y.call(object{"op": "withdraw", "id": id})
	id = proposalID(t, y.call(merge(5)))
	x.call(object{"op": "withdraw", "id": id})
	want(t, x.call(merge(6)), failure("busy")) // two accepted proposals for the unattributed identity
	proposalID(t, b.call(merge(7)))
	clock.Store(int64(approval.Lifetime))
	proposalID(t, x.call(merge(8))) // both hourly and pending slots were released
}

func TestFrameSizeTimeoutAndWaitBuffer(t *testing.T) {
	lim := socketLimits
	lim.frameBytes, lim.frame, lim.idle = 4096, 150*time.Millisecond, 500*time.Millisecond
	dir, s := fixture(t, Options{Enabled: []approval.Action{approval.Merge}}, lim, false)
	large := connect(t, dir)
	_, err := large.Write(bytes.Repeat([]byte{' '}, lim.frameBytes))
	check(t, err)
	want(t, large.receive(), failure("frame_too_large"))
	large.closed()
	partial := connect(t, dir)
	_, err = partial.Write([]byte(`{"op":`))
	check(t, err)
	partial.closed()
	idle := connect(t, dir)
	idle.closed()
	exact := connect(t, dir)
	line := []byte(`{"op":"types"}`)
	line = append(append(line, bytes.Repeat([]byte{' '}, lim.frameBytes-len(line)-1)...), '\n')
	_, err = exact.Write(line)
	check(t, err)
	if exact.receive()["types"] == nil {
		t.Fatal("exactly one frame should fit")
	}
	id := proposalID(t, exact.call(merge(1)))
	exact.send(waitRequest(id, 1, 300))
	waitSlots(t, s, 1)
	flood := []byte("{\"op\":\"types\"}\n")
	_, err = exact.Write(bytes.Repeat(flood, (lim.frameBytes+frameMargin)/len(flood)+1))
	check(t, err)
	want(t, exact.receive(), failure("frame_too_large"))
	exact.closed()
	waitSlots(t, s, 0)
}

func TestConnectionLimitAndNoLiveReplacement(t *testing.T) {
	lim := socketLimits
	lim.connections = 2
	dir, s := fixture(t, Options{}, lim, false)
	for i := 0; i < 2; i++ {
		want(t, connect(t, dir).call(object{"op": "types"}), object{"types": []any{}})
	}
	connect(t, dir).closed()
	if l, err := Listen(dir, s.journal, s.opts); !errors.Is(err, ErrInUse) {
		if l != nil {
			l.Close()
		}
		t.Fatalf("second listener: %v", err)
	}
}

func TestSocketPathsRejectUnsafeFilesAndPermitStaleSocket(t *testing.T) {
	dir := testDir(t)
	run, path := paths(dir)
	check(t, os.Mkdir(run, 0o700))
	check(t, os.WriteFile(path, []byte("not a socket"), 0o600))
	if _, err := listen(dir); !errors.Is(err, approval.ErrPrivate) {
		t.Fatal(err)
	}
	b, err := os.ReadFile(path)
	check(t, err)
	if string(b) != "not a socket" {
		t.Fatal("removed a non-socket")
	}
	check(t, os.Remove(path))
	check(t, os.Symlink("missing", path))
	if _, err := listen(dir); !errors.Is(err, approval.ErrPrivate) {
		t.Fatal(err)
	}
	check(t, os.Remove(path))
	stale, err := net.ListenUnix("unix", &net.UnixAddr{Name: path, Net: "unix"})
	check(t, err)
	stale.SetUnlinkOnClose(false)
	check(t, os.Chmod(path, 0o600))
	check(t, stale.Close())
	l, err := listen(dir)
	check(t, err)
	check(t, l.Close())
	if _, err := os.Lstat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("socket not removed at shutdown", err)
	}
}

func TestClientRefusesUnsafeSocketBeforeDial(t *testing.T) {
	dir := testDir(t)
	run, path := paths(dir)
	l, err := listen(dir) // no accept loop; a successful dial would queue a connection
	check(t, err)
	t.Cleanup(func() { check(t, l.Close()) })
	for _, tc := range []struct {
		path      string
		bad, good os.FileMode
	}{{dir, 0o755, 0o700}, {run, 0o750, 0o700}, {path, 0o660, 0o600}} {
		check(t, os.Chmod(tc.path, tc.bad))
		if _, err := Call(t.Context(), dir, object{"op": "types"}, 0); !errors.Is(err, approval.ErrPrivate) {
			t.Fatalf("unsafe mode: %v", err)
		}
		check(t, os.Chmod(tc.path, tc.good))
	}
	if os.Getuid() == 0 {
		// In an isolated root test environment, also check actual foreign inode ownership.
		for _, p := range []string{run, path} {
			check(t, os.Chown(p, 65534, -1))
			if _, err := Call(t.Context(), dir, object{"op": "types"}, 0); !errors.Is(err, approval.ErrPrivate) {
				t.Fatalf("foreign owner: %v", err)
			}
			check(t, os.Chown(p, 0, -1))
		}
	}
	check(t, os.Rename(path, path+".real"))
	check(t, os.Symlink(path+".real", path))
	if _, err := Call(t.Context(), dir, object{"op": "types"}, 0); !errors.Is(err, approval.ErrPrivate) {
		t.Fatalf("socket alias: %v", err)
	}
	check(t, l.SetDeadline(time.Now().Add(50*time.Millisecond)))
	if c, err := l.AcceptUnix(); err == nil {
		c.Close()
		t.Fatal("unsafe path was dialed")
	} else if !errors.Is(err, os.ErrDeadlineExceeded) {
		t.Fatal(err)
	}
	check(t, l.Close())
	if info, err := os.Lstat(path); err != nil || info.Mode()&os.ModeSymlink == 0 {
		t.Fatal("close unlinked a replacement", err)
	}
}

func TestClientCancelsWait(t *testing.T) {
	dir, s := fixture(t, Options{Enabled: []approval.Action{approval.Merge}}, socketLimits, false)
	id := proposalID(t, connect(t, dir).call(merge(1)))
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	done := make(chan error, 1)
	go func() {
		_, err := Call(ctx, dir, waitRequest(id, 1, 300), 300*time.Second)
		done <- err
	}()
	waitSlots(t, s, 1)
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("wait did not cancel")
	}
	waitSlots(t, s, 0)
}
