package vault_test

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/CrossTerra/beacon/internal/actionexec"
	"github.com/CrossTerra/beacon/internal/approval"
	"github.com/CrossTerra/beacon/internal/config"
	"github.com/CrossTerra/beacon/internal/keychain"
	"github.com/CrossTerra/beacon/internal/localstate"
	"github.com/CrossTerra/beacon/internal/passkey"
	"github.com/CrossTerra/beacon/internal/vault"
	"github.com/CrossTerra/beacon/internal/webauthn"
	"github.com/CrossTerra/beacon/internal/webauthntest"
)

const (
	pairOrigin = "https://signal.fixture.test"
	pairSlot   = "signal:signal.fixture.test:443"
	pairDB     = "unattended.kdbx"
	pairKey    = "unattended.keyx"
	pairMarker = ".signal-pair-test-fixture"
)

var fixturePair = vault.SignalPair{
	Origin: pairOrigin, Person: "act_01m6a6ft00e40r2081040g2081", ApprovalActor: "act_01m6a6ft00e40r2081040g2082",
	CollectorActor: "act_01m6a6ft00e40r2081040g2083", CollectorAccount: pairSlot,
}

var pairTime = time.Unix(1_800_000_000, 0)

func pairOK(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

func pairRead(t *testing.T, dir, name string) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(dir, name))
	pairOK(t, err)
	return b
}

func fixtureSignalKey(t *testing.T, tag string, fill byte) keychain.Secret {
	t.Helper()
	// Syntactically valid dummy bytes; these secrets are never sent anywhere.
	value := "sig_" + tag + "_" + base64.RawURLEncoding.EncodeToString(bytes.Repeat([]byte{fill}, 32))
	s, err := keychain.FromString(value)
	pairOK(t, err)
	t.Cleanup(s.Clear)
	return s
}

func fixtureSignalKeys(t *testing.T) (keychain.Secret, keychain.Secret) {
	t.Helper()
	return fixtureSignalKey(t, "01m6a6ft00e40r2081040g2085", 0x33), fixtureSignalKey(t, "01m6a6ft00e40r2081040g2087", 0x51)
}

func pairSetup(t *testing.T) string {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "state")
	pairOK(t, vault.Create(dir)) // fixture only, without Secret Service
	o, err := approval.ParseOrigin(approval.LocalOrigin, "")
	pairOK(t, err)
	pairOK(t, localstate.Save(dir, o))
	j, err := approval.OpenJournal(dir, pairTime, "")
	pairOK(t, err)
	pairOK(t, j.Close())
	return dir
}

// Supply the nonsecret reader bindings in the fixture so that visibility depends only on the
// canonical KDBX file. This is not an operator-facing configuration or activation path.
func pairReaderConfigs(t *testing.T, dir string) {
	t.Helper()
	settings := actionexec.Settings{Signal: &actionexec.SignalSettings{
		Origin: fixturePair.Origin, Actor: fixturePair.ApprovalActor, Person: fixturePair.Person, Handle: "fixture",
	}}
	b, err := json.Marshal(settings)
	pairOK(t, err)
	pairOK(t, approval.WritePrivate(filepath.Join(dir, "action-providers.json"), b))
	cfg := config.Config{Signal: config.Signal{URL: fixturePair.Origin}, Person: config.Person{Handle: "@fixture"},
		Rules: "rules.yaml", State: "collector", Mode: config.DryRun, Sources: []config.Source{}}
	b, err = cfg.Marshal()
	pairOK(t, err)
	pairOK(t, approval.WritePrivate(filepath.Join(dir, "config.yaml"), b))
	pairOK(t, approval.WritePrivate(filepath.Join(dir, "rules.yaml"), []byte("rules: []\n")))
	pairOK(t, os.Mkdir(filepath.Join(dir, "collector"), 0o700))
	pairOK(t, approval.WritePrivate(filepath.Join(dir, "collector", "cursors.json"), []byte(`{"fixture":"unchanged"}`)))
}

func assertPairReaders(t *testing.T, dir string, visible bool) {
	t.Helper()
	cfg, err := config.Load(filepath.Join(dir, "config.yaml"))
	pairOK(t, err)
	s, err := actionexec.LoadSettings(dir)
	pairOK(t, err)
	if s.Signal == nil || cfg.Person.Handle != "@"+s.Signal.Handle || s.Signal.Person != fixturePair.Person || cfg.KeychainAccount() != fixturePair.CollectorAccount {
		t.Fatal("reader configurations disagree on the fixture identity")
	}
	collector := keychain.Unattended{Dir: dir, Bindings: map[string]vault.Binding{
		cfg.KeychainAccount(): vault.SignalCollectorBinding(cfg.Signal.URL, cfg.KeychainAccount()),
	}}
	appKey, appErr := keychain.ReadRecordKey(dir, s.Signal.Origin, s.Signal.Actor)
	defer appKey.Clear()
	relayKey, relayErr := collector.Get(cfg.KeychainAccount())
	defer relayKey.Clear()
	if !visible {
		if appErr == nil || !appKey.Empty() || !errors.Is(relayErr, keychain.ErrNotFound) || !relayKey.Empty() {
			t.Fatal("a reader saw an unpublished key")
		}
		return
	}
	wantApp, wantRelay := fixtureSignalKeys(t)
	if appErr != nil || relayErr != nil || !appKey.Same(wantApp) || !relayKey.Same(wantRelay) {
		t.Fatal("the two readers did not reopen the right distinct keys")
	}
	who, transport, err := s.Signal.RecordConnection(dir)(t.Context())
	pairOK(t, err)
	defer transport.(interface{ Close() }).Close()
	if who.Person != fixturePair.Person || who.Handle != "fixture" {
		t.Fatal("record connection did not use the common person")
	}
}

// Called in a test child. Exit without unwinding at each side of the single publication;
// there is no real service and no credential in the child's arguments or environment.
func TestSignalPairInterruptedChild(t *testing.T) {
	dir, point := os.Getenv("BEACON_PAIR_FIXTURE_DIR"), os.Getenv("BEACON_PAIR_FIXTURE_POINT")
	if dir == "" {
		return
	}
	if _, err := approval.CheckPrivate(filepath.Join(dir, pairMarker), 0o600); err != nil {
		t.Fatal("refusing to run the child outside a marked test fixture")
	}
	const interrupted = 47
	switch point {
	case "before rename":
		vault.SetPublishHooksForTest(func() { os.Exit(interrupted) }, nil)
	case "after rename":
		vault.SetPublishHooksForTest(nil, func(string) { os.Exit(interrupted) })
	default:
		t.Fatal("invalid fixture interruption point")
	}
	app, relay := fixtureSignalKeys(t)
	pairOK(t, keychain.StoreSignalPair(t.Context(), dir, fixturePair, app, relay))
	t.Fatal("did not visit the requested publication boundary")
}

func TestSignalPairPublicationSurvivesProcessExit(t *testing.T) {
	for _, point := range []string{"before rename", "after rename"} {
		t.Run(point, func(t *testing.T) {
			dir := pairSetup(t)
			pairReaderConfigs(t, dir)
			pairOK(t, approval.WritePrivate(filepath.Join(dir, pairMarker), nil))
			before := pairRead(t, dir, pairDB)
			assertPairReaders(t, dir, false)
			cmd := exec.CommandContext(t.Context(), os.Args[0], "-test.run=^TestSignalPairInterruptedChild$")
			cmd.Env = []string{"HOME=" + filepath.Dir(dir), "TMPDIR=" + filepath.Dir(dir), "BEACON_PAIR_FIXTURE_DIR=" + dir, "BEACON_PAIR_FIXTURE_POINT=" + point}
			output, err := cmd.CombinedOutput()
			var exit *exec.ExitError
			if !errors.As(err, &exit) || exit.ExitCode() != 47 || len(output) != 0 {
				t.Fatal("fixture did not exit silently at the requested publication boundary")
			}
			afterRename := point == "after rename"
			assertPairReaders(t, dir, afterRename)
			if bytes.Equal(before, pairRead(t, dir, pairDB)) == afterRename {
				t.Fatal("the canonical database did not change exactly at rename")
			}
			candidates, err := filepath.Glob(filepath.Join(dir, "."+pairDB+"-*"))
			pairOK(t, err)
			if !afterRename && len(candidates) != 1 || afterRename && len(candidates) != 0 {
				t.Fatal("unexpected candidate at the interruption boundary")
			}
			for _, path := range candidates {
				_, err := approval.CheckPrivate(path, 0o600)
				pairOK(t, err)
				b, err := os.ReadFile(path)
				pairOK(t, err)
				if bytes.Contains(b, []byte("sig_")) {
					t.Fatal("staged database exposed a fixture credential")
				}
			}
			app, relay := fixtureSignalKeys(t)
			old, err := os.Stat(filepath.Join(dir, pairDB))
			pairOK(t, err)
			candidate := pairRead(t, dir, pairDB)
			pairOK(t, keychain.StoreSignalPair(t.Context(), dir, fixturePair, app, relay))
			assertPairReaders(t, dir, true)
			if afterRename {
				current, err := os.Stat(filepath.Join(dir, pairDB))
				pairOK(t, err)
				if !os.SameFile(old, current) || !bytes.Equal(candidate, pairRead(t, dir, pairDB)) {
					t.Fatal("retry republished a completed pair")
				}
			}
		})
	}
}

func enrolledFixtureKey(t *testing.T, dir string) func() []byte {
	t.Helper()
	rp := webauthn.RelyingParty{ID: "localhost", Origin: approval.LocalOrigin}
	code, err := passkey.Issue(dir, pairTime)
	pairOK(t, err)
	create, assert, _, err := passkey.Begin(dir, rp, code, pairTime)
	pairOK(t, err)
	key, err := webauthntest.Generate(webauthntest.AlgES256)
	pairOK(t, err)
	reg, err := key.Register(rp.ID, rp.Origin, create)
	pairOK(t, err)
	a, err := key.Assert(rp.ID, rp.Origin, assert, false)
	pairOK(t, err)
	prf := bytes.Repeat([]byte{0x71}, 32)
	t.Cleanup(func() { clear(prf) })
	j, err := approval.OpenJournal(dir, pairTime, "")
	pairOK(t, err)
	defer j.Close()
	pairOK(t, passkey.Finish(dir, rp, passkey.Ceremony{
		ClientDataJSON: reg.ClientDataJSON, AttestationObject: reg.AttestationObject, PRFEnabled: true,
	}, passkey.Ceremony{
		ClientDataJSON: a.ClientDataJSON, AuthenticatorData: a.AuthenticatorData, Signature: a.Signature,
		CredentialID: a.CredentialID, PRFOutput: bytes.Clone(prf),
	}, pairTime))
	return func() []byte {
		j, err := approval.OpenJournal(dir, pairTime, "")
		pairOK(t, err)
		defer j.Close()
		data, err := passkey.OpenVault(dir, a.CredentialID, bytes.Clone(prf), rp.Origin)
		pairOK(t, err)
		return data
	}
}

func TestSignalPairPreservesAnEnrolledWriteVaultAndUnrelatedEntries(t *testing.T) {
	dir := pairSetup(t)
	pairReaderConfigs(t, dir)
	open := enrolledFixtureKey(t, dir)
	beforeKey := open()
	defer clear(beforeKey)
	b := vault.Binding{Service: "github", Origin: "https://github.fixture.test", Account: "github:user:1", Use: "read"}
	const unrelated = "fixture-provider-value"
	pairOK(t, vault.Put(dir, b, unrelated))
	write, err := keychain.FromString("fixture-provider-write-value")
	pairOK(t, err)
	defer write.Clear()
	lock, err := approval.LockInstance(dir)
	pairOK(t, err)
	storeErr := keychain.StoreActionWrite(dir, b, write, beforeKey)
	pairOK(t, approval.CloseInstance(dir, lock))
	pairOK(t, storeErr)
	files := []string{pairKey, "settings.json", "action-providers.json", "config.yaml", "journal.log", "rules.yaml",
		"collector/cursors.json", "passkeys.json", "write-vault.json", "write-github.sealed"}
	before := make(map[string][]byte)
	for _, name := range files {
		before[name] = pairRead(t, dir, name)
	}
	app, relay := fixtureSignalKeys(t)
	assertPairReaders(t, dir, false)
	pairOK(t, keychain.StoreSignalPair(t.Context(), dir, fixturePair, app, relay))
	assertPairReaders(t, dir, true)
	got, err := vault.Get(dir, b)
	if err != nil || got != unrelated {
		t.Fatal("the unrelated binding changed")
	}
	afterKey := open()
	defer clear(afterKey)
	if !bytes.Equal(beforeKey, afterKey) {
		t.Fatal("the enrolled passkey did not open the same write key")
	}
	unsealed, err := keychain.UnsealActionWrite(dir, b, afterKey)
	defer unsealed.Clear()
	if err != nil || !unsealed.Same(write) {
		t.Fatal("the existing provider write credential no longer opens")
	}
	for _, name := range files {
		if !bytes.Equal(before[name], pairRead(t, dir, name)) {
			t.Fatalf("paired publication changed %s", name)
		}
	}
	completed := pairRead(t, dir, pairDB)
	pairOK(t, keychain.StoreSignalPair(t.Context(), dir, fixturePair, app, relay))
	if !bytes.Equal(completed, pairRead(t, dir, pairDB)) {
		t.Fatal("an exact retry rotated or rewrote the vault")
	}
}

func TestSignalPairRefusesExistingPartialOrDifferentBindings(t *testing.T) {
	for _, tc := range []string{"lone app slot", "wrong collector binding", "wrong person", "different keys", "same keys", "live instance", "missing database"} {
		t.Run(tc, func(t *testing.T) {
			dir := pairSetup(t)
			pair := fixturePair
			app, relay := fixtureSignalKeys(t)
			want := vault.ErrConflict
			switch tc {
			case "lone app slot":
				pairOK(t, keychain.ImportBound(dir, vault.SignalRecordBinding(pair.Origin, pair.ApprovalActor), app))
			case "wrong collector binding":
				pairOK(t, keychain.ImportBound(dir, vault.SignalCollectorBinding("https://different.fixture.test", pair.CollectorAccount), relay))
			case "wrong person", "different keys":
				pairOK(t, keychain.StoreSignalPair(t.Context(), dir, pair, app, relay))
				if tc == "wrong person" {
					pair.Person = "act_01m6a6ft00e40r2081040g2089"
				} else {
					relay = fixtureSignalKey(t, "01m6a6ft00e40r2081040g2090", 0x4f)
				}
			case "same keys":
				relay, want = app, nil
			case "live instance":
				lock, err := approval.LockInstance(dir)
				pairOK(t, err)
				defer approval.CloseInstance(dir, lock)
				want = approval.ErrLocked
			case "missing database":
				pairOK(t, os.Remove(filepath.Join(dir, pairDB)))
				want = vault.ErrPartial
			}
			before, readErr := os.ReadFile(filepath.Join(dir, pairDB))
			secretFile := pairRead(t, dir, pairKey)
			err := keychain.StoreSignalPair(t.Context(), dir, pair, app, relay)
			if err == nil || want != nil && !errors.Is(err, want) || strings.Contains(err.Error(), "sig_") {
				t.Fatal("the transaction accepted invalid state or reported credential data")
			}
			after, afterErr := os.ReadFile(filepath.Join(dir, pairDB))
			if !bytes.Equal(before, after) || !bytes.Equal(secretFile, pairRead(t, dir, pairKey)) ||
				errors.Is(readErr, os.ErrNotExist) != errors.Is(afterErr, os.ErrNotExist) {
				t.Fatal("a refusal changed the vault or key")
			}
		})
	}
}

func TestSignalPairStopsOnCancellationOrExternalChange(t *testing.T) {
	for _, tc := range []string{"cancel", "stale database", "key changed"} {
		t.Run(tc, func(t *testing.T) {
			dir := pairSetup(t)
			pairReaderConfigs(t, dir)
			before := pairRead(t, dir, pairDB)
			key := pairRead(t, dir, pairKey)
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			expected, want := before, vault.ErrConflict
			target := filepath.Join(dir, pairDB)
			restore := vault.SetPublishHooksForTest(func() {
				switch tc {
				case "cancel":
					cancel()
				case "stale database":
					// Change the encrypted file after the candidate has been synced.
					expected = append(bytes.Clone(before), 1)
					pairOK(t, os.WriteFile(target, expected, 0o600))
				case "key changed":
					pairOK(t, os.WriteFile(filepath.Join(dir, pairKey), []byte("different fixture key"), 0o600))
				}
			}, nil)
			defer restore()
			if tc == "cancel" {
				want = context.Canceled
			}
			app, relay := fixtureSignalKeys(t)
			if err := keychain.StoreSignalPair(ctx, dir, fixturePair, app, relay); !errors.Is(err, want) {
				t.Fatal("interruption or stale base did not stop publication")
			}
			if tc == "key changed" {
				if string(pairRead(t, dir, pairKey)) != "different fixture key" {
					t.Fatal("the manual key edit was overwritten")
				}
				// Restore only the fixture's key so the two readers can inspect the unchanged database.
				pairOK(t, os.WriteFile(filepath.Join(dir, pairKey), key, 0o600))
			}
			if !bytes.Equal(expected, pairRead(t, dir, pairDB)) {
				t.Fatal("a refused commit overwrote the on-disk database")
			}
			assertPairReaders(t, dir, false)
		})
	}
}

func TestSignalPairSerializesThePublicationWithSingleEntryWrites(t *testing.T) {
	dir := pairSetup(t)
	pairReaderConfigs(t, dir)
	other := vault.Binding{Service: "github", Origin: "https://github.fixture.test", Account: "github:user:1", Use: "read"}
	pairOK(t, vault.Put(dir, other, "fixture-before"))
	var singleErr error
	var tried bool
	restore := vault.SetPublishHooksForTest(func() {
		// Without the publication lock this writer could finish while the pair is staged.
		if !tried {
			tried = true
			singleErr = vault.Put(dir, other, "fixture-after")
		}
	}, nil)
	defer restore()
	app, relay := fixtureSignalKeys(t)
	pairOK(t, keychain.StoreSignalPair(t.Context(), dir, fixturePair, app, relay))
	if !tried || !errors.Is(singleErr, vault.ErrConflict) {
		t.Fatal("a concurrent Beacon publication was not excluded")
	}
	assertPairReaders(t, dir, true)
	got, err := vault.Get(dir, other)
	if err != nil || got != "fixture-before" {
		t.Fatal("the conflicting publication changed the unrelated entry")
	}
}
