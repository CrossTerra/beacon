package seed

import (
	"os"
	"path/filepath"
	"testing"
)

func TestParse(t *testing.T) {
	f, err := Parse([]byte("# dismissed\ngithub:pr:acme/app#1 never\n\njira:ENG-1 send\n"))
	if err != nil {
		t.Fatal(err)
	}
	if !f.Drops("github:pr:acme/app#1") || f.Drops("jira:ENG-1") {
		t.Fatal("drops")
	}
	n, s := f.Counts()
	if n != 1 || s != 1 {
		t.Fatalf("counts %d %d", n, s)
	}
	for _, bad := range []string{"github:pr:acme/app#1 maybe\n", "only-one\n", "jira:ENG-1 never\njira:ENG-1 send\n"} {
		if _, err := Parse([]byte(bad)); err == nil {
			t.Fatalf("parsed %q", bad)
		}
	}
}

func TestWriteRoundTrip(t *testing.T) {
	dir := t.TempDir()
	data := []byte("github:pr:acme/app#1 never\n")
	if _, err := Write(dir, data); err != nil {
		t.Fatal(err)
	}
	f, err := ReadDir(dir)
	if err != nil || !f.Drops("github:pr:acme/app#1") {
		t.Fatal(err)
	}
	info, err := os.Stat(filepath.Join(dir, FileName))
	if err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("mode %v %v", info, err)
	}
	empty, err := ReadDir(t.TempDir())
	if err != nil || empty.Drops("github:pr:acme/app#1") {
		t.Fatal(err)
	}
}
