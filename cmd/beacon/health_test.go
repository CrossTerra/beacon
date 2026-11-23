package main

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/CrossTerra/beacon/internal/keychain"
	"github.com/CrossTerra/beacon/internal/service"
)

// refusingStore is a keychain whose reads macOS refused, as after a rebuild signed with another certificate.
type refusingStore struct{ *keychain.Memory }

func (refusingStore) Get(string) (keychain.Secret, error) {
	return keychain.Secret{}, keychain.ErrReadRefused
}

// A refused read of the Signal key is recorded as keychain_read_refused, not as a missing key, and the message names
// the repair. Beacon.app's menu reads that code from pass.json.
func TestRefusedKeychainReadIsRecordedForTheMenu(t *testing.T) {
	h := newHarness(t)
	dir := t.TempDir()
	cfg, _ := writeSetup(t, dir, h.server.URL, "send", "kcr")
	a := h.app(h.egressList(cfg))
	a.keychain = func() (keychain.Store, error) { return refusingStore{h.store}, nil }
	if code := a.main(context.Background(), []string{"run", "--once", "--config", cfg}); code == 0 {
		t.Fatal("a refused key read must fail the pass")
	}
	if !strings.Contains(h.stderr.String(), "run beacon keychain repair") || strings.Contains(h.stderr.String(), "not in the keychain") {
		t.Fatalf("stderr: %s", h.stderr.String())
	}
	p, err := service.ReadPass(filepath.Join(dir, "state"))
	if err != nil || p.Outcome != "keychain_read_refused" || p.Lease != "" {
		t.Fatalf("pass %+v %v", p, err)
	}
}

// A send pass records when the lease it held runs out, so a reader can tell a stale lease from a held one.
func TestSendPassRecordsTheLeaseDeadline(t *testing.T) {
	h := newHarness(t)
	dir := t.TempDir()
	cfg, _ := writeSetup(t, dir, h.server.URL, "send", "lu")
	a := h.app(h.egressList(cfg))
	if code := a.main(context.Background(), []string{"run", "--once", "--config", cfg}); code != 0 {
		t.Fatalf("exit %d: %s", code, h.stderr.String())
	}
	p, err := service.ReadPass(filepath.Join(dir, "state"))
	if err != nil || p.Lease != service.LeaseHeld || p.LeaseUntil == nil || !p.LeaseUntil.After(time.Now()) || p.SendReason != "" {
		t.Fatalf("pass %+v %v", p, err)
	}
}
