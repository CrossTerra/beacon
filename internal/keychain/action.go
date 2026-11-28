package keychain

import (
	"bytes"
	"crypto/aes"
	"crypto/cipher"
	"crypto/sha256"
	"errors"
	"net/url"

	"github.com/CrossTerra/beacon/internal/vault"
)

var errActionCredential = errors.New("action credential unavailable")

// ActionTokens takes the two password inputs from the credential settings form without returning their bytes.
// Neither value is stored here. The save route first verifies a passkey assertion for the configured binding.
func ActionTokens(form url.Values) (read, write Secret, err error) {
	r, w := form["read_token"], form["write_token"]
	if len(r) != 1 || len(w) != 1 {
		return Secret{}, Secret{}, errActionCredential
	}
	read, write = Secret{value: []byte(r[0])}, Secret{value: []byte(w[0])}
	delete(form, "read_token")
	delete(form, "write_token")
	if !ActionToken(read) || !ActionToken(write) {
		read.Clear()
		write.Clear()
		return Secret{}, Secret{}, errActionCredential
	}
	return read, write, nil
}

// ActionToken checks the bearer header grammar. Tokens from settings or disk must not supply whitespace or
// control characters to the HTTP client.
func ActionToken(s Secret) bool {
	if len(s.value) == 0 || len(s.value) > 2048 {
		return false
	}
	for _, b := range s.value {
		if !(b >= 'a' && b <= 'z' || b >= 'A' && b <= 'Z' || b >= '0' && b <= '9' ||
			b == '-' || b == '_' || b == '.' || b == '~' || b == '+' || b == '/' || b == '=') {
			return false
		}
	}
	return true
}

// ActionSlackUserToken accepts only Slack's user OAuth token form, before any network request.
func ActionSlackUserToken(s Secret) bool {
	if len(s.value) <= 5 || len(s.value) > 1029 || !bytes.HasPrefix(s.value, []byte("xoxp-")) {
		return false
	}
	for _, b := range s.value[5:] {
		if !(b >= 'a' && b <= 'z' || b >= 'A' && b <= 'Z' || b >= '0' && b <= '9' || b == '-') {
			return false
		}
	}
	return true
}

// FineGrainedGitHubPAT recognizes the token form only. It says nothing about the permissions granted.
func FineGrainedGitHubPAT(s Secret) bool {
	return len(s.value) > len("github_pat_") && bytes.HasPrefix(s.value, []byte("github_pat_")) && ActionToken(s)
}

// ActionTokenFingerprint ties a short-lived, in-memory identity check to this exact credential. It must
// not be logged or saved, and is never accepted in place of a token.
func ActionTokenFingerprint(s Secret) [32]byte { return sha256.Sum256(s.value) }

// ReadAction reads only the unattended credential for b. A write token never goes into this database.
func ReadAction(dir string, b vault.Binding) (Secret, error) {
	b.Use = "read"
	return readBound(dir, b)
}

// ReadRecordKey opens only the unattended app-records key for this Signal origin and app actor.
// It is not a provider write credential and never comes from a proposal or the service environment.
func ReadRecordKey(dir, origin, actor string) (Secret, error) {
	s, err := readBound(dir, vault.SignalRecordBinding(origin, actor))
	if err != nil || !bytes.HasPrefix(s.value, []byte("sig_")) || len(s.value) == 4 {
		s.Clear()
		return Secret{}, errActionCredential
	}
	return s, nil
}

func readBound(dir string, b vault.Binding) (Secret, error) {
	value, err := vault.Get(dir, b)
	secret := Secret{value: []byte(value)}
	if err != nil || !ActionToken(secret) {
		secret.Clear()
		return Secret{}, errActionCredential
	}
	return secret, nil
}

func StoreActionRead(dir string, b vault.Binding, secret Secret) error {
	if !ActionToken(secret) {
		return errActionCredential
	}
	b.Use = "read"
	if err := vault.Create(dir); err != nil && !errors.Is(err, vault.ErrExists) {
		return errActionCredential
	}
	if err := vault.Put(dir, b, string(secret.value)); err != nil {
		return errActionCredential
	}
	return nil
}

// StoreActionWrite seals the token under the assertion's data key and the exact binding. Substituting an
// entry at a different provider, origin, account or workspace does not open it.
func StoreActionWrite(dir string, b vault.Binding, secret Secret, key []byte) error {
	aead, err := actionCipher(key)
	if err != nil || !ActionToken(secret) {
		return errActionCredential
	}
	return vault.StoreSealed(dir, b.Service, aead.Seal(nil, nil, secret.value, writeBinding(b)))
}

func UnsealActionWrite(dir string, b vault.Binding, key []byte) (Secret, error) {
	aead, err := actionCipher(key)
	if err != nil {
		return Secret{}, errActionCredential
	}
	sealed, err := vault.LoadSealed(dir, b.Service)
	if err != nil {
		return Secret{}, errActionCredential
	}
	value, err := aead.Open(nil, nil, sealed, writeBinding(b))
	secret := Secret{value: value}
	if err != nil || !ActionToken(secret) {
		clear(value)
		return Secret{}, errActionCredential
	}
	return secret, nil
}

func actionCipher(key []byte) (cipher.AEAD, error) {
	if len(key) != 32 {
		return nil, errActionCredential
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, errActionCredential
	}
	return cipher.NewGCMWithRandomNonce(block)
}

func writeBinding(b vault.Binding) []byte {
	// Provider bindings are validated ASCII identifiers and cannot contain NUL.
	return []byte("beacon/write/v1\x00" + b.Service + "\x00" + b.Origin + "\x00" + b.Account + "\x00" + b.Workspace)
}

// Clear overwrites a short-lived credential's backing bytes. Copies of the Secret share those bytes.
func (s Secret) Clear() { clear(s.value) }
