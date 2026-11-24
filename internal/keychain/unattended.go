package keychain

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"runtime"
	"strings"

	"github.com/CrossTerra/beacon/internal/vault"
)

// Unattended is the collector's store. Bindings come from the person's config, not a request.
type Unattended struct {
	Dir      string
	Bindings map[string]vault.Binding
}

func (s Unattended) Get(account string) (Secret, error) {
	b, ok := s.Bindings[account]
	if !ok {
		return Secret{}, ErrNotFound
	}
	return ReadBound(s.Dir, b)
}

func (s Unattended) Set(account string, value Secret) error {
	b, ok := s.Bindings[account]
	if !ok {
		return ErrNotFound
	}
	return StoreBound(s.Dir, b, value)
}

func (s Unattended) Delete(string) error { return errors.New("remove the entry in KeePassXC") }

func ReadBound(dir string, b vault.Binding) (Secret, error) {
	s, err := vault.Get(dir, b)
	if errors.Is(err, os.ErrNotExist) {
		return Secret{}, ErrNotFound
	}
	if err != nil {
		return Secret{}, err
	}
	return FromString(s)
}

func StoreBound(dir string, b vault.Binding, secret Secret) error {
	if secret.Empty() {
		return ErrNotFound
	}
	if err := vault.Create(dir); err != nil && !errors.Is(err, vault.ErrExists) {
		return err
	}
	return vault.Put(dir, b, string(secret.value))
}

func ImportBound(dir string, b vault.Binding, secret Secret) error {
	if secret.Empty() {
		return ErrNotFound
	}
	return vault.PutMissing(dir, b, string(secret.value))
}

func InUnattended(dir string, secret Secret) (bool, error) { return vault.Contains(dir, secret.value) }

// LegacyRead is only used by an explicit terminal migration, never by any runtime store.
// The platform tool writes into a private pipe, not the terminal or a file. Its stderr is discarded, even
// on refusal. No item is updated or deleted. Its service and account are labels, never a secret.
func LegacyRead(ctx context.Context, account string) (Secret, error) {
	if runtime.GOOS != "darwin" {
		return Secret{}, errors.New("importing from Keychain is only available on macOS")
	}
	if account == "" || strings.ContainsRune(account, 0) {
		return Secret{}, ErrNotFound
	}
	b, err := exec.CommandContext(ctx, "/usr/bin/security", "find-generic-password", "-w", "-s", Service, "-a", account).Output()
	defer clear(b)
	if err != nil {
		return Secret{}, errors.New("could not read the Keychain item; unlock it or re-enter the credential")
	}
	if len(b) > 64<<10 {
		return Secret{}, errors.New("the Keychain item is too large")
	}
	return FromString(strings.TrimSuffix(string(b), "\n"))
}
