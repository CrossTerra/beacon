package passkey

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/CrossTerra/beacon/internal/approval"
	"github.com/CrossTerra/beacon/internal/webauthn"
	"github.com/CrossTerra/beacon/internal/webauthntest"
)

func TestSetupCodeRegistersOnePasskeyAndWrapsAVaultKey(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "state")
	now := time.Unix(1_800_000_000, 0).UTC()
	code, err := Issue(dir, now)
	if err != nil {
		t.Fatal(err)
	}
	rp := webauthn.RelyingParty{ID: "localhost", Origin: approval.LocalOrigin}
	createCh, assertCh, salt, err := Begin(dir, rp, code, now)
	if err != nil || len(salt) != 32 {
		t.Fatal(err)
	}
	if _, _, _, err := Begin(dir, rp, code, now); err != ErrSetup {
		t.Fatalf("setup code reused: %v", err)
	}
	key, err := webauthntest.Generate(webauthntest.AlgES256)
	if err != nil {
		t.Fatal(err)
	}
	created, err := key.Register("localhost", approval.LocalOrigin, createCh)
	if err != nil {
		t.Fatal(err)
	}
	asserted, err := key.Assert("localhost", approval.LocalOrigin, assertCh, false)
	if err != nil {
		t.Fatal(err)
	}
	prf := bytes.Repeat([]byte{0x11}, 32)
	if err := Finish(dir, rp, Ceremony{
		ClientDataJSON: created.ClientDataJSON, AttestationObject: created.AttestationObject, PRFEnabled: false,
	}, Ceremony{PRFOutput: prf}, now); err != ErrPRF {
		t.Fatalf("missing PRF: %v", err)
	}
	if err := Finish(dir, rp, Ceremony{
		ClientDataJSON: created.ClientDataJSON, AttestationObject: created.AttestationObject, PRFEnabled: true,
	}, Ceremony{
		ClientDataJSON: asserted.ClientDataJSON, AuthenticatorData: asserted.AuthenticatorData,
		Signature: asserted.Signature, CredentialID: asserted.CredentialID, PRFOutput: append([]byte(nil), prf...),
	}, now); err != nil {
		t.Fatal(err)
	}
	body, err := os.ReadFile(filepath.Join(dir, "passkeys.json"))
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(body, prf) {
		t.Fatal("PRF output was stored")
	}
	if _, err := os.Lstat(filepath.Join(dir, "register-pending.json")); !os.IsNotExist(err) {
		t.Fatal("pending registration was kept")
	}
	info, err := approval.CheckPrivate(filepath.Join(dir, "passkeys.json"), 0o600)
	if err != nil || info.Mode().Perm() != 0o600 {
		t.Fatal(err)
	}
	if _, _, _, err := Begin(dir, rp, code, now); err != ErrRegistered {
		t.Fatalf("second passkey: %v", err)
	}
}

func TestSetupRefusesALockedPreview(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "state")
	if err := approval.PrivateDir(dir); err != nil {
		t.Fatal(err)
	}
	lock, err := approval.LockInstance(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer lock.Close()
	if _, err := Issue(dir, time.Now()); err != approval.ErrLocked {
		t.Fatalf("setup beside the service: %v", err)
	}
}
