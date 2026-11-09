package cursor

import (
	"os"
	"path/filepath"
	"testing"
)

func TestCommitAndAccept(t *testing.T) {
	dir := t.TempDir()
	ro, err := Read(dir)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := ro.Get("github"); ok || ro.Accepted("github") {
		t.Fatal("empty store has a cursor")
	}
	if err := ro.Commit("github", []byte("t")); err == nil {
		t.Fatal("read-only store committed")
	}
	entries, _ := os.ReadDir(dir)
	if len(entries) != 0 {
		t.Fatal("read created a file")
	}

	s, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Commit("github", []byte("2026-11-07T12:00:00Z")); err != nil {
		t.Fatal(err)
	}
	if err := s.MarkFirst([]string{"github"}); err != nil {
		t.Fatal(err)
	}
	if err := s.Accept([]string{"github"}); err != nil {
		t.Fatal(err)
	}
	if err := s.Accept([]string{"jira"}); err == nil {
		t.Fatal("accepted without a completed --first")
	}
	again, err := Read(dir)
	if err != nil {
		t.Fatal(err)
	}
	got, ok := again.Get("github")
	if !ok || string(got) != "2026-11-07T12:00:00Z" || !again.Accepted("github") || again.Accepted("jira") {
		t.Fatalf("cursor %q ok %v accepted %v", got, ok, again.Accepted("github"))
	}
	if cleared, err := again.Reset("github"); err == nil || cleared {
		t.Fatal("read-only store reset a cursor")
	}
	s, err = Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	cleared, err := s.Reset("github")
	if err != nil || !cleared {
		t.Fatalf("reset: %v cleared %v", err, cleared)
	}
	if _, ok := s.Get("github"); ok || !s.Accepted("github") || !s.FirstDone("github") {
		t.Fatal("reset cleared acceptance or left the cursor")
	}
	if cleared, err := s.Reset("github"); err != nil || cleared {
		t.Fatalf("second reset: %v cleared %v", err, cleared)
	}
	reloaded, err := Read(dir)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := reloaded.Get("github"); ok || !reloaded.Accepted("github") {
		t.Fatal("reset did not stick, or dropped acceptance")
	}

	for _, name := range []string{cursorsFile, acceptedFile} {
		info, err := os.Stat(filepath.Join(dir, name))
		if err != nil {
			t.Fatal(err)
		}
		if info.Mode().Perm() != 0o600 {
			t.Fatalf("%s mode %o", name, info.Mode().Perm())
		}
	}
}
