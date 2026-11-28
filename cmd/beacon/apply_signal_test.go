package main

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"hash/crc32"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
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

const signalDB = "unattended.kdbx"

var signalSetupTime = time.Unix(1_800_000_000, 0).UTC()

func signalApplyFixture(t *testing.T) (dir, code string) {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, "config"))
	t.Setenv("XDG_STATE_HOME", filepath.Join(home, "state"))
	t.Setenv("BEACON_CONFIG", "")
	dir, err := localstate.DefaultDir()
	must(t, err)
	a, out, _ := previewCLI()
	a.now = func() time.Time { return signalSetupTime }
	a.euid = func() int { return 501 }
	if a.main(t.Context(), []string{"setup", "--origin", "localhost"}) != 0 {
		t.Fatal("could not initialize the private post-install fixture")
	}
	return dir, strings.TrimSpace(out.String())
}

func applyArgs(dir, person string) []string {
	return []string{"setup", "apply-signal", "--state-dir", dir, "--signal-origin", checkOrigin, "--person", person}
}

func applyInput(t *testing.T) string {
	t.Helper()
	return verifyInput(t, checkOrigin, checkAppKey) + "\n" + checkRelayKey + "\n"
}

func runSignalApply(ctx context.Context, t *testing.T, f *verificationFixture, args []string, input string, boundary func(string) error) (exit int, stdout, stderr string) {
	t.Helper()
	a, out, errOut := previewCLI()
	a.euid = func() int { return 501 }
	a.stdin = strings.NewReader(input)
	a.transport = f.transport
	a.signalApplyBoundary = boundary
	exit = a.main(ctx, args)
	assertSignalRedacted(t, out.String()+errOut.String())
	return exit, out.String(), errOut.String()
}

func assertSignalRedacted(t *testing.T, text string) {
	t.Helper()
	for _, hidden := range []string{checkAppKey, checkRelayKey, "beacon-setup:v1:", checkIdentity} {
		if strings.Contains(text, hidden) {
			t.Fatal("fixture credential or source identity was reflected")
		}
	}
}

func assertSignalApplyLock(t *testing.T, dir string) {
	t.Helper()
	if approval.HoldsInstance(dir) {
		t.Fatal("the command registered its lock as a borrowable service lock")
	}
	f, err := approval.LockInstance(dir)
	if !errors.Is(err, approval.ErrLocked) {
		if f != nil {
			approval.CloseInstance(dir, f)
		}
		t.Fatal("the instance lock was not held continuously")
	}
}

// Reopen the real record connection and collector store, then exercise their CLI startup paths.
// No listener, approved action, provider read or Signal POST is used by this test.
func assertSignalApplyReaders(t *testing.T, dir string, active bool) {
	t.Helper()
	cfgPath := localstate.SignalCollectorPath(dir)
	a, _, _ := previewCLI()
	a.euid = func() int { return 501 }
	a.transport = func([]string) http.RoundTripper {
		return signalCheckTransport(func(*http.Request) (*http.Response, error) {
			t.Fatal("the starter attempted network I/O")
			return nil, nil
		})
	}
	s, err := actionexec.LoadSettings(dir)
	must(t, err)
	if s.Signal != nil {
		who, records, err := s.Signal.RecordConnection(dir)(t.Context())
		if c, ok := records.(interface{ Close() }); ok {
			c.Close()
		}
		if (err == nil) != active || (records != nil) != active || who.Person != checkPerson || who.Handle != "fixture" || s.Signal.Actor != checkApp {
			t.Fatal("approval record reader did not see the expected paired activation")
		}
	} else if active {
		t.Fatal("approval settings lost the committed pair")
	}
	cfg, err := a.loadConfig(cfgPath)
	if err == nil {
		store, err := a.collectorStore(cfg, false)
		must(t, err)
		got, readErr := store.Get(cfg.KeychainAccount())
		defer got.Clear()
		want, err := keychain.FromString(checkRelayKey)
		must(t, err)
		defer want.Clear()
		if (readErr == nil) != active || active && !got.Same(want) || !active && !got.Empty() ||
			cfg.Mode != config.DryRun || len(cfg.Sources) != 0 || cfg.Person.Handle != "@fixture" || cfg.Signal.URL != checkOrigin {
			t.Fatal("collector reader did not see the expected paired dry-run activation")
		}
	} else if active || !errors.Is(err, os.ErrNotExist) {
		t.Fatal("unexpected failure opening the canonical collector config")
	}
	job, err := collectorJob(dir, cfgPath)
	must(t, err)
	if job != nil {
		t.Fatal("the starter config enabled an automatic collector job")
	}
	if code := a.main(t.Context(), []string{"run", "--once", "--config", cfgPath}); (code == 0) != active {
		t.Fatal("collector CLI did not gate the explicit dry-run starter on paired activation")
	}
	reached := false
	a.approvalListeners = func() ([]net.Listener, error) {
		reached = true
		return nil, errors.New("fixture stops before starting listeners")
	}
	if a.main(t.Context(), []string{"serve", "--state-dir", dir}) == 0 || reached != (active || s.Signal == nil) {
		t.Fatal("service startup crossed an incomplete paired activation")
	}
}

func readSignalFixture(t *testing.T, dir, name string) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(dir, name))
	must(t, err)
	return b
}

func assertSignalOriginalFiles(t *testing.T, dir string, before map[string]string) {
	t.Helper()
	for path, original := range before {
		if original == "<dir>" || path == filepath.Join(dir, signalDB) {
			continue
		}
		b, err := os.ReadFile(path)
		if err != nil || string(b) != original {
			t.Fatalf("offline apply changed existing fixture file %s", filepath.Base(path))
		}
	}
	for _, body := range fixtureTree(t, dir) {
		assertSignalRedacted(t, body)
	}
}

func TestApplySignalOfflineAfterInstall(t *testing.T) {
	dir, _ := signalApplyFixture(t)
	before := fixtureTree(t, dir)
	f := signalVerificationFixture(t)
	f.before = func(string) { assertSignalApplyLock(t, dir) }
	args := applyArgs(dir, "@fixture")
	input := applyInput(t)
	code, out, _ := runSignalApply(t.Context(), t, f, args, input, func(string) error { assertSignalApplyLock(t, dir); return nil })
	if code != 0 || !strings.Contains(out, "Both keys were saved together") || len(f.calls) != 6 {
		t.Fatal("post-install CLI did not apply a freshly verified pair")
	}
	assertSignalApplyReaders(t, dir, true)
	assertSignalOriginalFiles(t, dir, before)
	if _, err := approval.CheckPrivate(filepath.Join(dir, localstate.SignalPreparationDir), os.ModeDir|0o700); err != nil {
		t.Fatal("preparation directory is not private")
	}
	for _, path := range []string{signalDB, "signal-onboarding/connection.json", "signal-onboarding/config.yaml", "signal-onboarding/rules.yaml"} {
		_, err := approval.CheckPrivate(filepath.Join(dir, path), 0o600)
		must(t, err)
	}
	active := fixtureTree(t, dir)
	f = signalVerificationFixture(t)
	code, out, _ = runSignalApply(t.Context(), t, f, applyArgs(dir, checkPerson), input, nil)
	if code != 0 || !strings.Contains(out, "No files were changed") || len(f.calls) != 6 || !reflect.DeepEqual(active, fixtureTree(t, dir)) {
		t.Fatal("exact retry did not reverify with no changes")
	}
	selector := verificationArgs(dir, localstate.SignalCollectorPath(dir), "@fixture")
	lock, err := approval.LockInstance(dir)
	must(t, err)
	defer approval.CloseInstance(dir, lock)
	code, out, _ = runSignalCheck(t.Context(), t, signalVerificationFixture(t), input, selector)
	if code != 0 || !strings.Contains(out, "Nothing was saved") || !reflect.DeepEqual(active, fixtureTree(t, dir)) {
		t.Fatal("the verifier was not read-only after activation")
	}
}

func enrollSignalApplyFixture(t *testing.T, dir, code string) func() []byte {
	t.Helper()
	rp := webauthn.RelyingParty{ID: "localhost", Origin: approval.LocalOrigin}
	create, challenge, _, err := passkey.Begin(dir, rp, code, signalSetupTime)
	must(t, err)
	key, err := webauthntest.Generate(webauthntest.AlgES256)
	must(t, err)
	reg, err := key.Register(rp.ID, rp.Origin, create)
	must(t, err)
	asserted, err := key.Assert(rp.ID, rp.Origin, challenge, false)
	must(t, err)
	prf := bytes.Repeat([]byte{0x6a}, 32)
	t.Cleanup(func() { clear(prf) })
	j, err := approval.OpenJournal(dir, signalSetupTime, "")
	must(t, err)
	defer j.Close()
	must(t, passkey.Finish(dir, rp, passkey.Ceremony{
		ClientDataJSON: reg.ClientDataJSON, AttestationObject: reg.AttestationObject, PRFEnabled: true,
	}, passkey.Ceremony{
		ClientDataJSON: asserted.ClientDataJSON, AuthenticatorData: asserted.AuthenticatorData, Signature: asserted.Signature,
		CredentialID: asserted.CredentialID, PRFOutput: bytes.Clone(prf),
	}, signalSetupTime))
	return func() []byte {
		j, err := approval.OpenJournal(dir, signalSetupTime, "")
		must(t, err)
		defer j.Close()
		opened, err := passkey.OpenVault(dir, asserted.CredentialID, bytes.Clone(prf), rp.Origin)
		must(t, err)
		return opened
	}
}

func TestApplySignalKeepsEnrolledPasskeyAndUnrelatedState(t *testing.T) {
	dir, code := signalApplyFixture(t)
	open := enrollSignalApplyFixture(t, dir, code)
	beforeKey := open()
	defer clear(beforeKey)
	b := vault.Binding{Service: "github", Origin: "https://github.fixture.test", Account: "github:user:1"}
	const unrelated = "synthetic-provider-read"
	read := b
	read.Use = "read"
	must(t, vault.Put(dir, read, unrelated))
	write, err := keychain.FromString("synthetic-provider-write")
	must(t, err)
	defer write.Clear()
	f, err := approval.LockInstance(dir)
	must(t, err)
	writeErr := keychain.StoreActionWrite(dir, b, write, beforeKey)
	must(t, approval.CloseInstance(dir, f))
	must(t, writeErr)
	must(t, approval.WritePrivate(filepath.Join(dir, "action-providers.json"), []byte("{ \"github\": {\"origin\": \"https://github.fixture.test\", \"user_id\": 1} }\n")))
	must(t, approval.WritePrivate(filepath.Join(dir, "rules.yaml"), []byte("# manual rules, not the starter\nrules: []\n")))
	must(t, os.Mkdir(filepath.Join(dir, "collector"), 0o700))
	must(t, approval.WritePrivate(filepath.Join(dir, "collector/cursors.json"), []byte(`{"unrelated":"cursor"}`)))
	before := fixtureTree(t, dir)
	if code, _, _ := runSignalApply(t.Context(), t, signalVerificationFixture(t), applyArgs(dir, checkPerson), applyInput(t), nil); code != 0 {
		t.Fatal("apply refused intact enrollment")
	}
	assertSignalApplyReaders(t, dir, true)
	afterKey := open()
	defer clear(afterKey)
	if !bytes.Equal(beforeKey, afterKey) {
		t.Fatal("the original passkey no longer opens the same vault")
	}
	got, err := keychain.UnsealActionWrite(dir, b, afterKey)
	defer got.Clear()
	value, readErr := vault.Get(dir, read)
	if err != nil || !got.Same(write) || readErr != nil || value != unrelated {
		t.Fatal("unrelated provider credentials did not survive")
	}
	assertSignalOriginalFiles(t, dir, before)
}

type signalApplyRecorded struct{ t *testing.T }

func (r signalApplyRecorded) PostRecord(_ context.Context, body []byte) approval.RecordAnswer {
	r.t.Helper()
	if !bytes.Contains(body, []byte(checkPerson)) {
		r.t.Fatal("fixture did not create a record owned by the intended person")
	}
	return approval.RecordAnswer{EventID: "evt_01m6a6ft00e40r2081040g2081"}
}

func seedSignalAction(t *testing.T, dir string, delivered bool) {
	t.Helper()
	j, err := approval.OpenJournal(dir, signalSetupTime, "")
	must(t, err)
	defer j.Close()
	_, err = j.Add(approval.Request{Type: approval.Merge, Agent: "fixture", Reason: "Fixture only.", Params: approval.Object{
		"repo": "fixture/repo", "number": int64(1), "base": "main", "head": strings.Repeat("a", 40), "method": "squash",
	}}, approval.PeerLabel{}, signalSetupTime)
	must(t, err)
	if delivered {
		r, err := j.Drain(t.Context(), approval.SignalIdentity{Person: checkPerson, Handle: "fixture"}, signalApplyRecorded{t})
		must(t, err)
		if r.Sent != 1 {
			t.Fatal("fixture action was not acknowledged")
		}
	}
}

func appendSignalFixtureFrame(body, frame []byte) []byte {
	n := uint32(len(frame))
	body = append(body, "BCN1"...)
	body = binary.BigEndian.AppendUint32(body, n)
	body = binary.BigEndian.AppendUint32(body, ^n)
	body = append(body, frame...)
	return binary.BigEndian.AppendUint32(body, crc32.ChecksumIEEE(frame))
}

func TestApplySignalRefusesAnyActionOrInvalidHistoryWithoutReplay(t *testing.T) {
	for _, name := range []string{"unowned action", "owned delivered action", "torn frame", "unknown frame", "duplicate field", "delivery only", "invalid checksum", "missing"} {
		t.Run(name, func(t *testing.T) {
			dir, _ := signalApplyFixture(t)
			path := filepath.Join(dir, "journal.log")
			body := readSignalFixture(t, dir, "journal.log")
			switch name {
			case "unowned action", "owned delivered action":
				seedSignalAction(t, dir, name == "owned delivered action")
			case "missing":
				must(t, os.Remove(path))
			default:
				switch name {
				case "torn frame":
					body = append(body, "BCN1\x00"...)
				case "unknown frame":
					body = appendSignalFixtureFrame(body, []byte(`{"kind":"unknown","at":1800000000000}`))
				case "duplicate field":
					body = appendSignalFixtureFrame(body, []byte(`{"kind":"proposed","kind":"passkey_added","at":1800000000000,"vault_hash":"AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA="}`))
				case "delivery only":
					body = appendSignalFixtureFrame(body, []byte(`{"kind":"sending","seq":1}`))
				case "invalid checksum":
					body[len(body)-1] ^= 1
				}
				must(t, os.WriteFile(path, body, 0o600))
			}
			before := fixtureTree(t, dir)
			f := signalVerificationFixture(t)
			code, out, errOut := runSignalApply(t.Context(), t, f, applyArgs(dir, "@fixture"), applyInput(t), nil)
			if code == 0 || out != "" || f.opened || !strings.Contains(errOut, "setup-only journal") || !reflect.DeepEqual(before, fixtureTree(t, dir)) {
				t.Fatal("journal refusal reached Signal, replayed or changed local state")
			}
		})
	}
}

func TestApplySignalRefusesUnreadyLocalStateBeforeVerification(t *testing.T) {
	for _, name := range []string{"held lock", "partial slot", "misbound slot", "existing providers signal", "existing collector config",
		"Swift archive", "unconfirmed reaper", "passkey publication debris", "other review origin"} {
		t.Run(name, func(t *testing.T) {
			dir, _ := signalApplyFixture(t)
			write := func(name, body string) { must(t, approval.WritePrivate(filepath.Join(dir, name), []byte(body))) }
			switch name {
			case "held lock":
				l, err := approval.LockInstance(dir)
				must(t, err)
				defer approval.CloseInstance(dir, l)
			case "partial slot":
				must(t, vault.Put(dir, vault.SignalRecordBinding(checkOrigin, checkApp), checkAppKey))
			case "misbound slot":
				must(t, vault.Put(dir, vault.SignalCollectorBinding("https://different.fixture.test", "fixture-slot"), checkRelayKey))
			case "existing providers signal":
				write("action-providers.json", `{"signal":{"origin":"`+checkOrigin+`","actor_id":"`+checkApp+`","person_id":"`+checkPerson+`","handle":"fixture"}}`)
			case "existing collector config":
				write("config.yaml", "# manual collector config")
			case "Swift archive":
				must(t, os.Mkdir(filepath.Join(dir, "swift-archive"), 0o700))
			case "unconfirmed reaper":
				write("supervisor.session", "fixture marker")
			case "passkey publication debris":
				write("write-vault.json", "partial fixture")
			case "other review origin":
				write("settings.json", `{"origin":"https://machine.tail.ts.net","tailscale_login":"fixture@example.test"}`)
			}
			before := fixtureTree(t, dir)
			f := signalVerificationFixture(t)
			code, out, errOut := runSignalApply(t.Context(), t, f, applyArgs(dir, "@fixture"), applyInput(t), nil)
			if code == 0 || out != "" || f.opened || strings.Contains(errOut, "input hidden") || !reflect.DeepEqual(before, fixtureTree(t, dir)) {
				t.Fatal("local refusal prompted, verified or changed state")
			}
		})
	}
}

func TestApplySignalRefusesWrongAuthorityOrInput(t *testing.T) {
	for _, name := range []string{"approval role", "collector owner", "unintended person", "wrong grant", "wrong origin",
		"malformed key", "duplicate key", "not a terminal", "credential in unknown flag"} {
		t.Run(name, func(t *testing.T) {
			dir, _ := signalApplyFixture(t)
			before := fixtureTree(t, dir)
			f := signalVerificationFixture(t)
			inputOrigin, collector, person := checkOrigin, checkRelayKey, "@fixture"
			var extra []string
			expectReads := true
			switch name {
			case "approval role":
				f.appMe["scopes"] = []string{"read", "write", "admin"}
			case "collector owner":
				f.relayMe["actor"].(approval.Object)["works_for"] = checkOther
			case "unintended person":
				person = "@different"
			case "wrong grant":
				f.grant["identity_prefixes"] = []string{"email:*"}
			case "wrong origin":
				inputOrigin, expectReads = "https://different.fixture.test", false
			case "malformed key":
				collector, expectReads = "sig_fixture_invalid", false
			case "duplicate key":
				collector, expectReads = checkAppKey, false
			case "not a terminal":
				f, err := os.CreateTemp(t.TempDir(), "input")
				must(t, err)
				defer f.Close()
				a, out, errOut := previewCLI()
				a.euid = func() int { return 501 }
				a.stdin = f // nonterminal os.File, not the hidden-input fixture seam
				if a.main(t.Context(), applyArgs(dir, person)) == 0 || out.Len() != 0 || !strings.Contains(errOut.String(), "hidden input is required") ||
					!reflect.DeepEqual(before, fixtureTree(t, dir)) {
					t.Fatal("apply accepted nonterminal input")
				}
				return
			case "credential in unknown flag":
				extra, expectReads = []string{"--" + checkAppKey}, false
			}
			input := verifyInput(t, inputOrigin, checkAppKey) + "\n" + collector + "\n"
			code, out, _ := runSignalApply(t.Context(), t, f, append(applyArgs(dir, person), extra...), input, nil)
			if code == 0 || out != "" || f.opened != expectReads || !reflect.DeepEqual(before, fixtureTree(t, dir)) {
				t.Fatal("wrong authority or input did not fail without writes")
			}
		})
	}
}

func TestApplySignalDoesNotRotateAnExistingPair(t *testing.T) {
	dir, _ := signalApplyFixture(t)
	args := applyArgs(dir, checkPerson)
	if code, _, _ := runSignalApply(t.Context(), t, signalVerificationFixture(t), args, applyInput(t), nil); code != 0 {
		t.Fatal("fixture apply failed")
	}
	before := fixtureTree(t, dir)
	f := signalVerificationFixture(t)
	const otherTag = "01m6a6ft00e40r2081040g2087"
	f.relayKey = strings.Replace(checkRelayKey, checkRelayTag, otherTag, 1)
	f.relayMe["key"].(approval.Object)["id"], f.grant["id"] = "key_"+otherTag, "key_"+otherTag
	code, out, errOut := runSignalApply(t.Context(), t, f, args, strings.Replace(applyInput(t), checkRelayKey, f.relayKey, 1), nil)
	if code == 0 || out != "" || len(f.calls) != 6 || strings.Contains(errOut, f.relayKey) || !reflect.DeepEqual(before, fixtureTree(t, dir)) {
		t.Fatal("same-actor replacement was not refused after verification")
	}
	assertSignalApplyReaders(t, dir, true)
}

const signalApplyMarker = ".signal-apply-fixture"

func TestSignalApplyInterruptedChild(t *testing.T) {
	dir := os.Getenv("BEACON_APPLY_FIXTURE_DIR")
	if dir == "" {
		return
	}
	if _, err := approval.CheckPrivate(filepath.Join(dir, signalApplyMarker), 0o600); err != nil {
		t.Fatal("refusing a child run outside a marked private fixture")
	}
	step := os.Getenv("BEACON_APPLY_FIXTURE_STEP")
	_, _, _ = runSignalApply(t.Context(), t, signalVerificationFixture(t), applyArgs(dir, checkPerson), applyInput(t), func(got string) error {
		assertSignalApplyLock(t, dir)
		if step == got {
			os.Exit(47) // no defers, cleanup, or flushes after the selected boundary
		}
		return nil
	})
	t.Fatal("child missed the interruption boundary")
}

func TestApplySignalReopensAndRetriesAcrossPublicationBoundaries(t *testing.T) {
	for _, step := range []string{signalConfigReady, signalConfigRenamed, signalConfigSynced, signalVaultReady, signalVaultRenamed, signalVaultSynced} {
		t.Run(step, func(t *testing.T) {
			dir, passkeyCode := signalApplyFixture(t)
			var reopen func() []byte
			if step == signalVaultReady || step == signalVaultRenamed {
				reopen = enrollSignalApplyFixture(t, dir, passkeyCode)
			}
			must(t, approval.WritePrivate(filepath.Join(dir, signalApplyMarker), nil))
			before := fixtureTree(t, dir)
			home, err := os.UserHomeDir()
			must(t, err)
			cmd := exec.CommandContext(t.Context(), os.Args[0], "-test.run=^TestSignalApplyInterruptedChild$")
			cmd.Env = []string{"HOME=" + home, "TMPDIR=" + home, "XDG_CONFIG_HOME=" + filepath.Join(home, "config"),
				"BEACON_APPLY_FIXTURE_DIR=" + dir, "BEACON_APPLY_FIXTURE_STEP=" + step}
			output, err := cmd.CombinedOutput()
			var exit *exec.ExitError
			if !errors.As(err, &exit) || exit.ExitCode() != 47 || len(output) != 0 {
				t.Fatal("fixture child did not exit silently at the publication boundary")
			}
			committed := step == signalVaultRenamed || step == signalVaultSynced
			pending, err := localstate.LoadSignalPreparation(dir)
			must(t, err)
			if (pending == nil) != (step == signalConfigReady) {
				t.Fatal("the canonical preparation was not published exactly at its rename")
			}
			canonical := filepath.Join(dir, signalDB)
			if (string(readSignalFixture(t, dir, signalDB)) != before[canonical]) != committed {
				t.Fatal("the vault changed at a different boundary than its rename")
			}
			assertSignalApplyReaders(t, dir, committed)
			assertSignalOriginalFiles(t, dir, before)
			dbBefore, err := os.Stat(canonical)
			must(t, err)
			var enrolledKey []byte
			if reopen != nil {
				enrolledKey = reopen()
				defer clear(enrolledKey)
			}
			f := signalVerificationFixture(t)
			code, out, _ := runSignalApply(t.Context(), t, f, applyArgs(dir, "@fixture"), applyInput(t), nil)
			if code != 0 || len(f.calls) != 6 || strings.Contains(out, "No files were changed") != committed {
				t.Fatal("retry did not reverify and recover publication")
			}
			dbAfter, err := os.Stat(canonical)
			must(t, err)
			if os.SameFile(dbBefore, dbAfter) != committed {
				t.Fatal("retry republished an already committed vault, or failed to publish an uncommitted pair")
			}
			assertSignalApplyReaders(t, dir, true)
			assertSignalOriginalFiles(t, dir, before)
			if reopen != nil {
				got := reopen()
				defer clear(got)
				if !bytes.Equal(enrolledKey, got) {
					t.Fatal("interruption or retry changed the enrolled write vault")
				}
			}
		})
	}
}

func TestApplySignalFailureLeavesInertPreparationOrCompletedPair(t *testing.T) {
	for _, boundary := range []string{signalConfigReady, signalConfigRenamed, signalVaultReady, signalVaultRenamed} {
		t.Run(boundary, func(t *testing.T) {
			dir, _ := signalApplyFixture(t)
			before := fixtureTree(t, dir)
			code, out, _ := runSignalApply(t.Context(), t, signalVerificationFixture(t), applyArgs(dir, "@fixture"), applyInput(t), func(step string) error {
				if step == boundary {
					return errors.New(checkAppKey + " synthetic I/O error")
				}
				return nil
			})
			if code == 0 || out != "" {
				t.Fatal("failure was reported as a successful activation")
			}
			assertSignalApplyReaders(t, dir, boundary == signalVaultRenamed)
			assertSignalOriginalFiles(t, dir, before)
		})
	}
}

func TestApplySignalRechecksAfterVerificationAndBeforeCommit(t *testing.T) {
	for _, name := range []string{"canceled verification", "vault edit during verification", "journal edit during verification", "config created before preparation",
		"settings edit before vault publication", "stale vault on retry", "stale journal on retry", "edited preparation on retry"} {
		t.Run(name, func(t *testing.T) {
			dir, _ := signalApplyFixture(t)
			base := readSignalFixture(t, dir, signalDB)
			args, input := applyArgs(dir, checkPerson), applyInput(t)
			f := signalVerificationFixture(t)
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			var edited string
			appendToJournal := func() {
				edited = filepath.Join(dir, "journal.log")
				b := append(readSignalFixture(t, dir, "journal.log"), 'B')
				must(t, os.WriteFile(edited, b, 0o600))
			}
			b := vault.Binding{Service: "github", Origin: "https://github.fixture.test", Account: "github:user:1", Use: "read"}
			editVault := func() {
				must(t, vault.Put(dir, b, "fixture edit"))
				base = readSignalFixture(t, dir, signalDB)
			}
			const providerEdit = `{"github":{"origin":"https://github.fixture.test","user_id":1}}`
			f.before = func(call string) {
				if call == "collector "+checkGrantPath {
					switch name {
					case "canceled verification":
						cancel()
					case "vault edit during verification":
						editVault()
					case "journal edit during verification":
						appendToJournal()
					}
				}
			}
			code, out, _ := runSignalApply(ctx, t, f, args, input, func(step string) error {
				switch {
				case name == "config created before preparation" && step == signalConfigReady:
					edited = filepath.Join(dir, "config.yaml")
					must(t, approval.WritePrivate(edited, []byte(providerEdit)))
				case name == "settings edit before vault publication" && step == signalVaultReady:
					edited = filepath.Join(dir, "action-providers.json")
					must(t, approval.WritePrivate(edited, []byte(providerEdit)))
				case strings.HasSuffix(name, "on retry") && step == signalConfigSynced:
					return errors.New("interrupted preparation fixture")
				}
				return nil
			})
			if code == 0 || out != "" || !bytes.Equal(base, readSignalFixture(t, dir, signalDB)) {
				t.Fatal("apply overwrote the changed base or reported success")
			}
			if edited != "" {
				data, err := os.ReadFile(edited)
				must(t, err)
				if string(data) != providerEdit && !bytes.HasSuffix(data, []byte{'B'}) {
					t.Fatal("apply changed the concurrent manual edit")
				}
			}

			if !strings.HasSuffix(name, "on retry") {
				return
			}
			switch name {
			case "stale vault on retry":
				editVault()
			case "stale journal on retry":
				seedSignalAction(t, dir, false)
			case "edited preparation on retry":
				must(t, approval.WritePrivate(localstate.SignalCollectorPath(dir), []byte("# keep the manually edited file\n")))
			}
			before := fixtureTree(t, dir)
			f = signalVerificationFixture(t)
			code, out, _ = runSignalApply(t.Context(), t, f, args, input, nil)
			if code == 0 || out != "" || f.opened || !reflect.DeepEqual(before, fixtureTree(t, dir)) {
				t.Fatal("retry rebased or overwrote stale state")
			}
		})
	}
}
