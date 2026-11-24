package supervisor

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"io"
	"os"
	"path/filepath"

	"github.com/CrossTerra/beacon/internal/approval"
	"golang.org/x/sys/unix"
)

const sessionFile = "supervisor.session"

func oldSession(dir string) (string, error) {
	f, err := approval.OpenPrivate(filepath.Join(dir, sessionFile), os.O_RDONLY)
	if errors.Is(err, os.ErrNotExist) {
		return "", nil
	}
	if err != nil {
		return "", ErrUnconfirmed
	}
	defer f.Close()
	var token [32]byte
	var raw [16]byte
	if _, err := io.ReadFull(f, token[:]); err != nil {
		return "", ErrUnconfirmed
	}
	if n, err := hex.Decode(raw[:], token[:]); n != len(raw) || err != nil || hex.EncodeToString(raw[:]) != string(token[:]) {
		return "", ErrUnconfirmed
	}
	if n, err := f.Read(make([]byte, 1)); n != 0 || err != io.EOF {
		return "", ErrUnconfirmed
	}
	return sessionPrefix(string(token[:])), nil
}

func sessionPrefix(token string) string { return markerKey + "=" + token + ":" }

func newSession(dir string) (string, error) {
	var raw [16]byte
	rand.Read(raw[:])
	token := hex.EncodeToString(raw[:])
	f, err := os.CreateTemp(dir, ".supervisor-session-")
	if err != nil {
		return "", ErrSession
	}
	defer os.Remove(f.Name())
	_, writeErr := f.WriteString(token)
	err = errors.Join(writeErr, f.Sync(), f.Close())
	if err != nil || os.Rename(f.Name(), filepath.Join(dir, sessionFile)) != nil || flushDir(dir) != nil {
		return "", ErrSession
	}
	return sessionPrefix(token), nil
}

func clearSession(dir string) error {
	if err := os.Remove(filepath.Join(dir, sessionFile)); err != nil {
		return ErrSession
	}
	if err := flushDir(dir); err != nil {
		return ErrSession
	}
	return nil
}

func flushDir(dir string) error {
	d, err := os.Open(dir)
	if err != nil {
		return err
	}
	defer d.Close()
	// File.Sync on Darwin requests F_FULLFSYNC, which isn't supported on all directories.
	return unix.Fsync(int(d.Fd()))
}
