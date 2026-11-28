package passkey

import (
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"

	"github.com/CrossTerra/beacon/internal/approval"
	"github.com/CrossTerra/beacon/internal/webauthn"
)

var errSetupState = errors.New("offline Signal setup requires intact localhost passkey state or no enrollment")

// CheckSetupState validates the enrollment shape and journaled write-vault hash without
// unwrapping the data key, consuming a setup code, or invoking passkey recovery.
func CheckSetupState(dir string, vaultHash []byte) ([32]byte, error) {
	for _, name := range []string{resetMarker, "register-pending.json", "setup-code.claimed"} {
		if _, err := os.Lstat(filepath.Join(dir, name)); !errors.Is(err, os.ErrNotExist) {
			return [32]byte{}, errSetupState
		}
	}
	f, err := approval.OpenPrivate(filepath.Join(dir, "passkeys.json"), os.O_RDONLY)
	v, vaultErr := readVault(dir)
	if f != nil {
		defer f.Close()
	}
	if len(vaultHash) == 0 && errors.Is(err, os.ErrNotExist) && errors.Is(vaultErr, os.ErrNotExist) {
		return [32]byte{}, nil
	}
	if err != nil || vaultErr != nil || len(vaultHash) != sha256.Size {
		return [32]byte{}, errSetupState
	}
	sum := sha256.Sum256(v)
	var check vaultFile
	if !bytes.Equal(sum[:], vaultHash) || json.Unmarshal(v, &check) != nil || check.Origin != approval.LocalOrigin ||
		len(check.Nonce) != nonceBytes || len(check.Check) != len(vaultPlain)+16 {
		return [32]byte{}, errSetupState
	}
	const limit = 16 << 10
	b, err := io.ReadAll(io.LimitReader(f, limit+1))
	if err != nil || len(b) > limit {
		return [32]byte{}, errSetupState
	}
	if _, err := approval.ParseObject(b); err != nil {
		return [32]byte{}, errSetupState
	}
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.DisallowUnknownFields()
	var s storeFile
	if dec.Decode(&s) != nil || s.Origin != approval.LocalOrigin || s.RPID != "localhost" || len(s.Credentials) != 1 {
		return [32]byte{}, errSetupState
	}
	c := s.Credentials[0]
	k, err := webauthn.ParseCOSEKey(c.COSE)
	if err != nil || int64(k.Algorithm) != c.Alg || len(c.ID) == 0 || len(c.Salt) != prfBytes ||
		len(c.Wrap.Nonce) != nonceBytes || len(c.Wrap.Key) != keyBytes+16 || c.Added.IsZero() {
		return [32]byte{}, errSetupState
	}
	return sha256.Sum256(b), nil // the journal digest already commits the sealed vault hash
}
