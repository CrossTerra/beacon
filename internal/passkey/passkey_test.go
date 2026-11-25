package passkey

import (
	"bytes"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
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
	}, Ceremony{PRFOutput: bytes.Clone(prf)}, now); err != ErrPRF {
		t.Fatalf("missing PRF: %v", err)
	}
	j := noteVault(t, dir)
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
	if err := j.Close(); err != nil {
		t.Fatal(err)
	}
	if _, _, _, err := Begin(dir, rp, code, now); err != ErrRegistered {
		t.Fatalf("second passkey: %v", err)
	}
}

func TestOpenVaultRejectsAForgedWrapAndAMissingVault(t *testing.T) {
	dir, rp, prf, id := register(t)
	noteVault(t, dir)
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

func TestSwappedVaultIsAJournalledReset(t *testing.T) {
	dir, _, prf, id := register(t)
	noteVault(t, dir)
	forged := bytes.Repeat([]byte{0x22}, 32)
	wrapped, data, err := wrapKey(forged, bytes.Repeat([]byte{0x33}, 32), []byte(approval.LocalOrigin))
	if err != nil {
		t.Fatal(err)
	}
	clear(data)
	if err := os.WriteFile(filepath.Join(dir, writeVaultName), []byte("swapped-vault"), 0o600); err != nil {
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
	if _, err := OpenVault(dir, id, append([]byte(nil), forged...), approval.LocalOrigin); err != ErrReset {
		t.Fatalf("swapped vault: %v", err)
	}
	journal, err := os.ReadFile(filepath.Join(dir, "journal.log"))
	if err != nil || !bytes.Contains(journal, []byte("vault_mismatch")) {
		t.Fatal("mismatch was not journalled")
	}
	_ = prf
}

func TestOpenVaultAuthenticatesOneSnapshotAgainstLiveEnrollment(t *testing.T) {
	dir, rp, prf, id := register(t)
	defer clear(prf)
	noteVault(t, dir)
	path := filepath.Join(dir, writeVaultName)
	// The journal's open handle and replayed enrollment still belong to the service.
	if err := os.Rename(filepath.Join(dir, "journal.log"), filepath.Join(dir, "live.log")); err != nil {
		t.Fatal(err)
	}
	reads := 0
	key, err := openVault(dir, id, bytes.Clone(prf), rp.Origin, func(dir string) ([]byte, error) {
		reads++
		body, err := readVault(dir)
		if err != nil {
			return nil, err
		}
		// Change the path only after taking the snapshot. Neither decrypt nor
		// enrollment verification should reopen a file to validate those bytes.
		return body, os.WriteFile(path, []byte(`{"status":"replaced"}`), 0o600)
	})
	defer clear(key)
	if err != nil || len(key) != keyBytes || reads != 1 {
		t.Fatalf("did not validate the enrolled snapshot: reads=%d err=%v", reads, err)
	}
	if _, err := OpenVault(dir, id, bytes.Clone(prf), rp.Origin); err != ErrReset {
		t.Fatalf("replacement was not refused by the live journal: %v", err)
	}
}

func TestUnrecordedVaultIsDebris(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "state")
	now := time.Unix(1_800_000_000, 0).UTC()
	if err := approval.PrivateDir(dir); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, writeVaultName), []byte("debris"), 0o600); err != nil {
		t.Fatal(err)
	}
	code, err := Issue(dir, now)
	if err != nil {
		t.Fatal(err)
	}
	rp := webauthn.RelyingParty{ID: "localhost", Origin: approval.LocalOrigin}
	if _, _, _, err := Begin(dir, rp, code, now); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(filepath.Join(dir, writeVaultName)); !os.IsNotExist(err) {
		t.Fatal("debris vault was kept")
	}
}

func noteVault(t *testing.T, dir string) *approval.Journal {
	t.Helper()
	j, err := approval.OpenJournal(dir, time.Unix(1_800_000_000, 0).UTC(), "")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { j.Close() })
	return j
}

func TestDeletedPasskeyListDoesNotBypassReset(t *testing.T) {
	dir, rp, _, _ := register(t)
	noteVault(t, dir).Close()
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
	// The reset is already later in the journal, even though the clock has not moved.
	code, err := Issue(dir, now)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, _, err := Begin(dir, rp, code, now); err != nil {
		t.Fatal(err)
	}
}

func TestJournalResetBindsTheCodeWithoutTheMarker(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "state")
	now := time.Unix(1_800_000_000, 0).UTC()
	j, err := approval.OpenJournal(dir, now, "")
	if err != nil {
		t.Fatal(err)
	}
	if err := j.RecordLocal("passkeys_reset", now); err != nil {
		t.Fatal(err)
	}
	j.Close()
	code, err := Issue(dir, now)
	if err != nil {
		t.Fatal(err)
	}
	rp := webauthn.RelyingParty{ID: "localhost", Origin: approval.LocalOrigin}
	createCh, assertCh, _, err := Begin(dir, rp, code, now)
	if err != nil {
		t.Fatal(err)
	}
	pending, err := os.ReadFile(filepath.Join(dir, "register-pending.json"))
	if err != nil || !bytes.Contains(pending, []byte(`"reset_index":`)) {
		t.Fatalf("pending was not bound to the journal reset: %s %v", pending, err)
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
	noteVault(t, dir)
	if err := Finish(dir, rp, Ceremony{
		ClientDataJSON: created.ClientDataJSON, AttestationObject: created.AttestationObject, PRFEnabled: true,
	}, Ceremony{
		ClientDataJSON: asserted.ClientDataJSON, AuthenticatorData: asserted.AuthenticatorData,
		Signature: asserted.Signature, CredentialID: asserted.CredentialID, PRFOutput: bytes.Repeat([]byte{0x11}, 32),
	}, now); err != nil {
		t.Fatal(err)
	}
}

func TestPendingRegistrationCannotFinishAfterReset(t *testing.T) {
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
	pending, err := os.ReadFile(filepath.Join(dir, "register-pending.json"))
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
	j, err := approval.OpenJournal(dir, now, "")
	if err != nil {
		t.Fatal(err)
	}
	if err := Reset(dir, now, j.RecordLocal); err != nil {
		t.Fatal(err)
	}
	j.Close()
	if _, err := os.Lstat(filepath.Join(dir, "register-pending.json")); !os.IsNotExist(err) {
		t.Fatal("reset left the pending registration")
	}
	if err := os.WriteFile(filepath.Join(dir, "register-pending.json"), pending, 0o600); err != nil {
		t.Fatal(err)
	}
	noteVault(t, dir)
	err = Finish(dir, rp, Ceremony{
		ClientDataJSON: created.ClientDataJSON, AttestationObject: created.AttestationObject, PRFEnabled: true,
	}, Ceremony{
		ClientDataJSON: asserted.ClientDataJSON, AuthenticatorData: asserted.AuthenticatorData,
		Signature: asserted.Signature, CredentialID: asserted.CredentialID, PRFOutput: bytes.Repeat([]byte{0x11}, 32),
	}, now)
	if err != ErrRegistered {
		t.Fatalf("finished a ceremony from before the reset: %v", err)
	}
	if _, err := os.Lstat(filepath.Join(dir, writeVaultName)); !os.IsNotExist(err) {
		t.Fatal("refused finish still published a vault")
	}
}

func TestFinishRefusesASecondActiveEnrollmentEvenWithoutItsFiles(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "state")
	now := time.Unix(1_800_000_000, 0).UTC()
	code, err := Issue(dir, now)
	if err != nil {
		t.Fatal(err)
	}
	noteVault(t, dir)
	rp := webauthn.RelyingParty{ID: "localhost", Origin: approval.LocalOrigin}
	create, assert, _, err := Begin(dir, rp, code, now)
	if err != nil {
		t.Fatal(err)
	}
	key, err := webauthntest.Generate(webauthntest.AlgES256)
	if err != nil {
		t.Fatal(err)
	}
	c, err := key.Register(rp.ID, rp.Origin, create)
	if err != nil {
		t.Fatal(err)
	}
	a, err := key.Assert(rp.ID, rp.Origin, assert, false)
	if err != nil {
		t.Fatal(err)
	}
	pendingPath := filepath.Join(dir, "register-pending.json")
	pending, err := os.ReadFile(pendingPath)
	if err != nil {
		t.Fatal(err)
	}
	finish := func() error {
		return Finish(dir, rp, Ceremony{ClientDataJSON: c.ClientDataJSON, AttestationObject: c.AttestationObject, PRFEnabled: true},
			Ceremony{ClientDataJSON: a.ClientDataJSON, AuthenticatorData: a.AuthenticatorData, Signature: a.Signature,
				CredentialID: a.CredentialID, PRFOutput: bytes.Repeat([]byte{0x11}, 32)}, now)
	}
	if err := finish(); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{writeVaultName, "passkeys.json"} {
		if err := os.Remove(filepath.Join(dir, name)); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(pendingPath, pending, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := finish(); err != ErrRegistered {
		t.Fatalf("a second enrollment reached publication: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, writeVaultName)); !os.IsNotExist(err) {
		t.Fatal("refused enrollment recreated the vault")
	}
}

func TestFinishCanRetryAfterPublishFails(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "state")
	now := time.Unix(1_800_000_000, 0).UTC()
	code, err := Issue(dir, now)
	if err != nil {
		t.Fatal(err)
	}
	noteVault(t, dir)
	rp := webauthn.RelyingParty{ID: "localhost", Origin: approval.LocalOrigin}
	create, assert, _, err := Begin(dir, rp, code, now)
	if err != nil {
		t.Fatal(err)
	}
	key, err := webauthntest.Generate(webauthntest.AlgES256)
	if err != nil {
		t.Fatal(err)
	}
	c, err := key.Register(rp.ID, rp.Origin, create)
	if err != nil {
		t.Fatal(err)
	}
	a, err := key.Assert(rp.ID, rp.Origin, assert, false)
	if err != nil {
		t.Fatal(err)
	}
	finish := func() error {
		return Finish(dir, rp, Ceremony{ClientDataJSON: c.ClientDataJSON, AttestationObject: c.AttestationObject, PRFEnabled: true},
			Ceremony{ClientDataJSON: a.ClientDataJSON, AuthenticatorData: a.AuthenticatorData, Signature: a.Signature,
				CredentialID: a.CredentialID, PRFOutput: bytes.Repeat([]byte{0x11}, 32)}, now)
	}

	// Occupy the second publication's destination so the vault has to roll back.
	credentials := filepath.Join(dir, "passkeys.json")
	if err := os.WriteFile(credentials, []byte("occupied"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := finish(); !errors.Is(err, ErrRegistered) {
		t.Fatalf("expected publication to fail: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, writeVaultName)); !os.IsNotExist(err) {
		t.Fatalf("failed publication left a vault: %v", err)
	}
	if _, active, err := approval.PasskeyVault(dir); err != nil || active {
		t.Fatalf("failed publication committed an enrollment: active=%v, err=%v", active, err)
	}
	if err := os.Remove(credentials); err != nil {
		t.Fatal(err)
	}
	if err := finish(); err != nil {
		t.Fatalf("could not retry the pending ceremony: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "register-pending.json")); !os.IsNotExist(err) {
		t.Fatalf("successful enrollment did not remove the pending file: %v", err)
	}
	vaultKey, err := OpenVault(dir, a.CredentialID, bytes.Repeat([]byte{0x11}, 32), rp.Origin)
	defer clear(vaultKey)
	if err != nil || len(vaultKey) != keyBytes {
		t.Fatalf("retry did not enroll a usable passkey: %v", err)
	}
}

func TestEarlierClockDoesNotHideALaterReset(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "state")
	early := time.Unix(1_800_000_000, 0).UTC()
	j, err := approval.OpenJournal(dir, early, "")
	if err != nil {
		t.Fatal(err)
	}
	if err := approval.EnrollPasskey(dir, 0, early.Add(time.Hour), []byte("sealed fixture"), func([]byte) error { return nil }, func() error { return nil }); err != nil {
		t.Fatal(err)
	}
	if err := j.RecordLocal("passkeys_reset", early); err != nil {
		t.Fatal(err)
	}
	j.Close()
	if _, active, err := approval.PasskeyVault(dir); err != nil || active {
		t.Fatalf("active %v %v", active, err)
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
	j := noteVault(t, dir)
	prf := bytes.Repeat([]byte{0x11}, 32)
	if err := Finish(dir, rp, Ceremony{
		ClientDataJSON: created.ClientDataJSON, AttestationObject: created.AttestationObject, PRFEnabled: true,
	}, Ceremony{
		ClientDataJSON: asserted.ClientDataJSON, AuthenticatorData: asserted.AuthenticatorData,
		Signature: asserted.Signature, CredentialID: asserted.CredentialID, PRFOutput: append([]byte(nil), prf...),
	}, now); err != nil {
		t.Fatal(err)
	}
	if err := j.Close(); err != nil {
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
