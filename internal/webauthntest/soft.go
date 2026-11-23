// Package webauthntest builds WebAuthn ceremonies for tests. It is not linked
// into the beacon binary.
package webauthntest

import (
	"crypto"
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"

	"github.com/fxamacker/cbor/v2"
)

// Alg is a COSE algorithm identifier. The values match internal/webauthn.
type Alg int64

const (
	AlgES256 Alg = -7
	AlgEdDSA Alg = -8
	AlgRS256 Alg = -257
)

// String is the algorithm's name.
func (a Alg) String() string {
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

// Key is a software authenticator credential.
type Key struct {
	Alg          Alg
	ID           []byte
	COSE         []byte
	SignCount    uint32
	priv         any
	backup       bool
	userPresent  bool
	userVerified bool
}

// Generate returns a key of the given algorithm. ID is 16 random bytes.
func Generate(alg Alg) (*Key, error) {
	id := make([]byte, 16)
	if _, err := rand.Read(id); err != nil {
		return nil, err
	}
	k := &Key{Alg: alg, ID: id, userPresent: true, userVerified: true}
	var err error
	switch alg {
	case AlgES256:
		var priv *ecdsa.PrivateKey
		priv, err = ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
		if err != nil {
			return nil, err
		}
		k.priv = priv
		k.COSE, err = marshalES256(&priv.PublicKey)
	case AlgEdDSA:
		pub, priv, e := ed25519.GenerateKey(rand.Reader)
		if e != nil {
			return nil, e
		}
		k.priv = priv
		k.COSE, err = marshalEdDSA(pub)
	case AlgRS256:
		var priv *rsa.PrivateKey
		priv, err = rsa.GenerateKey(rand.Reader, 2048)
		if err != nil {
			return nil, err
		}
		k.priv = priv
		k.COSE, err = marshalRS256(&priv.PublicKey)
	default:
		return nil, errUnsupported
	}
	if err != nil {
		return nil, err
	}
	return k, nil
}

// Backup sets the BE and BS flags, as an iCloud Keychain passkey does.
func (k *Key) Backup(on bool) { k.backup = on }

// Flags clears user presence or user verification so a test can refuse them.
func (k *Key) Flags(up, uv bool) {
	k.userPresent = up
	k.userVerified = uv
}

// Ceremony is one create or get the verifier can be given.
type Ceremony struct {
	ClientDataJSON    []byte
	AttestationObject []byte
	AuthenticatorData []byte
	Signature         []byte
	CredentialID      []byte
	COSEKey           []byte
}

// Register builds an attestation-none create ceremony.
func (k *Key) Register(rpID, origin string, challenge []byte) (Ceremony, error) {
	cd, err := clientData("webauthn.create", origin, challenge, false)
	if err != nil {
		return Ceremony{}, err
	}
	auth := k.authData(rpID, true)
	obj := struct {
		Fmt      string         `cbor:"fmt"`
		AttStmt  map[string]any `cbor:"attStmt"`
		AuthData []byte         `cbor:"authData"`
	}{Fmt: "none", AttStmt: map[string]any{}, AuthData: auth}
	raw, err := cbor.Marshal(obj)
	if err != nil {
		return Ceremony{}, err
	}
	return Ceremony{
		ClientDataJSON:    cd,
		AttestationObject: raw,
		AuthenticatorData: auth,
		CredentialID:      append([]byte(nil), k.ID...),
		COSEKey:           append([]byte(nil), k.COSE...),
	}, nil
}

// Assert builds a get ceremony and increments the sign count when it is already
// non-zero, or when bump is true. A zero count stays zero unless bump is set,
// which is how a test moves off the synced-passkey case.
func (k *Key) Assert(rpID, origin string, challenge []byte, bump bool) (Ceremony, error) {
	if bump || k.SignCount != 0 {
		k.SignCount++
	}
	cd, err := clientData("webauthn.get", origin, challenge, false)
	if err != nil {
		return Ceremony{}, err
	}
	auth := k.authData(rpID, false)
	sig, err := k.sign(auth, cd)
	if err != nil {
		return Ceremony{}, err
	}
	return Ceremony{
		ClientDataJSON:    cd,
		AuthenticatorData: auth,
		Signature:         sig,
		CredentialID:      append([]byte(nil), k.ID...),
		COSEKey:           append([]byte(nil), k.COSE...),
	}, nil
}

// CreationResponse is the JSON a browser posts, for go-webauthn's parser.
func (c Ceremony) CreationResponse() ([]byte, error) {
	return json.Marshal(map[string]any{
		"id":    base64.RawURLEncoding.EncodeToString(c.CredentialID),
		"rawId": base64.RawURLEncoding.EncodeToString(c.CredentialID),
		"type":  "public-key",
		"response": map[string]string{
			"clientDataJSON":    base64.RawURLEncoding.EncodeToString(c.ClientDataJSON),
			"attestationObject": base64.RawURLEncoding.EncodeToString(c.AttestationObject),
		},
	})
}

// AssertionResponse is the JSON a browser posts, for go-webauthn's parser.
func (c Ceremony) AssertionResponse() ([]byte, error) {
	return json.Marshal(map[string]any{
		"id":    base64.RawURLEncoding.EncodeToString(c.CredentialID),
		"rawId": base64.RawURLEncoding.EncodeToString(c.CredentialID),
		"type":  "public-key",
		"response": map[string]string{
			"clientDataJSON":    base64.RawURLEncoding.EncodeToString(c.ClientDataJSON),
			"authenticatorData": base64.RawURLEncoding.EncodeToString(c.AuthenticatorData),
			"signature":         base64.RawURLEncoding.EncodeToString(c.Signature),
		},
	})
}

func (k *Key) authData(rpID string, registration bool) []byte {
	rpHash := sha256.Sum256([]byte(rpID))
	var flags byte = 0
	if k.userPresent {
		flags |= 1 << 0
	}
	if k.userVerified {
		flags |= 1 << 2
	}
	if k.backup {
		flags |= 1<<3 | 1<<4
	}
	buf := make([]byte, 37)
	copy(buf, rpHash[:])
	buf[32] = flags
	binary.BigEndian.PutUint32(buf[33:37], k.SignCount)
	if !registration {
		return buf
	}
	buf[32] |= 1 << 6
	buf = append(buf, make([]byte, 16)...) // aaguid
	var lenb [2]byte
	binary.BigEndian.PutUint16(lenb[:], uint16(len(k.ID)))
	buf = append(buf, lenb[:]...)
	buf = append(buf, k.ID...)
	buf = append(buf, k.COSE...)
	return buf
}

func (k *Key) sign(authData, clientDataJSON []byte) ([]byte, error) {
	sum := sha256.Sum256(clientDataJSON)
	signed := append(append([]byte{}, authData...), sum[:]...)
	switch k.Alg {
	case AlgES256:
		hash := sha256.Sum256(signed)
		return ecdsa.SignASN1(rand.Reader, k.priv.(*ecdsa.PrivateKey), hash[:])
	case AlgEdDSA:
		return ed25519.Sign(k.priv.(ed25519.PrivateKey), signed), nil
	case AlgRS256:
		hash := sha256.Sum256(signed)
		return rsa.SignPKCS1v15(rand.Reader, k.priv.(*rsa.PrivateKey), crypto.SHA256, hash[:])
	default:
		return nil, errUnsupported
	}
}

func clientData(typ, origin string, challenge []byte, cross bool) ([]byte, error) {
	return json.Marshal(struct {
		Type        string `json:"type"`
		Challenge   string `json:"challenge"`
		Origin      string `json:"origin"`
		CrossOrigin bool   `json:"crossOrigin"`
	}{
		Type:        typ,
		Challenge:   base64.RawURLEncoding.EncodeToString(challenge),
		Origin:      origin,
		CrossOrigin: cross,
	})
}

type coseEC2 struct {
	Kty int64  `cbor:"1,keyasint"`
	Alg int64  `cbor:"3,keyasint"`
	Crv int64  `cbor:"-1,keyasint"`
	X   []byte `cbor:"-2,keyasint"`
	Y   []byte `cbor:"-3,keyasint"`
}

type coseOKP struct {
	Kty int64  `cbor:"1,keyasint"`
	Alg int64  `cbor:"3,keyasint"`
	Crv int64  `cbor:"-1,keyasint"`
	X   []byte `cbor:"-2,keyasint"`
}

type coseRSA struct {
	Kty int64  `cbor:"1,keyasint"`
	Alg int64  `cbor:"3,keyasint"`
	N   []byte `cbor:"-1,keyasint"`
	E   []byte `cbor:"-2,keyasint"`
}

func marshalES256(pub *ecdsa.PublicKey) ([]byte, error) {
	x := pub.X.FillBytes(make([]byte, 32))
	y := pub.Y.FillBytes(make([]byte, 32))
	return cbor.Marshal(coseEC2{Kty: 2, Alg: int64(AlgES256), Crv: 1, X: x, Y: y})
}

func marshalEdDSA(pub ed25519.PublicKey) ([]byte, error) {
	return cbor.Marshal(coseOKP{Kty: 1, Alg: int64(AlgEdDSA), Crv: 6, X: pub})
}

func marshalRS256(pub *rsa.PublicKey) ([]byte, error) {
	n := pub.N.Bytes()
	if len(n) < 256 {
		padded := make([]byte, 256)
		copy(padded[256-len(n):], n)
		n = padded
	}
	e := []byte{0x01, 0x00, 0x01} // 65537
	return cbor.Marshal(coseRSA{Kty: 3, Alg: int64(AlgRS256), N: n, E: e})
}

type unsupportedError struct{}

func (unsupportedError) Error() string { return "unsupported" }

var errUnsupported = unsupportedError{}
