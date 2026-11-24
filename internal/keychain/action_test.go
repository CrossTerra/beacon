package keychain

import (
	"bytes"
	"net/url"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/CrossTerra/beacon/internal/approval"
	"github.com/CrossTerra/beacon/internal/vault"
)

func TestWriteCredentialRequiresTheKeyAndExactBinding(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "state")
	j, err := approval.OpenJournal(dir, time.Now(), "")
	if err != nil {
		t.Fatal(err)
	}
	defer j.Close()
	_, write, err := ActionTokens(url.Values{"read_token": {"fixture-read"}, "write_token": {"fixture-write"}})
	if err != nil {
		t.Fatal(err)
	}
	defer write.Clear()
	b := vault.Binding{Service: "github", Origin: "https://github.fixture.test", Account: "github:user:123"}
	key := bytes.Repeat([]byte{10}, 32)
	if err := StoreActionWrite(dir, b, write, key); err != nil {
		t.Fatal(err)
	}
	stored, err := UnsealActionWrite(dir, b, key)
	defer stored.Clear()
	if err != nil || !stored.Same(write) {
		t.Fatal("the bound write credential did not open")
	}
	// The same ciphertext in a different provider slot must still be bound to the original provider.
	ciphertext, err := os.ReadFile(filepath.Join(dir, "write-github.sealed"))
	if err != nil {
		t.Fatal(err)
	}
	if err := vault.StoreSealed(dir, "slack", ciphertext); err != nil {
		t.Fatal(err)
	}
	for _, different := range []vault.Binding{
		{Service: "slack", Origin: b.Origin, Account: b.Account},
		{Service: b.Service, Origin: "https://another.fixture.test", Account: b.Account},
		{Service: b.Service, Origin: b.Origin, Account: "github:user:234"},
		{Service: b.Service, Origin: b.Origin, Account: b.Account, Workspace: "T1"},
	} {
		if s, err := UnsealActionWrite(dir, different, key); err == nil || !s.Empty() {
			t.Fatal("credential opened for a different binding")
		}
	}
	if s, err := UnsealActionWrite(dir, b, make([]byte, 32)); err == nil || !s.Empty() {
		t.Fatal("credential opened with a replacement data key")
	}
}
