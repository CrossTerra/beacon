//go:build darwin && cgo

package peer

import (
	"errors"
	"net"
	"os"
	"path/filepath"
	"testing"
)

// The test connects to its own listener, so the kernel audit token check runs against this process.
func pair(t *testing.T) *net.UnixConn {
	t.Helper()
	dir, err := os.MkdirTemp("", "peer")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	l, err := net.Listen("unix", filepath.Join(dir, "s"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { l.Close() })
	go func() {
		c, err := l.Accept()
		if err == nil {
			t.Cleanup(func() { c.Close() })
		}
	}()
	c, err := net.Dial("unix", filepath.Join(dir, "s"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { c.Close() })
	return c.(*net.UnixConn)
}

func TestCheck(t *testing.T) {
	self, err := SelfRequirement()
	if err != nil {
		t.Fatal(err)
	}
	if err := Check(pair(t), self); err != nil {
		t.Fatalf("own requirement refused: %v", err)
	}
	if err := Check(pair(t), `identifier "dev.beacon.app" and anchor apple generic`); !errors.Is(err, ErrRefused) {
		t.Fatalf("a different identity: %v", err)
	}
	if err := Check(pair(t), "not a requirement ("); err == nil || errors.Is(err, ErrRefused) {
		t.Fatalf("an unparsable requirement: %v", err)
	}
	if err := Check(pair(t), ""); err == nil {
		t.Fatal("an empty requirement passed")
	}
}
