package approval

import (
	"bytes"
	"crypto/sha256"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestEnrollmentDoesNotHashAReopenedPath(t *testing.T) {
	dir := scratch(t)
	j, err := OpenJournal(dir, t0, "")
	must(t, err)
	defer j.Close()
	sealed := []byte("sealed fixture")
	path := filepath.Join(dir, "write-vault.json")
	must(t, EnrollPasskey(dir, 0, t0, sealed, func(body []byte) error {
		if !bytes.Equal(body, sealed) {
			t.Fatal("publication did not get the sealed bytes")
		}
		return os.WriteFile(path, []byte("replacement fixture"), 0o600)
	}, func() error { return os.Remove(path) }))
	want := sha256.Sum256(sealed)
	hash, active, err := PasskeyVault(dir)
	must(t, err)
	if !active || !bytes.Equal(hash, want[:]) {
		t.Fatal("enrollment did not fix the hash of the sealed bytes")
	}
}

func TestEnrollmentUnwindsFailedJournalSync(t *testing.T) {
	dir := scratch(t)
	j, err := OpenJournal(dir, t0, "")
	must(t, err)
	defer j.Close()
	j.hooks.sync = func(*os.File) error { return errors.New("fixture sync failure") }
	path := filepath.Join(dir, "write-vault.json")
	err = EnrollPasskey(dir, 0, t0, []byte("sealed fixture"), func(body []byte) error {
		return os.WriteFile(path, body, 0o600)
	}, func() error { return os.Remove(path) })
	if !errors.Is(err, ErrJournalIO) {
		t.Fatalf("commit = %v", err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatal("failed enrollment left a published file")
	}
}
