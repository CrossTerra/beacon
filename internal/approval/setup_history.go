package approval

import (
	"bufio"
	"bytes"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"io"
	"os"
	"path/filepath"
)

var errSetupHistory = errors.New("offline Signal setup requires a complete setup-only journal (header and at most one passkey enrollment)")

type SetupHistory struct {
	ID        string
	Digest    [32]byte
	VaultHash []byte // nil if no enrollment was committed
}

// CheckJournalID checks the existing header without replaying or opening for write. A paired
// connection may follow later records in its own journal, but never a different journal.
func CheckJournalID(dir, id string) error {
	f, err := OpenPrivate(filepath.Join(dir, journalName), os.O_RDONLY)
	if err != nil {
		return ErrCorrupt
	}
	defer f.Close()
	b, err := readFrameBody(bufio.NewReader(io.LimitReader(f, 4096)))
	h, parseErr := ParseObject(b)
	if err != nil || parseErr != nil || len(h) != 2 || h["kind"] != "journal" || h["id"] != id || !ValidID(id, "jrn") {
		return ErrCorrupt
	}
	return nil
}

// ReadSetupHistory is not journal replay. It accepts only the minimum histories created by
// localhost setup and first enrollment, and never truncates even an incomplete final frame.
func ReadSetupHistory(lock *OfflineLock) (SetupHistory, error) {
	if err := lock.Check(); err != nil {
		return SetupHistory{}, err
	}
	f, err := OpenPrivate(filepath.Join(lock.Dir(), journalName), os.O_RDONLY)
	if err != nil {
		return SetupHistory{}, errSetupHistory
	}
	defer f.Close()
	const limit = 4096
	body, err := io.ReadAll(io.LimitReader(f, limit+1))
	if err != nil || len(body) > limit {
		return SetupHistory{}, errSetupHistory
	}
	r := bufio.NewReader(bytes.NewReader(body))
	header, err := readFrameBody(r)
	h, parseErr := ParseObject(header)
	id, _ := h["id"].(string)
	if err != nil || parseErr != nil || len(h) != 2 || h["kind"] != "journal" || !ValidID(id, "jrn") {
		return SetupHistory{}, errSetupHistory
	}
	s := SetupHistory{ID: id, Digest: sha256.Sum256(body)}
	if _, err := r.Peek(1); err == io.EOF {
		return s, nil
	}
	added, err := readFrameBody(r)
	a, parseErr := ParseObject(added)
	at, _ := a["at"].(int64)
	hash, _ := a["vault_hash"].(string)
	s.VaultHash, _ = base64.StdEncoding.Strict().DecodeString(hash)
	if err != nil || parseErr != nil || len(a) != 3 || a["kind"] != "passkey_added" || at <= 0 ||
		len(s.VaultHash) != sha256.Size || base64.StdEncoding.EncodeToString(s.VaultHash) != hash {
		return SetupHistory{}, errSetupHistory
	}
	if _, err := r.Peek(1); err != io.EOF {
		return SetupHistory{}, errSetupHistory
	}
	return s, nil
}
