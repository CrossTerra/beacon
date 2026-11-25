package vault

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/CrossTerra/beacon/internal/approval"
	"github.com/tobischo/gokeepasslib/v3"
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
	beforeReplace = func() {
		staged, err := filepath.Glob(filepath.Join(dir, "."+dbName+"-*"))
		if err != nil || len(staged) != 1 {
			t.Errorf("candidate was not staged before the final check: %v, %v", staged, err)
		} else if info, err := os.Stat(staged[0]); err != nil || info.Size() == 0 {
			t.Errorf("candidate was not written before the final check: %v", err)
		}
		body, err := os.ReadFile(filepath.Join(dir, dbName))
		if err != nil {
			t.Error(err)
			return
		}
		if err := os.WriteFile(filepath.Join(dir, dbName), append(body, 1), 0o600); err != nil {
			t.Error(err)
		}
	}
	t.Cleanup(func() { beforeReplace = nil })
	if err := Put(dir, b, "secret-two"); err != ErrConflict {
		t.Fatalf("overwrote a changed vault: %v", err)
	}
	got, err = Get(dir, b)
	if err != nil || got != "secret-one" {
		t.Fatalf("conflict wrote %q %v", got, err)
	}
}

func TestCreateDoesNotOverwriteAnExistingKeyFileOrVault(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "state")
	if err := approval.PrivateDir(dir); err != nil {
		t.Fatal(err)
	}
	keyPath := filepath.Join(dir, keyName)
	original := []byte("existing-key-not-a-vault")
	if err := os.WriteFile(keyPath, original, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := Setup(t.Context(), dir); err != ErrPartial {
		t.Fatalf("overwrote an existing key file: %v", err)
	}
	got, err := os.ReadFile(keyPath)
	if err != nil || !bytes.Equal(got, original) {
		t.Fatalf("key file changed: %q %v", got, err)
	}
	if _, err := os.Lstat(filepath.Join(dir, dbName)); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("created a database beside an existing key file")
	}

	other := filepath.Join(t.TempDir(), "state")
	if err := approval.PrivateDir(other); err != nil {
		t.Fatal(err)
	}
	dbPath := filepath.Join(other, dbName)
	dbOriginal := []byte("existing-vault")
	if err := os.WriteFile(dbPath, dbOriginal, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := Setup(t.Context(), other); err != ErrKeyMissing {
		t.Fatalf("overwrote an existing vault: %v", err)
	}
	got, err = os.ReadFile(dbPath)
	if err != nil || !bytes.Equal(got, dbOriginal) {
		t.Fatalf("vault changed: %q %v", got, err)
	}
	if _, err := os.Lstat(filepath.Join(other, keyName)); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("created a key file beside an existing vault")
	}

	fresh := filepath.Join(t.TempDir(), "state")
	if err := Create(fresh); err != nil {
		t.Fatal(err)
	}
	key, err := os.ReadFile(filepath.Join(fresh, keyName))
	if err != nil {
		t.Fatal(err)
	}
	db, err := os.ReadFile(filepath.Join(fresh, dbName))
	if err != nil {
		t.Fatal(err)
	}
	if err := Create(fresh); err != ErrExists {
		t.Fatalf("second create: %v", err)
	}
	if got, err := os.ReadFile(filepath.Join(fresh, keyName)); err != nil || !bytes.Equal(got, key) {
		t.Fatal("second create replaced the key file")
	}
	if got, err := os.ReadFile(filepath.Join(fresh, dbName)); err != nil || !bytes.Equal(got, db) {
		t.Fatal("second create replaced the vault")
	}
}

func TestSetupRefusesASecretServiceReferenceWithoutADatabase(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "state")
	if err := approval.PrivateDir(dir); err != nil {
		t.Fatal(err)
	}
	ref := filepath.Join(dir, keyRef)
	id := []byte("0123456789abcdef0123456789abcdef")
	if err := os.WriteFile(ref, id, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := Setup(t.Context(), dir); err != ErrPartial {
		t.Fatalf("a partial setup was not refused: %v", err)
	}
	if got, err := os.ReadFile(ref); err != nil || !bytes.Equal(got, id) {
		t.Fatal("setup replaced the Secret Service reference")
	}
	for _, name := range []string{dbName, keyName} {
		if _, err := os.Lstat(filepath.Join(dir, name)); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("setup must not create %s beside the existing reference", name)
		}
	}
}

func TestPasswordIsInnerProtected(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "state")
	if err := Create(dir); err != nil {
		t.Fatal(err)
	}
	b := Binding{Service: "github", Origin: "https://api.github.com", Account: "github:user:1", Use: "read"}
	if err := Put(dir, b, "secret-one"); err != nil {
		t.Fatal(err)
	}
	key, err := os.ReadFile(filepath.Join(dir, keyName))
	if err != nil {
		t.Fatal(err)
	}
	body, _, err := readDB(dir)
	if err != nil {
		t.Fatal(err)
	}
	db, err := decode(body, key)
	if err != nil {
		t.Fatal(err)
	}
	matches := find(beaconGroup(db), b)
	if len(matches) != 1 {
		t.Fatalf("entries %d", len(matches))
	}
	i := matches[0].GetPasswordIndex()
	if i < 0 || !matches[0].Values[i].Value.Protected.Bool || matches[0].Values[i].Value.Content == "secret-one" {
		t.Fatalf("password not inner-encrypted: %+v", matches[0].Values[i].Value)
	}
	if err := Put(dir, b, "secret-two"); err != nil {
		t.Fatal(err)
	}
	body, _, err = readDB(dir)
	if err != nil {
		t.Fatal(err)
	}
	db, err = decode(body, key)
	if err != nil {
		t.Fatal(err)
	}
	matches = find(beaconGroup(db), b)
	i = matches[0].GetPasswordIndex()
	if i < 0 || !matches[0].Values[i].Value.Protected.Bool || matches[0].Values[i].Value.Content == "secret-two" {
		t.Fatal("updated password was stored in the clear")
	}
}

func TestVaultUsesArgon2idAndAVersion2KeyFile(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "state")
	if err := Create(dir); err != nil {
		t.Fatal(err)
	}
	key, err := os.ReadFile(filepath.Join(dir, keyName))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(key, []byte("<Version>2.0</Version>")) || bytes.Equal(key, key[:32]) && len(key) == 32 {
		t.Fatalf("key file is not KeePassXC XML v2.0: %q", key)
	}
	body, _, err := readDB(dir)
	if err != nil {
		t.Fatal(err)
	}
	db, err := decode(body, key)
	if err != nil {
		t.Fatal(err)
	}
	h := db.Header.FileHeaders
	if !bytes.Equal(h.CipherID, gokeepasslib.CipherChaCha20) {
		t.Fatalf("cipher %x", h.CipherID)
	}
	if !bytes.Equal(h.KdfParameters.UUID, gokeepasslib.KdfArgon2id) || h.KdfParameters.Memory != kdfMemory || h.KdfParameters.Iterations != kdfIterations {
		t.Fatalf("kdf %+v", h.KdfParameters)
	}
}

func TestConflictRetriesOnceWhenTheEntryIsUnchanged(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "state")
	if err := Create(dir); err != nil {
		t.Fatal(err)
	}
	a := Binding{Service: "github", Origin: "https://api.github.com", Account: "github:user:1", Use: "read"}
	b := Binding{Service: "slack", Origin: "https://slack.com", Account: "slack:user:U1", Use: "write"}
	if err := Put(dir, a, "secret-a"); err != nil {
		t.Fatal(err)
	}
	if err := Put(dir, b, "secret-b"); err != nil {
		t.Fatal(err)
	}
	var once bool
	beforeReplace = func() {
		if once {
			return
		}
		once = true
		key, err := os.ReadFile(filepath.Join(dir, keyName))
		if err != nil {
			t.Error(err)
			return
		}
		body, _, err := readDB(dir)
		if err != nil {
			t.Error(err)
			return
		}
		db, err := decode(body, key)
		if err != nil {
			t.Error(err)
			return
		}
		if err := db.UnlockProtectedEntries(); err != nil {
			t.Error(err)
			return
		}
		matches := find(beaconGroup(db), b)
		if len(matches) != 1 {
			t.Errorf("binding b: %d", len(matches))
			return
		}
		setPassword(matches[0], "other-writer")
		if err := db.LockProtectedEntries(); err != nil {
			t.Error(err)
			return
		}
		var buf bytes.Buffer
		if err := gokeepasslib.NewEncoder(&buf).Encode(db); err != nil {
			t.Error(err)
			return
		}
		if err := os.WriteFile(filepath.Join(dir, dbName), buf.Bytes(), 0o600); err != nil {
			t.Error(err)
		}
	}
	t.Cleanup(func() { beforeReplace = nil })
	if err := Put(dir, a, "secret-a2"); err != nil {
		t.Fatal(err)
	}
	if got, err := Get(dir, a); err != nil || got != "secret-a2" {
		t.Fatalf("retried write %q %v", got, err)
	}
	if got, err := Get(dir, b); err != nil || got != "other-writer" {
		t.Fatalf("retry overwrote the other entry %q %v", got, err)
	}
}

func TestChangedEntryIsNotRetriedAndReadBackMustMatch(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "state")
	if err := Create(dir); err != nil {
		t.Fatal(err)
	}
	b := Binding{Service: "github", Origin: "https://api.github.com", Account: "github:user:1", Use: "read"}
	if err := Put(dir, b, "secret-one"); err != nil {
		t.Fatal(err)
	}
	beforeReplace = func() {
		key, err := os.ReadFile(filepath.Join(dir, keyName))
		if err != nil {
			t.Error(err)
			return
		}
		body, _, err := readDB(dir)
		if err != nil {
			t.Error(err)
			return
		}
		db, err := decode(body, key)
		if err != nil {
			t.Error(err)
			return
		}
		if err := db.UnlockProtectedEntries(); err != nil {
			t.Error(err)
			return
		}
		setPassword(find(beaconGroup(db), b)[0], "other-writer")
		if err := db.LockProtectedEntries(); err != nil {
			t.Error(err)
			return
		}
		var buf bytes.Buffer
		if err := gokeepasslib.NewEncoder(&buf).Encode(db); err != nil {
			t.Error(err)
			return
		}
		if err := os.WriteFile(filepath.Join(dir, dbName), buf.Bytes(), 0o600); err != nil {
			t.Error(err)
		}
	}
	t.Cleanup(func() { beforeReplace = nil })
	if err := Put(dir, b, "secret-two"); err != ErrConflict {
		t.Fatalf("overwrote a changed entry: %v", err)
	}
	beforeReplace = nil
	if got, err := Get(dir, b); err != nil || got != "other-writer" {
		t.Fatalf("entry %q %v", got, err)
	}
	afterRename = func(path string) {
		if err := os.WriteFile(path, []byte("not the bytes just written"), 0o600); err != nil {
			t.Error(err)
		}
	}
	t.Cleanup(func() { afterRename = nil })
	if err := Put(dir, b, "secret-three"); err != ErrConflict {
		t.Fatalf("read-back mismatch: %v", err)
	}
}
