// Package passkey stores the preview's passkey list and the write vault's wrapped data key.
// It does not approve an action or read a provider token.
package passkey

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/hkdf"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"time"

	"github.com/CrossTerra/beacon/internal/approval"
	"github.com/CrossTerra/beacon/internal/webauthn"
	"golang.org/x/sys/unix"
)

const (
	codeLife    = 10 * time.Minute
	pendingLife = 2 * time.Minute
	tokenBytes  = 32
	prfBytes    = 32
	keyBytes    = 32
	nonceBytes  = 12
	hkdfInfo    = "beacon-write-vault-v1"
)

var (
	ErrSetup      = errors.New("setup code is missing, expired, or already used")
	ErrPending    = errors.New("passkey registration is not waiting")
	ErrRegistered = errors.New("a passkey is already registered")
	ErrPRF        = errors.New("passkey registration needs PRF")
)

// Ceremony is one browser result. PRF output is not part of the signed assertion.
type Ceremony struct {
	ClientDataJSON    []byte
	AttestationObject []byte
	AuthenticatorData []byte
	Signature         []byte
	CredentialID      []byte
	PRFEnabled        bool
	PRFOutput         []byte
}

// Issue writes a one-time setup code and returns it. The code is not stored.
// It holds the instance lock, so it refuses while beacon serve is running.
func Issue(dir string, now time.Time) (string, error) {
	if err := approval.PrivateDir(dir); err != nil {
		return "", err
	}
	lock, err := approval.LockInstance(dir)
	if err != nil {
		return "", err
	}
	defer lock.Close()
	raw := make([]byte, tokenBytes)
	if _, err := rand.Read(raw); err != nil {
		return "", err
	}
	sum := sha256.Sum256(raw)
	body, err := json.Marshal(codeFile{Hash: sum[:], Expires: now.Add(codeLife).UTC()})
	if err != nil {
		return "", err
	}
	if err := writeNew(dir, "setup-code.json", body); err != nil {
		return "", err
	}
	_ = os.Remove(filepath.Join(dir, "register-pending.json"))
	return base64.RawURLEncoding.EncodeToString(raw), nil
}

// Begin consumes a live setup code and returns the create challenge, the assertion challenge,
// and the PRF salt the assertion must evaluate. The salt is an input to the authenticator.
func Begin(dir string, rp webauthn.RelyingParty, code string, now time.Time) (create, assert, salt []byte, err error) {
	if rp.ID == "" || rp.Origin == "" {
		return nil, nil, nil, errors.New("no canonical review origin")
	}
	raw, err := base64.RawURLEncoding.Strict().DecodeString(code)
	if err != nil || len(raw) != tokenBytes {
		return nil, nil, nil, ErrSetup
	}
	if _, err := os.Lstat(filepath.Join(dir, "passkeys.json")); err == nil {
		return nil, nil, nil, ErrRegistered
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, nil, nil, err
	}
	f, err := approval.OpenPrivate(filepath.Join(dir, "setup-code.json"), os.O_RDWR)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil, nil, ErrSetup
		}
		return nil, nil, nil, err
	}
	defer f.Close()
	var stored codeFile
	if err := json.NewDecoder(f).Decode(&stored); err != nil || len(stored.Hash) != sha256.Size {
		return nil, nil, nil, ErrSetup
	}
	sum := sha256.Sum256(raw)
	if subtle.ConstantTimeCompare(sum[:], stored.Hash) != 1 || !now.Before(stored.Expires) {
		return nil, nil, nil, ErrSetup
	}
	create, assert, salt = make([]byte, tokenBytes), make([]byte, tokenBytes), make([]byte, prfBytes)
	if _, err := rand.Read(create); err != nil {
		return nil, nil, nil, err
	}
	if _, err := rand.Read(assert); err != nil {
		return nil, nil, nil, err
	}
	if _, err := rand.Read(salt); err != nil {
		return nil, nil, nil, err
	}
	body, err := json.Marshal(pendingFile{
		Origin: rp.Origin, RPID: rp.ID, Create: create, Assert: assert, Salt: salt, Expires: now.Add(pendingLife).UTC(),
	})
	if err != nil {
		return nil, nil, nil, err
	}
	if err := writeNew(dir, "register-pending.json", body); err != nil {
		return nil, nil, nil, err
	}
	if err := os.Remove(filepath.Join(dir, "setup-code.json")); err != nil {
		return nil, nil, nil, err
	}
	return create, assert, salt, nil
}

// Finish verifies the create ceremony and the following assertion, then stores the
// credential and a data key wrapped under that assertion's PRF output. The output
// is not written. A passkey without PRF is refused.
func Finish(dir string, rp webauthn.RelyingParty, create, assert Ceremony, now time.Time) error {
	if !create.PRFEnabled || len(assert.PRFOutput) != prfBytes {
		return ErrPRF
	}
	defer clear(assert.PRFOutput)
	f, err := approval.OpenPrivate(filepath.Join(dir, "register-pending.json"), os.O_RDWR)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return ErrPending
		}
		return err
	}
	defer f.Close()
	var pending pendingFile
	if err := json.NewDecoder(f).Decode(&pending); err != nil || pending.Origin != rp.Origin || pending.RPID != rp.ID ||
		!now.Before(pending.Expires) {
		return ErrPending
	}
	reg, err := webauthn.VerifyRegistration(webauthn.RelyingParty{ID: rp.ID, Origin: rp.Origin}, pending.Create, create.ClientDataJSON, create.AttestationObject)
	if err != nil {
		return err
	}
	got, err := webauthn.VerifyAssertion(webauthn.RelyingParty{ID: rp.ID, Origin: rp.Origin}, pending.Assert, webauthn.Credential{
		ID: reg.CredentialID, COSEKey: reg.COSEKey, SignCount: reg.SignCount,
	}, assert.CredentialID, assert.ClientDataJSON, assert.AuthenticatorData, assert.Signature)
	if err != nil {
		return err
	}
	wrapped, err := wrapKey(assert.PRFOutput, pending.Salt, []byte(rp.Origin))
	if err != nil {
		return err
	}
	body, err := json.Marshal(storeFile{Origin: rp.Origin, RPID: rp.ID, Credentials: []credential{{
		ID: reg.CredentialID, COSE: reg.COSEKey, Alg: int64(reg.Algorithm), SignCount: got.SignCount,
		Salt: pending.Salt, Wrap: wrapped, Name: "Passkey", Added: now.UTC(),
	}}})
	if err != nil {
		return err
	}
	if err := writeNew(dir, "passkeys.json", body); err != nil {
		return err
	}
	return os.Remove(filepath.Join(dir, "register-pending.json"))
}

// OpenVault unwraps the write-vault data key for one credential. The caller must clear the key.
// A wrong PRF output fails closed and approves nothing.
func OpenVault(dir string, credentialID, prf []byte, origin string) (key []byte, err error) {
	if len(prf) != prfBytes {
		return nil, ErrPRF
	}
	defer clear(prf)
	body, err := os.ReadFile(filepath.Join(dir, "passkeys.json"))
	if err != nil {
		return nil, err
	}
	var stored storeFile
	if err := json.Unmarshal(body, &stored); err != nil || stored.Origin != origin {
		return nil, ErrPending
	}
	var found *credential
	for i := range stored.Credentials {
		if subtle.ConstantTimeCompare(stored.Credentials[i].ID, credentialID) == 1 {
			found = &stored.Credentials[i]
			break
		}
	}
	if found == nil {
		return nil, errors.New("passkey not registered")
	}
	kek, err := hkdf.Key(sha256.New, prf, found.Salt, hkdfInfo, keyBytes)
	if err != nil {
		return nil, err
	}
	defer clear(kek)
	block, err := aes.NewCipher(kek)
	if err != nil {
		return nil, err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	return gcm.Open(nil, found.Wrap.Nonce, found.Wrap.Key, []byte(origin))
}

func wrapKey(prf, salt, origin []byte) (wrapped, error) {
	kek, err := hkdf.Key(sha256.New, prf, salt, hkdfInfo, keyBytes)
	if err != nil {
		return wrapped{}, err
	}
	defer clear(kek)
	block, err := aes.NewCipher(kek)
	if err != nil {
		return wrapped{}, err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return wrapped{}, err
	}
	data := make([]byte, keyBytes)
	if _, err := rand.Read(data); err != nil {
		return wrapped{}, err
	}
	defer clear(data)
	nonce := make([]byte, nonceBytes)
	if _, err := rand.Read(nonce); err != nil {
		return wrapped{}, err
	}
	return wrapped{Nonce: nonce, Key: gcm.Seal(nil, nonce, data, origin)}, nil
}

type codeFile struct {
	Hash    []byte    `json:"hash"`
	Expires time.Time `json:"expires"`
}

type pendingFile struct {
	Origin  string    `json:"origin"`
	RPID    string    `json:"rp_id"`
	Create  []byte    `json:"create_challenge"`
	Assert  []byte    `json:"assert_challenge"`
	Salt    []byte    `json:"prf_salt"`
	Expires time.Time `json:"expires"`
}

type storeFile struct {
	Origin      string       `json:"origin"`
	RPID        string       `json:"rp_id"`
	Credentials []credential `json:"credentials"`
}

type credential struct {
	ID        []byte    `json:"id"`
	COSE      []byte    `json:"cose"`
	Alg       int64     `json:"alg"`
	SignCount uint32    `json:"sign_count"`
	Salt      []byte    `json:"prf_salt"`
	Wrap      wrapped   `json:"wrapped_data_key"`
	Name      string    `json:"name"`
	Added     time.Time `json:"added_at"`
}

type wrapped struct {
	Nonce []byte `json:"nonce"`
	Key   []byte `json:"key"`
}

func writeNew(dir, name string, body []byte) error {
	path := filepath.Join(dir, name)
	tmp, err := os.CreateTemp(dir, "."+name+"-")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if err := os.Chmod(tmp.Name(), 0o600); err != nil {
		tmp.Close()
		return err
	}
	if _, err := tmp.Write(body); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmp.Name(), path); err != nil {
		return err
	}
	d, err := os.Open(dir)
	if err != nil {
		return err
	}
	defer d.Close()
	return unix.Fsync(int(d.Fd()))
}
