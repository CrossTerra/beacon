package supervisor

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"

	"github.com/CrossTerra/beacon/internal/approval"
	"golang.org/x/sys/unix"
)

const sessionFile = "supervisor.session"

// A copied state directory must not sweep the original instance. The token is useless without the
// device and inode of the directory and lock that published it.
type sessionRecord struct {
	Token   string `json:"token"`
	LockDev uint64 `json:"lock_dev"`
	LockIno uint64 `json:"lock_ino"`
	DirDev  uint64 `json:"dir_dev"`
	DirIno  uint64 `json:"dir_ino"`
}

func oldSession(dir string, lock *os.File) (string, error) {
	f, err := approval.OpenPrivate(filepath.Join(dir, sessionFile), os.O_RDONLY)
	if errors.Is(err, os.ErrNotExist) {
		return "", nil
	}
	if err != nil {
		return "", ErrUnconfirmed
	}
	defer f.Close()
	b, err := io.ReadAll(io.LimitReader(f, 513))
	if err != nil || len(b) == 0 || len(b) > 512 {
		return "", ErrUnconfirmed
	}
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.DisallowUnknownFields()
	var rec sessionRecord
	if dec.Decode(&rec) != nil || dec.More() || !sessionToken(rec.Token) || rec.matches(dir, lock) != nil {
		return "", ErrUnconfirmed
	}
	return sessionPrefix(rec.Token), nil
}

func sessionToken(token string) bool {
	var raw [16]byte
	n, err := hex.Decode(raw[:], []byte(token))
	return n == len(raw) && err == nil && hex.EncodeToString(raw[:]) == token
}

func sessionPrefix(token string) string { return markerKey + "=" + token + ":" }

func newSession(dir string, lock *os.File) (string, error) {
	var raw [16]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return "", ErrSession
	}
	rec := sessionRecord{Token: hex.EncodeToString(raw[:])}
	if err := rec.capture(dir, lock); err != nil {
		return "", ErrSession
	}
	b, err := json.Marshal(rec)
	if err != nil {
		return "", ErrSession
	}
	f, err := os.CreateTemp(dir, ".supervisor-session-")
	if err != nil {
		return "", ErrSession
	}
	defer os.Remove(f.Name())
	_, writeErr := f.Write(b)
	err = errors.Join(writeErr, f.Sync(), f.Close())
	if err != nil || os.Rename(f.Name(), filepath.Join(dir, sessionFile)) != nil || flushDir(dir) != nil {
		return "", ErrSession
	}
	return sessionPrefix(rec.Token), nil
}

func (r *sessionRecord) capture(dir string, lock *os.File) error {
	lockID, err := fileID(lock)
	if err != nil {
		return err
	}
	d, err := os.Open(dir)
	if err != nil {
		return err
	}
	defer d.Close()
	dirID, err := fileID(d)
	if err != nil {
		return err
	}
	r.LockDev, r.LockIno, r.DirDev, r.DirIno = lockID.dev, lockID.ino, dirID.dev, dirID.ino
	return nil
}

func (r sessionRecord) matches(dir string, lock *os.File) error {
	var want sessionRecord
	if err := want.capture(dir, lock); err != nil {
		return err
	}
	if r.LockDev != want.LockDev || r.LockIno != want.LockIno || r.DirDev != want.DirDev || r.DirIno != want.DirIno {
		return ErrUnconfirmed
	}
	return nil
}

type devInode struct{ dev, ino uint64 }

func fileID(f *os.File) (devInode, error) {
	var st unix.Stat_t
	if f == nil || unix.Fstat(int(f.Fd()), &st) != nil {
		return devInode{}, ErrUnconfirmed
	}
	return devInode{deviceBits(st), st.Ino}, nil
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
