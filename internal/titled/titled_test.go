package titled

import (
	"os"
	"path/filepath"
	"testing"
)

func TestStore(t *testing.T) {
	dir := t.TempDir()
	s, err := Read(dir)
	if err != nil || s.Checked("slack:C1:1.000001") {
		t.Fatalf("%v", err)
	}
	if err := s.Mark("slack:C1:1.000001", Renamed); err != nil {
		t.Fatal(err)
	}
	if err := s.Mark("slack:C1:1.000001", "a title"); err == nil {
		t.Fatal("recorded free text")
	}
	again, err := Read(dir)
	if err != nil || !again.Checked("slack:C1:1.000001") {
		t.Fatalf("%v", err)
	}
	info, err := os.Stat(filepath.Join(dir, FileName))
	if err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("%v %v", err, info.Mode())
	}
	if err := os.WriteFile(filepath.Join(dir, FileName), []byte(`{"checked":{"slack:C1:1.000001":"DM with Dana"}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Read(dir); err == nil {
		t.Fatal("read a record it would not have written")
	}
}
