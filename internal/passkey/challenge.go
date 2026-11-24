package passkey

import (
	"crypto/sha256"
	"encoding/base64"

	"github.com/CrossTerra/beacon/internal/approval"
)

// ApprovalChallenge is the WebAuthn challenge for one approval. It is the SHA-256 of the canonical
// binding, so a different hash, order, origin, or nonce is a different assertion.
func ApprovalChallenge(origin, instance string, nonce []byte, hashes []string) ([]byte, error) {
	approved := make([]any, len(hashes))
	for i, h := range hashes {
		approved[i] = h
	}
	b, err := approval.Canonical(approval.Object{
		"v": int64(1), "kind": "approve", "origin": origin, "instance": instance,
		"nonce": base64.RawURLEncoding.EncodeToString(nonce), "approved": approved,
	})
	if err != nil {
		return nil, err
	}
	sum := sha256.Sum256(b)
	return sum[:], nil
}

// SettingsChallenge authorizes storing credentials for one configured binding, never an action.
func SettingsChallenge(origin, instance string, nonce []byte, service, provider, account, workspace string) ([]byte, error) {
	b, err := approval.Canonical(approval.Object{
		"v": int64(1), "kind": "settings", "origin": origin, "instance": instance,
		"nonce": base64.RawURLEncoding.EncodeToString(nonce), "service": service, "provider": provider,
		"account": account, "workspace": workspace,
	})
	if err != nil {
		return nil, err
	}
	sum := sha256.Sum256(b)
	return sum[:], nil
}
