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
	"io"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/CrossTerra/beacon/internal/approval"
	"github.com/CrossTerra/beacon/internal/webauthn"
	"golang.org/x/sys/unix"
)

const (
	codeLife       = 10 * time.Minute
	pendingLife    = 2 * time.Minute
	tokenBytes     = 32
	prfBytes       = 32
	keyBytes       = 32
	nonceBytes     = 12
	hkdfInfo       = "beacon-write-vault-v1"
	vaultPlain     = "beacon-write-vault-v1"
	writeVaultName = "write-vault.json"
	resetMarker    = "passkey-reset.json"
)

var mu sync.Mutex

var (
	ErrSetup      = errors.New("setup code is missing, expired, or already used")
	ErrPending    = errors.New("passkey registration is not waiting")
	ErrRegistered = errors.New("a passkey is already registered")
	ErrPRF        = errors.New("passkey registration needs PRF")
	ErrVault      = errors.New("write vault is absent or does not match this passkey")
	ErrReset      = errors.New("write vault does not match the recorded passkey")
	ErrSignCount  = errors.New("sign count did not increase")
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
	defer approval.CloseInstance(dir, lock)
	mu.Lock()
	defer mu.Unlock()
	if err := clearDebris(dir); err != nil {
		return "", err
	}
	resetIndex, block, err := priorPasskeys(dir)
	if err != nil {
		return "", err
	}
	if block && resetIndex == 0 {
		return "", ErrRegistered
	}
	raw := make([]byte, tokenBytes)
	if _, err := rand.Read(raw); err != nil {
		return "", err
	}
	sum := sha256.Sum256(raw)
	code := codeFile{Hash: sum[:], Expires: now.Add(codeLife).UTC(), IssuedAt: now.UTC(), ResetIndex: resetIndex}
	body, err := json.Marshal(code)
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
	release, err := withInstance(dir)
	if err != nil {
		return nil, nil, nil, err
	}
	defer release()
	mu.Lock()
	defer mu.Unlock()
	if err := clearDebris(dir); err != nil {
		return nil, nil, nil, err
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
	resetIndex, block, err := priorPasskeys(dir)
	if err != nil {
		return nil, nil, nil, err
	}
	stored, err := claimCode(dir)
	if err != nil {
		return nil, nil, nil, err
	}
	sum := sha256.Sum256(raw)
	if subtle.ConstantTimeCompare(sum[:], stored.Hash) != 1 || !now.Before(stored.Expires) || len(stored.Hash) != sha256.Size {
		return nil, nil, nil, ErrSetup
	}
	if block && (resetIndex == 0 || stored.ResetIndex != resetIndex) {
		return nil, nil, nil, ErrRegistered
	}
	if !block && stored.ResetIndex != 0 {
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
		Origin: rp.Origin, RPID: rp.ID, Create: create, Assert: assert, Salt: salt,
		ResetIndex: resetIndex, Expires: now.Add(pendingLife).UTC(),
	})
	if err != nil {
		return nil, nil, nil, err
	}
	if err := writeNew(dir, "register-pending.json", body); err != nil {
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
	release, err := withInstance(dir)
	if err != nil {
		return err
	}
	defer release()
	mu.Lock()
	defer mu.Unlock()
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
	_, resetIndex, err := approval.PasskeyMarks(dir)
	if err != nil {
		return err
	}
	if pending.ResetIndex != resetIndex {
		os.Remove(filepath.Join(dir, "register-pending.json"))
		return ErrRegistered
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
	wrapped, dataKey, err := wrapKey(assert.PRFOutput, pending.Salt, []byte(rp.Origin))
	if err != nil {
		return err
	}
	defer clear(dataKey)
	if err := sealVault(dir, dataKey, rp.Origin); err != nil {
		return err
	}
	body, err := json.Marshal(storeFile{Origin: rp.Origin, RPID: rp.ID, Credentials: []credential{{
		ID: reg.CredentialID, COSE: reg.COSEKey, Alg: int64(reg.Algorithm), SignCount: got.SignCount,
		Salt: pending.Salt, Wrap: wrapped, Name: "Passkey", Added: now.UTC(),
	}}})
	if err != nil {
		return err
	}
	if err := publishNew(dir, "passkeys.json", body); err != nil {
		os.Remove(filepath.Join(dir, writeVaultName))
		return err
	}
	return os.Remove(filepath.Join(dir, "register-pending.json"))
}

// CommitAdded records the write-vault hash. If that record fails, both registration files are removed.
func CommitAdded(dir string, at time.Time, record func([]byte, time.Time) error) error {
	hash, err := VaultHash(dir)
	if err != nil || record == nil {
		Abandon(dir)
		if err == nil {
			err = errors.New("passkey was not recorded")
		}
		return err
	}
	if err := record(hash, at); err != nil {
		Abandon(dir)
		return err
	}
	return nil
}

// VaultHash is the SHA-256 of write-vault.json.
func VaultHash(dir string) ([]byte, error) {
	body, err := readVault(dir)
	if err != nil {
		return nil, err
	}
	sum := sha256.Sum256(body)
	return append([]byte(nil), sum[:]...), nil
}

func readVault(dir string) ([]byte, error) {
	f, err := approval.OpenPrivate(filepath.Join(dir, writeVaultName), os.O_RDONLY)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	body, err := io.ReadAll(io.LimitReader(f, 4097))
	if err != nil || len(body) > 4096 {
		return nil, ErrVault
	}
	return body, nil
}

// Abandon removes a registration that was not recorded. The reset marker stays.
func Abandon(dir string) {
	os.Remove(filepath.Join(dir, "passkeys.json"))
	os.Remove(filepath.Join(dir, writeVaultName))
}

// OpenVault unwraps the write-vault data key for one credential. The caller must clear the key.
// A wrong PRF output fails closed and approves nothing.
func OpenVault(dir string, credentialID, prf []byte, origin string) ([]byte, error) {
	return openVault(dir, credentialID, prf, origin, readVault)
}

func openVault(dir string, credentialID, prf []byte, origin string, read func(string) ([]byte, error)) ([]byte, error) {
	if len(prf) != prfBytes {
		return nil, ErrPRF
	}
	defer clear(prf)
	vaultBody, err := read(dir)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, ErrVault
		}
		return nil, err
	}
	sum := sha256.Sum256(vaultBody)
	matches, err := approval.CheckPasskeyVault(dir, sum[:], time.Now().UTC())
	if err != nil {
		return nil, err
	}
	if !matches {
		return nil, ErrReset
	}
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
	if len(found.Wrap.Nonce) != gcm.NonceSize() {
		return nil, ErrVault
	}
	key, err := gcm.Open(nil, found.Wrap.Nonce, found.Wrap.Key, []byte(origin))
	if err != nil {
		return nil, err
	}
	if err := openVaultCheck(vaultBody, key, origin); err != nil {
		clear(key)
		return nil, err
	}
	return key, nil
}

func wrapKey(prf, salt, origin []byte) (wrapped, []byte, error) {
	kek, err := hkdf.Key(sha256.New, prf, salt, hkdfInfo, keyBytes)
	if err != nil {
		return wrapped{}, nil, err
	}
	defer clear(kek)
	block, err := aes.NewCipher(kek)
	if err != nil {
		return wrapped{}, nil, err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return wrapped{}, nil, err
	}
	data := make([]byte, keyBytes)
	if _, err := rand.Read(data); err != nil {
		return wrapped{}, nil, err
	}
	nonce := make([]byte, nonceBytes)
	if _, err := rand.Read(nonce); err != nil {
		clear(data)
		return wrapped{}, nil, err
	}
	return wrapped{Nonce: nonce, Key: gcm.Seal(nil, nonce, data, origin)}, data, nil
}

func sealVault(dir string, key []byte, origin string) error {
	block, err := aes.NewCipher(key)
	if err != nil {
		return err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return err
	}
	nonce := make([]byte, nonceBytes)
	if _, err := rand.Read(nonce); err != nil {
		return err
	}
	body, err := json.Marshal(vaultFile{Origin: origin, Nonce: nonce, Check: gcm.Seal(nil, nonce, []byte(vaultPlain), []byte(origin))})
	if err != nil {
		return err
	}
	return publishNew(dir, writeVaultName, body)
}

func openVaultCheck(body, key []byte, origin string) error {
	var stored vaultFile
	if err := json.Unmarshal(body, &stored); err != nil || stored.Origin != origin || len(stored.Nonce) != nonceBytes {
		return ErrVault
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return err
	}
	plain, err := gcm.Open(nil, stored.Nonce, stored.Check, []byte(origin))
	if err != nil || subtle.ConstantTimeCompare(plain, []byte(vaultPlain)) != 1 {
		return ErrVault
	}
	return nil
}

// NoteSignCount persists the assertion's sign count before an approval is committed.
// A non-zero count must have increased. Both zero is an authenticator that does not count.
func NoteSignCount(dir string, id []byte, count uint32) error {
	release, err := withInstance(dir)
	if err != nil {
		return err
	}
	defer release()
	mu.Lock()
	defer mu.Unlock()
	path := filepath.Join(dir, "passkeys.json")
	body, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	var stored storeFile
	if err := json.Unmarshal(body, &stored); err != nil {
		return err
	}
	found := -1
	for i := range stored.Credentials {
		if subtle.ConstantTimeCompare(stored.Credentials[i].ID, id) == 1 {
			found = i
			break
		}
	}
	if found < 0 {
		return errors.New("passkey not registered")
	}
	prev := stored.Credentials[found].SignCount
	if !(count > prev || count == 0 && prev == 0) {
		return ErrSignCount
	}
	stored.Credentials[found].SignCount = count
	next, err := json.Marshal(stored)
	if err != nil {
		return err
	}
	return writeNew(dir, "passkeys.json", next)
}

// Reset records a journal reset, leaves a marker, and removes the passkey list and write vault.
// The marker stays, so a later registration must use a code issued after that journal reset.
func Reset(dir string, at time.Time, record func(string, time.Time) error) error {
	// The caller opens the journal first, which already holds the instance lock.
	if !approval.HoldsInstance(dir) {
		return approval.ErrLocked
	}
	mu.Lock()
	defer mu.Unlock()
	if record == nil {
		return errors.New("passkey reset is not recorded")
	}
	if err := record("passkeys_reset", at); err != nil {
		return err
	}
	body, err := json.Marshal(struct {
		At int64 `json:"at"`
	}{at.UnixMilli()})
	if err != nil {
		return err
	}
	if err := writeNew(dir, resetMarker, body); err != nil {
		return err
	}
	if err := os.Remove(filepath.Join(dir, "passkeys.json")); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if err := os.Remove(filepath.Join(dir, writeVaultName)); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if err := os.Remove(filepath.Join(dir, "register-pending.json")); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}

func clearDebris(dir string) error {
	if _, err := os.Lstat(filepath.Join(dir, "passkeys.json")); err == nil {
		return nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	added, _, err := approval.PasskeyMarks(dir)
	if err != nil || added != 0 {
		return err
	}
	if err := os.Remove(filepath.Join(dir, writeVaultName)); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}

func priorPasskeys(dir string) (resetIndex int, block bool, err error) {
	addedIndex, resetIndex, err := approval.PasskeyMarks(dir)
	if err != nil {
		return 0, false, err
	}
	_, vaultErr := os.Lstat(filepath.Join(dir, writeVaultName))
	vault := vaultErr == nil
	if vaultErr != nil && !errors.Is(vaultErr, os.ErrNotExist) {
		return 0, false, vaultErr
	}
	_, markerErr := os.Lstat(filepath.Join(dir, resetMarker))
	marker := markerErr == nil
	if markerErr != nil && !errors.Is(markerErr, os.ErrNotExist) {
		return 0, false, markerErr
	}
	if !vault && !marker && addedIndex == 0 {
		// A journalled reset still binds the ceremony if the marker was never written or was removed.
		return resetIndex, resetIndex != 0, nil
	}
	if !vault && resetIndex > addedIndex {
		return resetIndex, true, nil
	}
	return 0, true, nil
}

func claimCode(dir string) (codeFile, error) {
	src := filepath.Join(dir, "setup-code.json")
	dst := filepath.Join(dir, "setup-code.claimed")
	if err := os.Remove(dst); err != nil && !errors.Is(err, os.ErrNotExist) {
		return codeFile{}, err
	}
	if err := os.Rename(src, dst); err != nil {
		return codeFile{}, ErrSetup
	}
	defer os.Remove(dst)
	f, err := approval.OpenPrivate(dst, os.O_RDONLY)
	if err != nil {
		return codeFile{}, ErrSetup
	}
	defer f.Close()
	var stored codeFile
	if err := json.NewDecoder(f).Decode(&stored); err != nil {
		return codeFile{}, ErrSetup
	}
	return stored, nil
}

func withInstance(dir string) (func(), error) {
	f, err := approval.LockInstance(dir)
	if err == nil {
		return func() { approval.CloseInstance(dir, f) }, nil
	}
	if errors.Is(err, approval.ErrLocked) && approval.HoldsInstance(dir) {
		return func() {}, nil
	}
	return nil, err
}

type codeFile struct {
	Hash       []byte    `json:"hash"`
	Expires    time.Time `json:"expires"`
	IssuedAt   time.Time `json:"issued_at"`
	ResetIndex int       `json:"reset_index,omitempty"`
}

type vaultFile struct {
	Origin string `json:"origin"`
	Nonce  []byte `json:"nonce"`
	Check  []byte `json:"check"`
}

type pendingFile struct {
	Origin     string    `json:"origin"`
	RPID       string    `json:"rp_id"`
	Create     []byte    `json:"create_challenge"`
	Assert     []byte    `json:"assert_challenge"`
	Salt       []byte    `json:"prf_salt"`
	ResetIndex int       `json:"reset_index,omitempty"`
	Expires    time.Time `json:"expires"`
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

func publishNew(dir, name string, body []byte) error {
	path := filepath.Join(dir, name)
	tmp, err := os.CreateTemp(dir, "."+name+"-")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if err := writeTemp(tmp, body); err != nil {
		return err
	}
	if err := os.Link(tmp.Name(), path); err != nil {
		if errors.Is(err, os.ErrExist) {
			return ErrRegistered
		}
		return err
	}
	return syncDir(dir)
}

func writeNew(dir, name string, body []byte) error {
	path := filepath.Join(dir, name)
	tmp, err := os.CreateTemp(dir, "."+name+"-")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if err := writeTemp(tmp, body); err != nil {
		return err
	}
	if err := os.Rename(tmp.Name(), path); err != nil {
		return err
	}
	return syncDir(dir)
}

func writeTemp(tmp *os.File, body []byte) error {
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
	return tmp.Close()
}

func syncDir(dir string) error {
	d, err := os.Open(dir)
	if err != nil {
		return err
	}
	defer d.Close()
	return unix.Fsync(int(d.Fd()))
}
