//go:build darwin && cgo

package main

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/CrossTerra/beacon/internal/peer"
)

// fakeApp is a listener standing in for Beacon.app. It records every byte a client sends and answers each line.
type fakeApp struct {
	dir      string
	received chan []byte
}

func newFakeApp(t *testing.T, requirement string, reply string) (*fakeApp, *app, *harness) {
	t.Helper()
	dir, err := os.MkdirTemp("", "act")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	socket := filepath.Join(dir, "act.sock")
	l, err := net.Listen("unix", socket)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { l.Close() })
	f := &fakeApp{dir: dir, received: make(chan []byte, 1)}
	go func() {
		c, err := l.Accept()
		if err != nil {
			return
		}
		defer c.Close()
		r := bufio.NewReader(c)
		line, err := r.ReadBytes('\n')
		if err == nil {
			_, _ = c.Write([]byte(reply + "\n"))
		} else {
			rest, _ := io.ReadAll(r)
			line = append(line, rest...)
		}
		f.received <- line
	}()
	settings, _ := json.Marshal(actSettings{Requirement: requirement, Socket: socket})
	path := filepath.Join(dir, "act.json")
	if err := os.WriteFile(path, settings, 0o600); err != nil {
		t.Fatal(err)
	}
	h := newHarness(t)
	a := h.app(nil)
	a.actSettingsFile = path
	return f, a, h
}

func selfRequirement(t *testing.T) string {
	t.Helper()
	r, err := peer.SelfRequirement()
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func TestActProposeChecksTheListenerThenSendsOneFrame(t *testing.T) {
	f, a, h := newFakeApp(t, selfRequirement(t), `{"id":"apr_x","params_hash":"sha256:00","status":"pending","version":1}`)
	a.stdin = strings.NewReader(`{"type":"github.merge","params":{"number":140,"head":"ab"},"reason":"r","agent":{"name":"bosun"}}`)
	if code := a.main(context.Background(), []string{"act", "propose"}); code != 0 {
		t.Fatalf("exit %d: %s", code, h.stderr.String())
	}
	frame := <-f.received
	var sent map[string]any
	if err := json.Unmarshal(frame, &sent); err != nil || sent["op"] != "propose" || !strings.HasSuffix(string(frame), "}\n") {
		t.Fatalf("frame %q", frame)
	}
	if !strings.Contains(string(frame), `"params":{"number":140,"head":"ab"}`) {
		t.Fatalf("the parameters were not passed through as sent: %s", frame)
	}
	if h.stdout.String() != `{"id":"apr_x","params_hash":"sha256:00","status":"pending","version":1}`+"\n" {
		t.Fatalf("stdout %q", h.stdout.String())
	}
}

func TestActQuitSendsNothingWhenTheListenerPidDiffers(t *testing.T) {
	f, a, h := newFakeApp(t, selfRequirement(t), `{"status":"quitting"}`)
	t.Setenv("BEACON_ACT_PID", "1")
	if code := a.main(context.Background(), []string{"act", "quit"}); code == 0 {
		t.Fatalf("exit %d: %s", code, h.stderr.String())
	}
	if !strings.Contains(h.stderr.String(), "not 1") {
		t.Fatalf("stderr %s", h.stderr.String())
	}
	if got := <-f.received; len(got) != 0 {
		t.Fatalf("the unnamed listener received %q", got)
	}
}

func TestActSendsNothingToAnotherListener(t *testing.T) {
	f, a, h := newFakeApp(t, `identifier "dev.beacon.app" and anchor apple generic`, `{"status":"pending"}`)
	if code := a.main(context.Background(), []string{"act", "get", "apr_x"}); code == 0 || !strings.Contains(h.stderr.String(), "not the configured Beacon app") {
		t.Fatalf("exit %d: %s", code, h.stderr.String())
	}
	if got := <-f.received; len(got) != 0 {
		t.Fatalf("the wrong listener received %d bytes", len(got))
	}
}

func TestActWaitAndErrorReplies(t *testing.T) {
	f, a, h := newFakeApp(t, selfRequirement(t), `{"error":"not_found"}`)
	if code := a.main(context.Background(), []string{"act", "wait", "apr_x", "--after", "3", "--timeout", "5"}); code != 1 {
		t.Fatalf("exit %d: %s", code, h.stderr.String())
	}
	var sent map[string]any
	if err := json.Unmarshal(<-f.received, &sent); err != nil || sent["op"] != "wait" || sent["after"] != float64(3) || sent["timeout"] != float64(5) {
		t.Fatalf("sent %v", sent)
	}
	if h.stdout.String() != `{"error":"not_found"}`+"\n" {
		t.Fatalf("stdout %q", h.stdout.String())
	}
}

func TestActRefusesBadInputWithoutConnecting(t *testing.T) {
	f, a, h := newFakeApp(t, selfRequirement(t), `{}`)
	for _, input := range []string{`{"op":"withdraw","id":"apr_x"}`, `[1]`, `{"a":1} {"b":2}`, strings.Repeat(" ", actFrameMax)} {
		a.stdin = strings.NewReader(input)
		if code := a.main(context.Background(), []string{"act", "propose"}); code == 0 {
			t.Fatalf("accepted %.40q", input)
		}
	}
	for _, args := range [][]string{{"act"}, {"act", "approve", "apr_x"}, {"act", "list"}, {"act", "get"}, {"act", "wait", "apr_x", "extra"}} {
		if code := a.main(context.Background(), args); code != 2 {
			t.Fatalf("%v: exit %d", args, code)
		}
	}
	select {
	case got := <-f.received:
		t.Fatalf("a refused request reached the socket: %q", got)
	default:
	}
	a.actSettingsFile = filepath.Join(f.dir, "missing.json")
	if code := a.main(context.Background(), []string{"act", "types"}); code == 0 || !strings.Contains(h.stderr.String(), "code requirement") {
		t.Fatalf("exit %d: %s", code, h.stderr.String())
	}
}

func TestActQuitSendsTheOneOp(t *testing.T) {
	f, a, h := newFakeApp(t, selfRequirement(t), `{"status":"quitting"}`)
	if code := a.main(context.Background(), []string{"act", "quit"}); code != 0 {
		t.Fatalf("exit %d: %s", code, h.stderr.String())
	}
	if got := strings.TrimSpace(string(<-f.received)); got != `{"op":"quit"}` {
		t.Fatalf("sent %s", got)
	}
	if strings.TrimSpace(h.stdout.String()) != `{"status":"quitting"}` {
		t.Fatalf("stdout %q", h.stdout.String())
	}
	if code := a.main(context.Background(), []string{"act", "quit", "now"}); code != 2 {
		t.Fatalf("an argument was accepted: %d", code)
	}
}
