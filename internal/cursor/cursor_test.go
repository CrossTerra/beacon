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
	if err := s.Accept([]string{"github"}); err != nil {
		t.Fatal(err)
	}
	again, err := Read(dir)
	if err != nil {
		t.Fatal(err)
	}
	got, ok := again.Get("github")
	if !ok || string(got) != "2026-11-07T12:00:00Z" || !again.Accepted("github") || again.Accepted("jira") {
		t.Fatalf("cursor %q ok %v accepted %v", got, ok, again.Accepted("github"))
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
