package main

import (
	"context"
	"encoding/base64"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/CrossTerra/beacon/internal/approval"
	"github.com/CrossTerra/beacon/internal/localstate"
	"github.com/CrossTerra/beacon/internal/passkey"
	"github.com/CrossTerra/beacon/internal/webauthn"
	"github.com/CrossTerra/beacon/internal/webauthntest"
)

func TestSetupRefusesIncompleteUnattendedVault(t *testing.T) {
	dir := previewTemp(t)
	key := filepath.Join(dir, "unattended.keyx")
	must(t, os.WriteFile(key, []byte("interrupted setup fixture"), 0o600))
	a, out, stderr := previewCLI()
	if code := a.main(t.Context(), []string{"setup", "--state-dir", dir, "--origin", "localhost"}); code == 0 ||
		out.Len() != 0 || !strings.Contains(stderr.String(), "incomplete unattended vault") {
		t.Fatalf("partial vault looked ready (exit %d): %s", code, stderr)
	}
	for _, name := range []string{"unattended.kdbx", "settings.json", "setup-code.json"} {
		if _, err := os.Lstat(filepath.Join(dir, name)); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("setup published %s with no database", name)
		}
	}
}

func TestSetupTailscaleThenSwitchOriginOnlyWithConsent(t *testing.T) {
	dir := previewTemp(t)
	a, _, stderr := previewCLI()
	a.tailscale = func(_ context.Context, args ...string) ([]byte, error) {
		switch strings.Join(args, " ") {
		case "status --json":
			return []byte(`{"BackendState":"Running","Self":{"DNSName":"box.tail.ts.net.","UserID":9},"User":{"9":{"LoginName":"person@example.test"}}}`), nil
		case "serve status --json":
			return []byte(`{"TCP":{"443":{"HTTPS":true}},"Web":{"box.tail.ts.net:443":{"Handlers":{"/":{"Proxy":"http://127.0.0.1:7317"}}}}}`), nil
		}
		return nil, errors.New("unexpected tailscale invocation")
	}
	setup := func(mode string, input string) int {
		a.stdin = strings.NewReader(input)
		return a.main(t.Context(), []string{"setup", "--state-dir", dir, "--origin", mode})
	}
	if setup("tailscale", "") != 0 {
		t.Fatal(stderr)
	}
	if setup("localhost", "no\n") == 0 {
		t.Fatal("switch did not require a confirmation")
	}
	old, err := localstate.Load(dir)
	if err != nil || old.URL() != "https://box.tail.ts.net" {
		t.Fatalf("the declined switch changed the origin: %v %v", old, err)
	}
	file := filepath.Join(dir, "unattended.kdbx")
	info, err := os.Stat(file)
	must(t, err)
	if setup("localhost", "yes\n") != 0 {
		t.Fatal(stderr)
	}
	next, err := localstate.Load(dir)
	if err != nil || next.URL() != approval.LocalOrigin {
		t.Fatalf("switch did not commit: %v %v", next, err)
	}
	info2, err := os.Stat(file)
	must(t, err)
	if !os.SameFile(info, info2) {
		t.Fatal("switch replaced unattended vault")
	}
	if _, reset, err := approval.PasskeyMarks(dir); reset == 0 || err != nil {
		t.Fatalf("switch did not journal a passkey reset: %d, %v", reset, err)
	}
}

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
	j, err := approval.OpenJournal(dir, now, "")
	must(t, err)
	defer j.Close()
	if err := passkey.Finish(dir, rp, passkey.Ceremony{
		ClientDataJSON: created.ClientDataJSON, AttestationObject: created.AttestationObject, PRFEnabled: true,
	}, passkey.Ceremony{
		ClientDataJSON: asserted.ClientDataJSON, AuthenticatorData: asserted.AuthenticatorData,
		Signature: asserted.Signature, CredentialID: asserted.CredentialID, PRFOutput: prf,
	}, now); err != nil {
		t.Fatal(err)
	}
	must(t, j.Close())
	a, out, stderr := previewCLI()
	if code := a.main(t.Context(), []string{"setup", "--state-dir", dir, "--reset-passkeys"}); code != 0 {
		t.Fatalf("reset exited %d: %s", code, stderr)
	}
	next := strings.TrimSpace(out.String())
	if _, err := os.Lstat(filepath.Join(dir, "passkeys.json")); !os.IsNotExist(err) {
		t.Fatal("passkeys.json survived reset")
	}
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
	j, err = approval.OpenJournal(dir, time.Now(), "")
	must(t, err)
	defer j.Close()
	if err := passkey.Finish(dir, rp, passkey.Ceremony{
		ClientDataJSON: created.ClientDataJSON, AttestationObject: created.AttestationObject, PRFEnabled: true,
	}, passkey.Ceremony{
		ClientDataJSON: asserted.ClientDataJSON, AuthenticatorData: asserted.AuthenticatorData,
		Signature: asserted.Signature, CredentialID: asserted.CredentialID, PRFOutput: append([]byte(nil), prf...),
	}, time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
}
