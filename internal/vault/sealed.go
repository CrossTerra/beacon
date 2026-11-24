package vault

import (
	"errors"
	"io"
	"os"
	"path/filepath"

	"github.com/CrossTerra/beacon/internal/approval"
)

const maxSealed = 4096

func sealedName(service string) string {
	switch service {
	case "github", "slack":
		return "write-" + service + ".sealed"
	}
	return ""
}

func RemoveWriteCredentials(dir string) error {
	for _, service := range []string{"github", "slack"} {
		if err := os.Remove(filepath.Join(dir, sealedName(service))); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
	}
	return approval.FlushDirectory(dir)
}

// StoreSealed writes only an authenticated ciphertext. The write-vault key-check file is not modified.
func StoreSealed(dir, service string, ciphertext []byte) error {
	name := sealedName(service)
	if name == "" || len(ciphertext) == 0 || len(ciphertext) > maxSealed || !approval.HoldsInstance(dir) {
		return ErrUnreadable
	}
	return writePrivate(dir, name, ciphertext)
}

func LoadSealed(dir, service string) ([]byte, error) {
	name := sealedName(service)
	if name == "" {
		return nil, ErrUnreadable
	}
	f, err := approval.OpenPrivate(filepath.Join(dir, name), os.O_RDONLY)
	if err != nil {
		return nil, ErrUnreadable
	}
	defer f.Close()
	b, err := io.ReadAll(io.LimitReader(f, maxSealed+1))
	if err != nil || len(b) > maxSealed {
		return nil, ErrUnreadable
	}
	return b, nil
}
