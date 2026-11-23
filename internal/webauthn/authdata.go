package webauthn

import (
	"bytes"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/binary"

	"github.com/fxamacker/cbor/v2"
)

const (
	flagUP   = 1 << 0
	flagRFU1 = 1 << 1
	flagUV   = 1 << 2
	flagBE   = 1 << 3
	flagBS   = 1 << 4
	flagRFU2 = 1 << 5
	flagAT   = 1 << 6
	flagED   = 1 << 7
)

type authData struct {
	signCount      uint32
	credentialID   []byte
	aaguid         [16]byte
	backupEligible bool
	backupState    bool
}

type attestationObject struct {
	Fmt      string          `cbor:"fmt"`
	AttStmt  cbor.RawMessage `cbor:"attStmt"`
	AuthData []byte          `cbor:"authData"`
}

func parseAttestation(raw []byte, rpID string) (authData, []byte, error) {
	var obj attestationObject
	rest, err := unmarshalOne(raw, &obj)
	if err != nil || len(rest) != 0 {
		return authData{}, nil, errCode(CodeAttestation)
	}
	// Attestation is not checked (D22). Only none is accepted, and its
	// statement must be an empty map: synced passkeys send that, and anything
	// else would be a statement we had decided not to read.
	if obj.Fmt != "none" || !bytes.Equal(obj.AttStmt, []byte{0xa0}) {
		return authData{}, nil, errCode(CodeAttestation)
	}
	auth, cose, err := parseAuthData(obj.AuthData, rpID, true)
	if err != nil {
		return authData{}, nil, err
	}
	return auth, cose, nil
}

func parseAssertionAuthData(raw []byte, rpID string) (authData, error) {
	auth, _, err := parseAuthData(raw, rpID, false)
	return auth, err
}

func parseAuthData(raw []byte, rpID string, registration bool) (authData, []byte, error) {
	if len(raw) < minAuthData || len(raw) > maxAuthData {
		return authData{}, nil, errCode(CodeAuthData)
	}
	want := sha256.Sum256([]byte(rpID))
	if subtleBytes(raw[:32], want[:]) != 1 {
		return authData{}, nil, errCode(CodeRPID)
	}
	flags := raw[32]
	if flags&flagRFU1 != 0 || flags&flagRFU2 != 0 {
		return authData{}, nil, errCode(CodeFlags)
	}
	if flags&flagUP == 0 {
		return authData{}, nil, errCode(CodeUserPresent)
	}
	if flags&flagUV == 0 {
		return authData{}, nil, errCode(CodeUserVerified)
	}
	be := flags&flagBE != 0
	bs := flags&flagBS != 0
	// WebAuthn: backup state set without backup eligibility is not a credential.
	if bs && !be {
		return authData{}, nil, errCode(CodeFlags)
	}
	out := authData{
		signCount:      binary.BigEndian.Uint32(raw[33:37]),
		backupEligible: be,
		backupState:    bs,
	}
	rest := raw[37:]
	if registration {
		if flags&flagAT == 0 {
			return authData{}, nil, errCode(CodeAuthData)
		}
		cose, next, err := parseAttested(rest, &out)
		if err != nil {
			return authData{}, nil, err
		}
		rest = next
		if err := parseExtensions(flags, rest); err != nil {
			return authData{}, nil, err
		}
		return out, cose, nil
	}
	if flags&flagAT != 0 {
		// A get ceremony does not carry attested credential data.
		return authData{}, nil, errCode(CodeAuthData)
	}
	if err := parseExtensions(flags, rest); err != nil {
		return authData{}, nil, err
	}
	return out, nil, nil
}

func parseAttested(rest []byte, out *authData) ([]byte, []byte, error) {
	if len(rest) < 18 {
		return nil, nil, errCode(CodeAuthData)
	}
	copy(out.aaguid[:], rest[:16])
	idLen := int(binary.BigEndian.Uint16(rest[16:18]))
	if idLen == 0 || idLen > maxCredentialID {
		return nil, nil, errCode(CodeCredential)
	}
	if len(rest) < 18+idLen {
		return nil, nil, errCode(CodeAuthData)
	}
	out.credentialID = append([]byte(nil), rest[18:18+idLen]...)
	after := rest[18+idLen:]
	var discard any
	tail, err := unmarshalOne(after, &discard)
	if err != nil || len(after) == len(tail) {
		return nil, nil, errCode(CodePublicKey)
	}
	cose := after[:len(after)-len(tail)]
	return cose, tail, nil
}

func parseExtensions(flags byte, rest []byte) error {
	if flags&flagED == 0 {
		if len(rest) != 0 {
			return errCode(CodeAuthData)
		}
		return nil
	}
	if len(rest) == 0 {
		return errCode(CodeAuthData)
	}
	var ext map[string]cbor.RawMessage
	tail, err := unmarshalOne(rest, &ext)
	if err != nil || len(tail) != 0 || ext == nil {
		return errCode(CodeAuthData)
	}
	return nil
}

func subtleBytes(a, b []byte) int {
	if len(a) != len(b) {
		return 0
	}
	return subtle.ConstantTimeCompare(a, b)
}
