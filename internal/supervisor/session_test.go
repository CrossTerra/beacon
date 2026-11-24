package supervisor

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func testLock(t *testing.T, dir string) *os.File {
	t.Helper()
	f, err := os.OpenFile(filepath.Join(dir, "instance.lock"), os.O_RDWR|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { f.Close() })
	return f
}

func TestSessionBoundToDirectoryAndLock(t *testing.T) {
	dir := t.TempDir()
	lock := testLock(t, dir)
	prefix, err := newSession(dir, lock)
	if err != nil || prefix == "" {
		t.Fatalf("new session: %q %v", prefix, err)
	}
	got, err := oldSession(dir, lock)
	if err != nil || got != prefix {
		t.Fatalf("bound session: %q %v", got, err)
	}

	other := t.TempDir()
	otherLock := testLock(t, other)
	b, err := os.ReadFile(filepath.Join(dir, sessionFile))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(other, sessionFile), b, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := oldSession(other, otherLock); !errors.Is(err, ErrUnconfirmed) {
		t.Fatalf("copied session was sweepable: %v", err)
	}
	if again, err := oldSession(dir, lock); err != nil || again != prefix {
		t.Fatalf("original session changed after the copy was refused: %q %v", again, err)
	}
}

func TestSessionRejectsUnboundFile(t *testing.T) {
	dir := t.TempDir()
	lock := testLock(t, dir)
	for _, body := range []string{"0123456789abcdef0123456789abcdef", `{"token":"0123456789abcdef0123456789abcdef"}`, "not-a-session"} {
		if err := os.WriteFile(filepath.Join(dir, sessionFile), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err := oldSession(dir, lock); !errors.Is(err, ErrUnconfirmed) {
			t.Fatalf("unbound session %q was accepted: %v", body, err)
		}
	}
}
