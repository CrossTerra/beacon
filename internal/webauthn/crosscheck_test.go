package webauthn

import (
	"encoding/base64"
	"testing"

	"github.com/go-webauthn/webauthn/protocol"
	"github.com/go-webauthn/webauthn/protocol/webauthncose"
	goweb "github.com/go-webauthn/webauthn/webauthn"

	"github.com/CrossTerra/beacon/internal/webauthntest"
)

// TestCrossCheck requires the same accept or refuse from Beacon's verifier and
// from go-webauthn, on valid ceremonies and on ceremonies broken one field at
// a time. Sign count is the exception the plan requires: go-webauthn only sets
// a clone warning, and Beacon refuses (13.01-D22).
func TestCrossCheck(t *testing.T) {
	rp := RelyingParty{ID: "localhost", Origin: "http://localhost:7317"}
	params := []protocol.CredentialParameter{
		{Type: protocol.PublicKeyCredentialType, Algorithm: webauthncose.AlgES256},
		{Type: protocol.PublicKeyCredentialType, Algorithm: webauthncose.AlgEdDSA},
		{Type: protocol.PublicKeyCredentialType, Algorithm: webauthncose.AlgRS256},
	}
	for _, alg := range []webauthntest.Alg{webauthntest.AlgES256, webauthntest.AlgEdDSA, webauthntest.AlgRS256} {
		t.Run(alg.String(), func(t *testing.T) {
			key, err := webauthntest.Generate(alg)
			if err != nil {
				t.Fatal(err)
			}
			challenge := bytesOf(t, 32)
			reg, err := key.Register(rp.ID, rp.Origin, challenge)
			if err != nil {
				t.Fatal(err)
			}
			body, err := reg.CreationResponse()
			if err != nil {
				t.Fatal(err)
			}
			if _, err := VerifyRegistration(rp, challenge, reg.ClientDataJSON, reg.AttestationObject); err != nil {
				t.Fatal(err)
			}
			if err := libraryRegister(body, challenge, rp, params); err != nil {
				t.Fatal(err)
			}

			assertion, err := key.Assert(rp.ID, rp.Origin, challenge, true)
			if err != nil {
				t.Fatal(err)
			}
			cred := Credential{ID: key.ID, COSEKey: key.COSE, SignCount: 0}
			if _, err := VerifyAssertion(rp, challenge, cred, assertion.CredentialID, assertion.ClientDataJSON, assertion.AuthenticatorData, assertion.Signature); err != nil {
				t.Fatal(err)
			}
			abody, err := assertion.AssertionResponse()
			if err != nil {
				t.Fatal(err)
			}
			if err := libraryAssert(abody, challenge, rp, key.COSE); err != nil {
				t.Fatal(err)
			}

			t.Run("wrong origin", func(t *testing.T) {
				other := RelyingParty{ID: rp.ID, Origin: "http://127.0.0.1:7317"}
				_, ours := VerifyRegistration(other, challenge, reg.ClientDataJSON, reg.AttestationObject)
				theirs := libraryRegister(body, challenge, other, params)
				if (ours == nil) != (theirs == nil) {
					t.Fatalf("ours %v, theirs %v", ours, theirs)
				}
			})
			t.Run("wrong challenge", func(t *testing.T) {
				bad := bytesOf(t, 32)
				_, ours := VerifyRegistration(rp, bad, reg.ClientDataJSON, reg.AttestationObject)
				theirs := libraryRegister(body, bad, rp, params)
				if (ours == nil) != (theirs == nil) {
					t.Fatalf("ours %v, theirs %v", ours, theirs)
				}
			})
			t.Run("missing UV", func(t *testing.T) {
				key.Flags(true, false)
				broken, err := key.Register(rp.ID, rp.Origin, challenge)
				if err != nil {
					t.Fatal(err)
				}
				raw, err := broken.CreationResponse()
				if err != nil {
					t.Fatal(err)
				}
				_, ours := VerifyRegistration(rp, challenge, broken.ClientDataJSON, broken.AttestationObject)
				theirs := libraryRegister(raw, challenge, rp, params)
				if ours == nil || theirs == nil {
					t.Fatalf("ours %v, theirs %v", ours, theirs)
				}
				key.Flags(true, true)
			})
			t.Run("bad signature", func(t *testing.T) {
				broken := assertion
				broken.Signature = append([]byte(nil), assertion.Signature...)
				broken.Signature[0] ^= 0xff
				raw, err := broken.AssertionResponse()
				if err != nil {
					t.Fatal(err)
				}
				_, ours := VerifyAssertion(rp, challenge, cred, broken.CredentialID, broken.ClientDataJSON, broken.AuthenticatorData, broken.Signature)
				theirs := libraryAssert(raw, challenge, rp, key.COSE)
				if ours == nil || theirs == nil {
					t.Fatalf("ours %v, theirs %v", ours, theirs)
				}
			})
			t.Run("malformed", func(t *testing.T) {
				_, ours := VerifyRegistration(rp, challenge, reg.ClientDataJSON, []byte{0xff})
				theirs := libraryRegister([]byte("{"), challenge, rp, params)
				if ours == nil || theirs == nil {
					t.Fatalf("ours %v, theirs %v", ours, theirs)
				}
			})
		})
	}
}

func TestCrossCheckSignCount(t *testing.T) {
	// go-webauthn records a clone warning and still returns the credential.
	// Beacon refuses. Both must notice; Beacon's refusal is the plan.
	stored := []uint32{0, 0, 1, 2, 5}
	got := []uint32{0, 1, 1, 1, 0}
	wantRefuse := []bool{false, false, true, true, true}
	for i := range stored {
		a := goweb.Authenticator{SignCount: stored[i]}
		a.UpdateCounter(got[i])
		ours := checkSignCount(stored[i], got[i])
		if (ours != nil) != wantRefuse[i] || a.CloneWarning != wantRefuse[i] {
			t.Fatalf("stored %d got %d: ours %v clone %v", stored[i], got[i], ours, a.CloneWarning)
		}
	}
}

func libraryRegister(body, challenge []byte, rp RelyingParty, params []protocol.CredentialParameter) error {
	parsed, err := protocol.ParseCredentialCreationResponseBytes(body)
	if err != nil {
		return err
	}
	_, err = parsed.Verify(
		base64.RawURLEncoding.EncodeToString(challenge),
		true,
		true,
		rp.ID,
		[]string{rp.Origin},
		nil,
		protocol.TopOriginIgnoreVerificationMode,
		nil,
		params,
	)
	return err
}

func libraryAssert(body, challenge []byte, rp RelyingParty, cose []byte) error {
	parsed, err := protocol.ParseCredentialRequestResponseBytes(body)
	if err != nil {
		return err
	}
	return parsed.Verify(
		base64.RawURLEncoding.EncodeToString(challenge),
		rp.ID,
		[]string{rp.Origin},
		nil,
		protocol.TopOriginIgnoreVerificationMode,
		"",
		true,
		true,
		cose,
	)
}
