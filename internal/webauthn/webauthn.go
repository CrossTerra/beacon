// Package webauthn verifies the WebAuthn ceremonies Beacon uses (13.01-D22).
//
// It is deliberately narrow. Registration is attestation none: the attestation
// statement is not checked, because synced passkeys do not provide one and the
// PRF check does not need it. The public key must be ES256, EdDSA or RS256.
// An assertion is checked for its authenticator data and flags, clientDataJSON,
// signature and sign count. The PRF output is not part of the signed data; the
// caller handles it.
//
// Decoding uses github.com/fxamacker/cbor/v2. go-webauthn is not imported here.
// Tests cross-check verdicts against it.
package webauthn

import (
	"crypto"
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/subtle"
	"errors"
)

// Algorithm is a COSE algorithm identifier Beacon accepts.
type Algorithm int64

const (
	// AlgES256 is ECDSA over P-256 with SHA-256.
	AlgES256 Algorithm = -7
	// AlgEdDSA is Ed25519.
	AlgEdDSA Algorithm = -8
	// AlgRS256 is RSASSA-PKCS1-v1.5 with SHA-256.
	AlgRS256 Algorithm = -257
)

// String returns the name used on the spike page. Unknown algorithms are "unknown".
func (a Algorithm) String() string {
	switch a {
	case AlgES256:
		return "ES256"
	case AlgEdDSA:
		return "EdDSA"
	case AlgRS256:
		return "RS256"
	default:
		return "unknown"
	}
}

// Ceremony is the clientDataJSON type.
type Ceremony string

const (
	// CeremonyCreate is navigator.credentials.create.
	CeremonyCreate Ceremony = "webauthn.create"
	// CeremonyGet is navigator.credentials.get.
	CeremonyGet Ceremony = "webauthn.get"
)

// RelyingParty is the canonical RP ID and origin for one request (13.01-D4).
type RelyingParty struct {
	// ID is the WebAuthn RP ID: "localhost" or the ts.net name, with no scheme or port.
	ID string
	// Origin is the exact origin, including the scheme and, for localhost, the port.
	Origin string
}

// Registration is a credential a create ceremony proved.
type Registration struct {
	CredentialID   []byte
	COSEKey        []byte
	Algorithm      Algorithm
	SignCount      uint32
	AAGUID         [16]byte
	BackupEligible bool
	BackupState    bool
}

// Credential is a stored credential an assertion is checked against.
type Credential struct {
	ID        []byte
	COSEKey   []byte
	SignCount uint32
}

// Assertion is a verified get ceremony. The caller stores SignCount.
type Assertion struct {
	CredentialID   []byte
	SignCount      uint32
	BackupEligible bool
	BackupState    bool
}

// Error is a fixed reason. Its text is the code only, so a log cannot pick up
// key material, an assertion or a PRF output from it.
type Error struct {
	Code string
}

func (e *Error) Error() string { return e.Code }

// Is reports whether err is an Error with the same code.
func (e *Error) Is(err error) bool {
	var o *Error
	if !errors.As(err, &o) {
		return false
	}
	return e.Code == o.Code
}

func errCode(code string) error { return &Error{Code: code} }

// Reason codes. Callers map these to a page; they are not sentences.
const (
	CodeMalformed    = "malformed"
	CodeClientData   = "client_data"
	CodeType         = "type"
	CodeChallenge    = "challenge"
	CodeOrigin       = "origin"
	CodeCrossOrigin  = "cross_origin"
	CodeRPID         = "rp_id"
	CodeAuthData     = "auth_data"
	CodeFlags        = "flags"
	CodeUserPresent  = "user_present"
	CodeUserVerified = "user_verified"
	CodeAttestation  = "attestation"
	CodeCredential   = "credential"
	CodePublicKey    = "public_key"
	CodeSignature    = "signature"
	CodeSignCount    = "sign_count"
)

// Limits. Real ceremonies are a few hundred bytes; an RSA key is the large case.
const (
	maxClientDataJSON = 16 << 10
	maxAttestation    = 16 << 10
	maxAuthData       = 8 << 10
	maxSignature      = 1024
	maxCredentialID   = 1023
	minAuthData       = 37
)

// VerifyRegistration checks a create ceremony. attestationObject is the raw
// CBOR bytes, not base64. challenge is the raw bytes issued to the client.
func VerifyRegistration(rp RelyingParty, challenge, clientDataJSON, attestationObject []byte) (Registration, error) {
	if len(clientDataJSON) == 0 || len(clientDataJSON) > maxClientDataJSON {
		return Registration{}, errCode(CodeClientData)
	}
	if len(attestationObject) == 0 || len(attestationObject) > maxAttestation {
		return Registration{}, errCode(CodeAttestation)
	}
	if _, err := parseClientData(clientDataJSON, CeremonyCreate, rp.Origin, challenge); err != nil {
		return Registration{}, err
	}
	auth, cose, err := parseAttestation(attestationObject, rp.ID)
	if err != nil {
		return Registration{}, err
	}
	pub, err := ParseCOSEKey(cose)
	if err != nil {
		return Registration{}, err
	}
	return Registration{
		CredentialID:   append([]byte(nil), auth.credentialID...),
		COSEKey:        append([]byte(nil), cose...),
		Algorithm:      pub.Algorithm,
		SignCount:      auth.signCount,
		AAGUID:         auth.aaguid,
		BackupEligible: auth.backupEligible,
		BackupState:    auth.backupState,
	}, nil
}

// VerifyAssertion checks a get ceremony. authenticatorData and signature are
// raw bytes. cred is the credential looked up by the asserted id, which must
// equal cred.ID.
func VerifyAssertion(rp RelyingParty, challenge []byte, cred Credential, assertedID, clientDataJSON, authenticatorData, signature []byte) (Assertion, error) {
	if len(clientDataJSON) == 0 || len(clientDataJSON) > maxClientDataJSON {
		return Assertion{}, errCode(CodeClientData)
	}
	if len(authenticatorData) < minAuthData || len(authenticatorData) > maxAuthData {
		return Assertion{}, errCode(CodeAuthData)
	}
	if len(signature) == 0 || len(signature) > maxSignature {
		return Assertion{}, errCode(CodeSignature)
	}
	if len(cred.ID) == 0 || len(assertedID) == 0 || subtle.ConstantTimeCompare(cred.ID, assertedID) != 1 {
		return Assertion{}, errCode(CodeCredential)
	}
	if _, err := parseClientData(clientDataJSON, CeremonyGet, rp.Origin, challenge); err != nil {
		return Assertion{}, err
	}
	auth, err := parseAssertionAuthData(authenticatorData, rp.ID)
	if err != nil {
		return Assertion{}, err
	}
	pub, err := ParseCOSEKey(cred.COSEKey)
	if err != nil {
		return Assertion{}, err
	}
	if err := verifySignature(pub, authenticatorData, clientDataJSON, signature); err != nil {
		return Assertion{}, err
	}
	if err := checkSignCount(cred.SignCount, auth.signCount); err != nil {
		return Assertion{}, err
	}
	return Assertion{
		CredentialID:   append([]byte(nil), cred.ID...),
		SignCount:      auth.signCount,
		BackupEligible: auth.backupEligible,
		BackupState:    auth.backupState,
	}, nil
}

// checkSignCount applies 13.01-D22: a non-zero sign count must have increased.
// Both zero is an authenticator that does not count, which synced passkeys do.
func checkSignCount(stored, got uint32) error {
	if got == 0 && stored == 0 {
		return nil
	}
	if got > stored {
		return nil
	}
	return errCode(CodeSignCount)
}

func verifySignature(pub PublicKey, authenticatorData, clientDataJSON, signature []byte) error {
	sum := sha256.Sum256(clientDataJSON)
	signed := make([]byte, 0, len(authenticatorData)+len(sum))
	signed = append(signed, authenticatorData...)
	signed = append(signed, sum[:]...)
	switch pub.Algorithm {
	case AlgES256:
		key, ok := pub.key.(*ecdsa.PublicKey)
		if !ok {
			return errCode(CodePublicKey)
		}
		hash := sha256.Sum256(signed)
		if !ecdsa.VerifyASN1(key, hash[:], signature) {
			return errCode(CodeSignature)
		}
	case AlgEdDSA:
		key, ok := pub.key.(ed25519.PublicKey)
		if !ok || !ed25519.Verify(key, signed, signature) {
			return errCode(CodeSignature)
		}
	case AlgRS256:
		key, ok := pub.key.(*rsa.PublicKey)
		if !ok {
			return errCode(CodePublicKey)
		}
		hash := sha256.Sum256(signed)
		if rsa.VerifyPKCS1v15(key, crypto.SHA256, hash[:], signature) != nil {
			return errCode(CodeSignature)
		}
	default:
		return errCode(CodePublicKey)
	}
	return nil
}
