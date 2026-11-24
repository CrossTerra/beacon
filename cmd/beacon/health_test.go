package main

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/CrossTerra/beacon/internal/keychain"
	"github.com/CrossTerra/beacon/internal/service"
	"github.com/CrossTerra/beacon/internal/vault"
)

// refusingStore represents a locked Secret Service collection.
type refusingStore struct{ *keychain.Memory }

func (refusingStore) Get(string) (keychain.Secret, error) {
	return keychain.Secret{}, vault.ErrLocked
}

func TestLockedVaultPausesTheCollector(t *testing.T) {
	h := newHarness(t)
	dir := t.TempDir()
	cfg, _ := writeSetup(t, dir, h.server.URL, "send", "kcr")
	a := h.app(h.egressList(cfg))
	a.keychain = func() (keychain.Store, error) { return refusingStore{h.store}, nil }
	if code := a.main(context.Background(), []string{"run", "--once", "--config", cfg}); code == 0 {
		t.Fatal("a refused key read must fail the pass")
	}
	if !strings.Contains(h.stderr.String(), "unlock your Secret Service collection") {
		t.Fatalf("stderr: %s", h.stderr.String())
	}
	p, err := service.ReadPass(filepath.Join(dir, "state"))
	if err != nil || p.Outcome != "vault_locked" || p.Lease != "" {
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
