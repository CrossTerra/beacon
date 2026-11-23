package webauthn

import (
	"bytes"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"testing"

	"github.com/CrossTerra/beacon/internal/webauthntest"
)

func TestEd25519Point(t *testing.T) {
	pub, _, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	if !ed25519Point(pub) {
		t.Fatal("generated Ed25519 public key rejected")
	}
	// y = 2^255-1 is not a field element. The sign bit is separate.
	if ed25519Point(bytes.Repeat([]byte{0xff}, 32)) {
		t.Fatal("y >= p accepted")
	}
	if ed25519Point(make([]byte, 31)) {
		t.Fatal("short key accepted")
	}
}

func TestRegisterAndAssert(t *testing.T) {
	rp := RelyingParty{ID: "localhost", Origin: "http://localhost:7317"}
	for _, alg := range []webauthntest.Alg{webauthntest.AlgES256, webauthntest.AlgEdDSA, webauthntest.AlgRS256} {
		t.Run(alg.String(), func(t *testing.T) {
			key, err := webauthntest.Generate(alg)
			if err != nil {
				t.Fatal(err)
			}
			key.Backup(true)
			challenge := bytesOf(t, 32)
			reg, err := key.Register(rp.ID, rp.Origin, challenge)
			if err != nil {
				t.Fatal(err)
			}
			got, err := VerifyRegistration(rp, challenge, reg.ClientDataJSON, reg.AttestationObject)
			if err != nil {
				t.Fatal(err)
			}
			if Algorithm(alg) != got.Algorithm || got.SignCount != 0 || !got.BackupEligible || !got.BackupState {
				t.Fatalf("registration = %+v", got)
			}
			cred := Credential{ID: got.CredentialID, COSEKey: got.COSEKey, SignCount: got.SignCount}
			assertion, err := key.Assert(rp.ID, rp.Origin, challenge, false)
			if err != nil {
				t.Fatal(err)
			}
			as, err := VerifyAssertion(rp, challenge, cred, assertion.CredentialID, assertion.ClientDataJSON, assertion.AuthenticatorData, assertion.Signature)
			if err != nil {
				t.Fatal(err)
			}
			if as.SignCount != 0 {
				t.Fatalf("sign count %d, want 0", as.SignCount)
			}
		})
	}
}

func TestSignCount(t *testing.T) {
	rp := RelyingParty{ID: "localhost", Origin: "http://localhost:7317"}
	key, err := webauthntest.Generate(webauthntest.AlgES256)
	if err != nil {
		t.Fatal(err)
	}
	challenge := bytesOf(t, 32)
	regC, err := key.Register(rp.ID, rp.Origin, challenge)
	if err != nil {
		t.Fatal(err)
	}
	reg, err := VerifyRegistration(rp, challenge, regC.ClientDataJSON, regC.AttestationObject)
	if err != nil {
		t.Fatal(err)
	}
	cred := Credential{ID: reg.CredentialID, COSEKey: reg.COSEKey, SignCount: 0}
	// Both zero is a synced passkey.
	a, err := key.Assert(rp.ID, rp.Origin, challenge, false)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := VerifyAssertion(rp, challenge, cred, a.CredentialID, a.ClientDataJSON, a.AuthenticatorData, a.Signature); err != nil {
		t.Fatal(err)
	}
	// A later non-zero count must increase.
	key.SignCount = 1
	a, err = key.Assert(rp.ID, rp.Origin, challenge, false)
	if err != nil {
		t.Fatal(err)
	}
	// Assert with SignCount already 1 and bump false does not increment (see Assert).
	// Set the authenticator count by bumping from 0.
	key.SignCount = 0
	a, err = key.Assert(rp.ID, rp.Origin, challenge, true)
	if err != nil {
		t.Fatal(err)
	}
	got, err := VerifyAssertion(rp, challenge, cred, a.CredentialID, a.ClientDataJSON, a.AuthenticatorData, a.Signature)
	if err != nil {
		t.Fatal(err)
	}
	if got.SignCount != 1 {
		t.Fatalf("count %d", got.SignCount)
	}
	cred.SignCount = 1
	key.SignCount = 1
	a, err = key.Assert(rp.ID, rp.Origin, challenge, true)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := VerifyAssertion(rp, challenge, cred, a.CredentialID, a.ClientDataJSON, a.AuthenticatorData, a.Signature); err != nil {
		t.Fatal(err)
	}
	// Replay of that count, and a lower count, are refused.
	cred.SignCount = 2
	if _, err := VerifyAssertion(rp, challenge, cred, a.CredentialID, a.ClientDataJSON, a.AuthenticatorData, a.Signature); !isCode(err, CodeSignCount) {
		t.Fatalf("replay: %v", err)
	}
	cred.SignCount = 3
	if _, err := VerifyAssertion(rp, challenge, cred, a.CredentialID, a.ClientDataJSON, a.AuthenticatorData, a.Signature); !isCode(err, CodeSignCount) {
		t.Fatalf("lower: %v", err)
	}
}

func TestRefusals(t *testing.T) {
	rp := RelyingParty{ID: "localhost", Origin: "http://localhost:7317"}
	key, err := webauthntest.Generate(webauthntest.AlgES256)
	if err != nil {
		t.Fatal(err)
	}
	challenge := bytesOf(t, 32)
	regC, err := key.Register(rp.ID, rp.Origin, challenge)
	if err != nil {
		t.Fatal(err)
	}
	reg, err := VerifyRegistration(rp, challenge, regC.ClientDataJSON, regC.AttestationObject)
	if err != nil {
		t.Fatal(err)
	}
	cred := Credential{ID: reg.CredentialID, COSEKey: reg.COSEKey}

	t.Run("wrong origin", func(t *testing.T) {
		other := RelyingParty{ID: "localhost", Origin: "http://127.0.0.1:7317"}
		_, err := VerifyRegistration(other, challenge, regC.ClientDataJSON, regC.AttestationObject)
		if !isCode(err, CodeOrigin) {
			t.Fatal(err)
		}
	})
	t.Run("wrong challenge", func(t *testing.T) {
		_, err := VerifyRegistration(rp, bytesOf(t, 32), regC.ClientDataJSON, regC.AttestationObject)
		if !isCode(err, CodeChallenge) {
			t.Fatal(err)
		}
	})
	t.Run("missing UV", func(t *testing.T) {
		key.Flags(true, false)
		c, err := key.Register(rp.ID, rp.Origin, challenge)
		if err != nil {
			t.Fatal(err)
		}
		_, err = VerifyRegistration(rp, challenge, c.ClientDataJSON, c.AttestationObject)
		if !isCode(err, CodeUserVerified) {
			t.Fatal(err)
		}
		key.Flags(true, true)
	})
	t.Run("bad signature", func(t *testing.T) {
		a, err := key.Assert(rp.ID, rp.Origin, challenge, false)
		if err != nil {
			t.Fatal(err)
		}
		a.Signature[0] ^= 0xff
		_, err = VerifyAssertion(rp, challenge, cred, a.CredentialID, a.ClientDataJSON, a.AuthenticatorData, a.Signature)
		if !isCode(err, CodeSignature) {
			t.Fatal(err)
		}
	})
	t.Run("truncated auth data", func(t *testing.T) {
		_, err := VerifyAssertion(rp, challenge, cred, cred.ID, regC.ClientDataJSON, []byte{1, 2, 3}, []byte{1})
		if !isCode(err, CodeAuthData) && !isCode(err, CodeClientData) && !isCode(err, CodeSignature) {
			// client data is a create ceremony, so type fails first if we get that far.
			// Size check on authenticator data runs before client data. Expect auth_data.
			t.Fatal(err)
		}
		_, err = VerifyAssertion(rp, challenge, cred, cred.ID, mustGetClient(t, key, rp, challenge), []byte{1, 2, 3}, []byte{9})
		if !isCode(err, CodeAuthData) {
			t.Fatal(err)
		}
	})
	t.Run("malformed cbor", func(t *testing.T) {
		_, err := VerifyRegistration(rp, challenge, regC.ClientDataJSON, []byte{0xff, 0x00})
		if !isCode(err, CodeAttestation) {
			t.Fatal(err)
		}
	})
	t.Run("off curve", func(t *testing.T) {
		cose := append([]byte(nil), reg.COSEKey...)
		// Flip a coordinate byte inside the COSE key. Parsing must refuse, not panic.
		if len(cose) < 8 {
			t.Fatal("short cose")
		}
		cose[len(cose)-1] ^= 0x01
		if _, err := ParseCOSEKey(cose); err == nil {
			// A one-bit flip can theoretically land on the curve. Flip more.
			for i := range cose {
				cose[i] ^= 0xff
			}
			if _, err := ParseCOSEKey(cose); err == nil {
				t.Fatal("mangled key accepted")
			}
		}
	})
	t.Run("cross origin", func(t *testing.T) {
		raw := []byte(`{"type":"webauthn.create","challenge":"` + challengeB64(challenge) + `","origin":"http://localhost:7317","crossOrigin":true}`)
		_, err := VerifyRegistration(rp, challenge, raw, regC.AttestationObject)
		if !isCode(err, CodeCrossOrigin) {
			t.Fatal(err)
		}
	})
}

func TestMalformedDoesNotPanic(t *testing.T) {
	rp := RelyingParty{ID: "localhost", Origin: "http://localhost:7317"}
	challenge := bytesOf(t, 32)
	inputs := [][]byte{
		nil,
		{},
		{0x00},
		{0xa0},
		{0xbf, 0xff},
		bytesOf(t, 64),
		[]byte(`{"fmt":"none"}`),
	}
	for _, in := range inputs {
		func() {
			defer func() {
				if r := recover(); r != nil {
					t.Fatalf("panic on %x: %v", in, r)
				}
			}()
			_, _ = VerifyRegistration(rp, challenge, []byte(`{"type":"webauthn.create","challenge":"`+challengeB64(challenge)+`","origin":"http://localhost:7317"}`), in)
			_, _ = ParseCOSEKey(in)
		}()
	}
}

func mustGetClient(t *testing.T, key *webauthntest.Key, rp RelyingParty, challenge []byte) []byte {
	t.Helper()
	a, err := key.Assert(rp.ID, rp.Origin, challenge, false)
	if err != nil {
		t.Fatal(err)
	}
	return a.ClientDataJSON
}

func challengeB64(challenge []byte) string {
	return base64.RawURLEncoding.EncodeToString(challenge)
}

func bytesOf(t *testing.T, n int) []byte {
	t.Helper()
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		t.Fatal(err)
	}
	return b
}

func isCode(err error, code string) bool {
	var e *Error
	return errors.As(err, &e) && e.Code == code
}
