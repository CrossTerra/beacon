package main

import (
	"encoding/base64"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/CrossTerra/beacon/internal/approval"
	"github.com/CrossTerra/beacon/internal/passkey"
	"github.com/CrossTerra/beacon/internal/webauthn"
	"github.com/CrossTerra/beacon/internal/webauthntest"
)

func TestSetupPrintsAOneUseCode(t *testing.T) {
	dir := previewTemp(t)
	a, out, stderr := previewCLI()
	if code := a.main(t.Context(), []string{"setup", "--state-dir", dir}); code != 0 {
		t.Fatalf("setup exited %d: %s", code, stderr)
	}
	printed := strings.TrimSpace(out.String())
	raw, err := base64.RawURLEncoding.Strict().DecodeString(printed)
	if err != nil || len(raw) != 32 {
		t.Fatalf("code %q: %v", printed, err)
	}
	if code := a.main(t.Context(), []string{"setup"}); code == 0 {
		t.Fatal("setup without a directory was accepted")
	}
}

func TestResetPasskeysThenRegister(t *testing.T) {
	dir := previewTemp(t)
	now := time.Now().UTC()
	code, err := passkey.Issue(dir, now)
	if err != nil {
		t.Fatal(err)
	}
	rp := webauthn.RelyingParty{ID: "localhost", Origin: approval.LocalOrigin}
	createCh, assertCh, _, err := passkey.Begin(dir, rp, code, now)
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
	prf := make([]byte, 32)
	if err := passkey.Finish(dir, rp, passkey.Ceremony{
		ClientDataJSON: created.ClientDataJSON, AttestationObject: created.AttestationObject, PRFEnabled: true,
	}, passkey.Ceremony{
		ClientDataJSON: asserted.ClientDataJSON, AuthenticatorData: asserted.AuthenticatorData,
		Signature: asserted.Signature, CredentialID: asserted.CredentialID, PRFOutput: prf,
	}, now); err != nil {
		t.Fatal(err)
	}
	time.Sleep(20 * time.Millisecond)
	a, out, stderr := previewCLI()
	if code := a.main(t.Context(), []string{"setup", "--state-dir", dir, "--reset-passkeys"}); code != 0 {
		t.Fatalf("reset exited %d: %s", code, stderr)
	}
	if strings.TrimSpace(out.String()) != "passkeys reset" {
		t.Fatalf("reset output %q", out.String())
	}
	if _, err := os.Lstat(filepath.Join(dir, "passkeys.json")); !os.IsNotExist(err) {
		t.Fatal("passkeys.json survived reset")
	}
	a, out, stderr = previewCLI()
	if code := a.main(t.Context(), []string{"setup", "--state-dir", dir}); code != 0 {
		t.Fatalf("setup after reset exited %d: %s", code, stderr)
	}
	next := strings.TrimSpace(out.String())
	createCh, assertCh, _, err = passkey.Begin(dir, rp, next, time.Now().UTC())
	if err != nil {
		t.Fatal(err)
	}
	created, err = key.Register("localhost", approval.LocalOrigin, createCh)
	if err != nil {
		t.Fatal(err)
	}
	asserted, err = key.Assert("localhost", approval.LocalOrigin, assertCh, false)
	if err != nil {
		t.Fatal(err)
	}
	if err := passkey.Finish(dir, rp, passkey.Ceremony{
		ClientDataJSON: created.ClientDataJSON, AttestationObject: created.AttestationObject, PRFEnabled: true,
	}, passkey.Ceremony{
		ClientDataJSON: asserted.ClientDataJSON, AuthenticatorData: asserted.AuthenticatorData,
		Signature: asserted.Signature, CredentialID: asserted.CredentialID, PRFOutput: append([]byte(nil), prf...),
	}, time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
}
