package passkey

import (
	"encoding/json"
	"os"
	"path/filepath"

	"github.com/CrossTerra/beacon/internal/webauthn"
)

// Stored is a registered passkey without its wrapped key.
type Stored struct {
	ID        []byte
	COSE      []byte
	SignCount uint32
	Salt      []byte
}

// List returns the passkeys for this directory. The wrapped data key is not included.
func List(dir string) ([]Stored, error) {
	body, err := os.ReadFile(filepath.Join(dir, "passkeys.json"))
	if err != nil {
		return nil, err
	}
	var stored storeFile
	if err := json.Unmarshal(body, &stored); err != nil {
		return nil, err
	}
	out := make([]Stored, len(stored.Credentials))
	for i, c := range stored.Credentials {
		out[i] = Stored{ID: append([]byte(nil), c.ID...), COSE: append([]byte(nil), c.COSE...), SignCount: c.SignCount, Salt: append([]byte(nil), c.Salt...)}
	}
	return out, nil
}

// Credential is the verifier's view of one stored passkey.
func (s Stored) Credential() webauthn.Credential {
	return webauthn.Credential{ID: s.ID, COSEKey: s.COSE, SignCount: s.SignCount}
}
