package passkey

import (
	"bytes"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
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

func TestOpenVaultRejectsAForgedWrapAndAMissingVault(t *testing.T) {
	dir, rp, prf, id := register(t)
	key, err := OpenVault(dir, id, append([]byte(nil), prf...), approval.LocalOrigin)
	if err != nil || len(key) != 32 {
		t.Fatal(err)
	}
	clear(key)
	forged := bytes.Repeat([]byte{0x22}, 32)
	wrapped, _, err := wrapKey(forged, bytes.Repeat([]byte{0x33}, 32), []byte(approval.LocalOrigin))
	if err != nil {
		t.Fatal(err)
	}
	body, err := os.ReadFile(filepath.Join(dir, "passkeys.json"))
	if err != nil {
		t.Fatal(err)
	}
	var stored storeFile
	if err := json.Unmarshal(body, &stored); err != nil {
		t.Fatal(err)
	}
	stored.Credentials[0].Wrap = wrapped
	stored.Credentials[0].Salt = bytes.Repeat([]byte{0x33}, 32)
	next, err := json.Marshal(stored)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "passkeys.json"), next, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := OpenVault(dir, id, append([]byte(nil), forged...), approval.LocalOrigin); err != ErrVault {
		t.Fatalf("forged wrap opened the vault: %v", err)
	}
	other, _, _, otherID := register(t)
	if err := os.Remove(filepath.Join(other, writeVaultName)); err != nil {
		t.Fatal(err)
	}
	if _, err := OpenVault(other, otherID, bytes.Repeat([]byte{0x11}, 32), approval.LocalOrigin); err != ErrVault {
		t.Fatalf("missing vault: %v", err)
	}
	_ = rp
}

func TestDeletedPasskeyListDoesNotBypassReset(t *testing.T) {
	dir, rp, _, _ := register(t)
	if err := os.Remove(filepath.Join(dir, "passkeys.json")); err != nil {
		t.Fatal(err)
	}
	if _, err := Issue(dir, time.Unix(1_800_000_000, 0).UTC()); err != ErrRegistered {
		t.Fatalf("issued a code while the write vault exists: %v", err)
	}
	raw := bytes.Repeat([]byte{0x44}, 32)
	sum := sha256.Sum256(raw)
	body, err := json.Marshal(codeFile{Hash: sum[:], Expires: time.Unix(1_800_000_100, 0).UTC(), IssuedAt: time.Unix(1_800_000_000, 0).UTC()})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "setup-code.json"), body, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, _, _, err := Begin(dir, rp, base64.RawURLEncoding.EncodeToString(raw), time.Unix(1_800_000_000, 0).UTC()); err != ErrRegistered {
		t.Fatalf("registration beside a write vault: %v", err)
	}
}

func TestResetCodeMustFollowTheJournalReset(t *testing.T) {
	dir, rp, _, _ := register(t)
	now := time.Unix(1_800_000_000, 0).UTC()
	j, err := approval.OpenJournal(dir, now, "")
	if err != nil {
		t.Fatal(err)
	}
	if err := Reset(dir, now, j.RecordLocal); err != nil {
		t.Fatal(err)
	}
	j.Close()
	if _, err := Issue(dir, now); err != ErrRegistered {
		t.Fatalf("code issued at the reset instant: %v", err)
	}
	later := now.Add(time.Second)
	code, err := Issue(dir, later)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, _, err := Begin(dir, rp, code, later); err != nil {
		t.Fatal(err)
	}
}

func TestRegistrationRefusesAnotherProcessLock(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "state")
	if err := approval.PrivateDir(dir); err != nil {
		t.Fatal(err)
	}
	lock, err := approval.LockInstance(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer approval.CloseInstance(dir, lock)
	approval.DropHeld(dir)
	if _, _, _, err := Begin(dir, webauthn.RelyingParty{ID: "localhost", Origin: approval.LocalOrigin}, "x", time.Now()); err != approval.ErrLocked {
		t.Fatalf("registration beside the service: %v", err)
	}
}

func TestSignCountIsStoredBeforeItCanBeReplayed(t *testing.T) {
	dir, _, _, id := register(t)
	if err := NoteSignCount(dir, id, 1); err != nil {
		t.Fatal(err)
	}
	if err := NoteSignCount(dir, id, 1); err != ErrSignCount {
		t.Fatalf("same count: %v", err)
	}
	if err := NoteSignCount(dir, id, 2); err != nil {
		t.Fatal(err)
	}
	creds, err := List(dir)
	if err != nil || creds[0].SignCount != 2 {
		t.Fatalf("stored %v %v", creds, err)
	}
}

func register(t *testing.T) (string, webauthn.RelyingParty, []byte, []byte) {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "state")
	now := time.Unix(1_800_000_000, 0).UTC()
	code, err := Issue(dir, now)
	if err != nil {
		t.Fatal(err)
	}
	rp := webauthn.RelyingParty{ID: "localhost", Origin: approval.LocalOrigin}
	createCh, assertCh, _, err := Begin(dir, rp, code, now)
	if err != nil {
		t.Fatal(err)
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
		ClientDataJSON: created.ClientDataJSON, AttestationObject: created.AttestationObject, PRFEnabled: true,
	}, Ceremony{
		ClientDataJSON: asserted.ClientDataJSON, AuthenticatorData: asserted.AuthenticatorData,
		Signature: asserted.Signature, CredentialID: asserted.CredentialID, PRFOutput: append([]byte(nil), prf...),
	}, now); err != nil {
		t.Fatal(err)
	}
	return dir, rp, prf, asserted.CredentialID
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
	defer approval.CloseInstance(dir, lock)
	if _, err := Issue(dir, time.Now()); err != approval.ErrLocked {
		t.Fatalf("setup beside the service: %v", err)
	}
}
