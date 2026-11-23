package vault

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/CrossTerra/beacon/internal/approval"
)

func TestKeyFileIsPrivateAndAChangedFileIsNotOverwritten(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "state")
	if err := Create(dir); err != nil {
		t.Fatal(err)
	}
	if _, err := approval.CheckPrivate(filepath.Join(dir, keyName), 0o600); err != nil {
		t.Fatal(err)
	}
	b := Binding{Service: "github", Origin: "https://api.github.com", Account: "github:user:1", Use: "read"}
	if err := Put(dir, b, "secret-one"); err != nil {
		t.Fatal(err)
	}
	got, err := Get(dir, b)
	if err != nil || got != "secret-one" {
		t.Fatalf("get %q %v", got, err)
	}
	beforeSave = func() {
		body, err := os.ReadFile(filepath.Join(dir, dbName))
		if err != nil {
			t.Error(err)
			return
		}
		if err := os.WriteFile(filepath.Join(dir, dbName), append(body, 1), 0o600); err != nil {
			t.Error(err)
		}
	}
	t.Cleanup(func() { beforeSave = nil })
	if err := Put(dir, b, "secret-two"); err != ErrConflict {
		t.Fatalf("overwrote a changed vault: %v", err)
	}
	got, err = Get(dir, b)
	if err != nil || got != "secret-one" {
		t.Fatalf("conflict wrote %q %v", got, err)
	}
}
