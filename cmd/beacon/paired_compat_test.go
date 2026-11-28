package main

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/CrossTerra/beacon/internal/approval"
	"github.com/CrossTerra/beacon/internal/config"
	"github.com/CrossTerra/beacon/internal/keychain"
	"github.com/CrossTerra/beacon/internal/localstate"
	"github.com/CrossTerra/beacon/internal/passkey"
)

func legacyInitArgs(extra ...string) []string {
	return append([]string{"init", "--signal-url", checkOrigin, "--handle", "@fixture"}, extra...)
}

func backingCollectorCLI() (*app, *bytes.Buffer, *bytes.Buffer) {
	a, out, stderr := previewCLI()
	a.keychain = nil // exercise the actual private KDBX reader/writer in the fixture home
	a.euid = func() int { return 501 }
	return a, out, stderr
}

func assertPairedCLIRefusal(t *testing.T, args []string, vaultDir, input, reason string) {
	t.Helper()
	a, out, stderr := backingCollectorCLI()
	a.vaultDir = vaultDir
	r := strings.NewReader(input)
	a.stdin = r
	var external bool
	a.tailscale = func(context.Context, ...string) ([]byte, error) {
		external = true
		return nil, errors.New("unexpected Tailscale fixture call")
	}
	a.transport = func([]string) http.RoundTripper {
		external = true
		return signalCheckTransport(func(*http.Request) (*http.Response, error) {
			return nil, errors.New("unexpected provider fixture call")
		})
	}
	a.google.open = func(string) error {
		external = true
		return errors.New("unexpected consent fixture call")
	}
	if code := a.main(t.Context(), args); code == 0 || out.Len() != 0 || stderr.String() != "beacon: "+reason+"\n" ||
		r.Len() != len(input) || external {
		t.Fatalf("%v did not refuse with only its fixed reason before input or external calls", args)
	}
}

func TestPairedSignalRejectsIncompatibleLegacyCommands(t *testing.T) {
	for _, committed := range []bool{false, true} {
		name := "prepared"
		if committed {
			name = "committed"
		}
		t.Run(name, func(t *testing.T) {
			dir, code := signalApplyFixture(t)
			open := enrollSignalApplyFixture(t, dir, code)
			priorKey := open()
			defer clear(priorKey)
			originalDB := readSignalFixture(t, dir, signalDB)
			f := signalVerificationFixture(t)
			exit, _, _ := runSignalApply(t.Context(), t, f, applyArgs(dir, "@fixture"), applyInput(t), func(step string) error {
				if !committed && step == signalConfigSynced {
					return errors.New("fixture stops after durable preparation")
				}
				return nil
			})
			prep, err := localstate.LoadSignalPreparation(dir)
			must(t, err)
			if prep == nil || (exit == 0) != committed || bytes.Equal(originalDB, readSignalFixture(t, dir, signalDB)) == committed {
				t.Fatal("fixture did not reach the required paired state")
			}
			home, err := os.UserHomeDir()
			must(t, err)
			starter := localstate.SignalCollectorPath(dir)
			alias := filepath.Join(home, "starter-alias.yaml")
			must(t, os.Symlink(starter, alias))
			before := fixtureTree(t, home)
			const initReason = "beacon init cannot be used on an instance with paired Signal setup"
			const originReason = "paired Signal setup requires localhost; cannot switch the review origin to Tailscale"
			const starterReason = "the paired Signal collector starter is read-only"
			for _, c := range []struct {
				name, input, reason string
				args                []string
			}{
				{"init default", checkRelayKey, initReason, legacyInitArgs()},
				{"init force", checkRelayKey, initReason, legacyInitArgs("--force")},
				{"init different config", checkRelayKey, initReason, legacyInitArgs("--config", filepath.Join(home, "legacy", "config.yaml"))},
				{"init starter", checkRelayKey, starterReason, legacyInitArgs("--config", starter, "--force")},
				{"Tailscale", "yes", originReason, []string{"setup", "--origin", "tailscale"}},
				{"Tailscale and reset", "yes", originReason, []string{"setup", "--origin", "tailscale", "--reset-passkeys"}},
				{"Slack add", "xoxp-synthetic-slack-key", starterReason, []string{"source", "add", "slack", "--config", starter}},
				{"Slack alias", "xoxp-synthetic-slack-key", starterReason, []string{"source", "add", "slack", "--config", alias}},
				{"GitHub add", "synthetic-github-key", starterReason, []string{"source", "add", "github", "--config", starter, "--repo", "fixture/repo"}},
				{"GitHub import", "yes", starterReason, []string{"source", "add", "github", "--config", starter, "--repo", "fixture/repo", "--from-gh"}},
				{"Jira add", "fixture@example.test\nsynthetic-jira-key", starterReason, []string{"source", "add", "jira", "--config", starter, "--site", "fixture", "--project", "DEMO"}},
				{"Google add", "", starterReason, []string{"source", "add", "google", "--config", starter, "--account", "fixture@example.test", "--open"}},
				{"enrolled reissue", "", passkey.ErrRegistered.Error(), []string{"setup", "--reissue-code"}},
			} {
				t.Run(c.name, func(t *testing.T) {
					assertPairedCLIRefusal(t, c.args, "", c.input+"\n", c.reason)
					if !reflect.DeepEqual(before, fixtureTree(t, home)) {
						t.Fatal("refusal changed the prepared or paired fixture")
					}
				})
			}
			exit, out, _ := runSignalApply(t.Context(), t, signalVerificationFixture(t), applyArgs(dir, "@fixture"), applyInput(t), nil)
			if exit != 0 || strings.Contains(out, "No files were changed") != committed {
				t.Fatal("the refused legacy commands stranded the apply retry")
			}
			assertSignalApplyReaders(t, dir, true)
			got := open()
			defer clear(got)
			if !bytes.Equal(priorKey, got) {
				t.Fatal("the original passkey lost its write vault")
			}
		})
	}
}

func TestPairedTailOriginGuardDoesNotReplayTornJournal(t *testing.T) {
	dir, _ := signalApplyFixture(t)
	if exit, _, _ := runSignalApply(t.Context(), t, signalVerificationFixture(t), applyArgs(dir, "@fixture"), applyInput(t), nil); exit != 0 {
		t.Fatal("fixture could not apply the pair")
	}
	journal := filepath.Join(dir, "journal.log")
	must(t, os.WriteFile(journal, append(readSignalFixture(t, dir, "journal.log"), 'B'), 0o600))
	before := fixtureTree(t, dir)
	assertPairedCLIRefusal(t, []string{"setup", "--origin", "tailscale"}, "", "yes\n",
		"paired Signal setup requires localhost; cannot switch the review origin to Tailscale")
	if !reflect.DeepEqual(before, fixtureTree(t, dir)) {
		t.Fatal("refused origin change repaired the torn journal")
	}
}

func TestPairedSignalInitAndApplyExcludeEachOther(t *testing.T) {
	const busy = "another command is setting up the collector for this instance"
	t.Run("apply owns the state first", func(t *testing.T) {
		dir, _ := signalApplyFixture(t)
		home, err := os.UserHomeDir()
		must(t, err)
		check := func() {
			before := fixtureTree(t, home)
			assertPairedCLIRefusal(t, legacyInitArgs("--force"), "", checkRelayKey+"\n", busy)
			if !reflect.DeepEqual(before, fixtureTree(t, home)) {
				t.Fatal("concurrent init changed the fixture")
			}
		}
		f := signalVerificationFixture(t)
		f.before = func(call string) {
			if call == "app /api/me" {
				// No preparation yet; only the shared state guard can reject init here.
				check()
			}
		}
		code, _, _ := runSignalApply(t.Context(), t, f, applyArgs(dir, "@fixture"), applyInput(t), func(step string) error {
			if step == signalVaultReady {
				check()
			}
			return nil
		})
		if code != 0 {
			t.Fatal("concurrent init prevented the original apply from completing")
		}
		assertSignalApplyReaders(t, dir, true)
	})
	t.Run("init owns the state first", func(t *testing.T) {
		dir, _ := signalApplyFixture(t)
		a, out, stderr := backingCollectorCLI()
		a.stdin = strings.NewReader(checkRelayKey + "\n")
		reached := false
		a.stderr = &collectorPromptHook{Writer: stderr, atPrompt: func() {
			reached = true
			before := fixtureTree(t, dir)
			f := signalVerificationFixture(t)
			code, stdout, errOut := runSignalApply(t.Context(), t, f, applyArgs(dir, "@fixture"), applyInput(t), nil)
			if code == 0 || f.opened || stdout != "" || errOut != "beacon: "+busy+"\n" || !reflect.DeepEqual(before, fixtureTree(t, dir)) {
				t.Fatal("apply crossed an in-progress legacy init")
			}
		}}
		if code := a.main(t.Context(), legacyInitArgs()); code != 0 || !reached || !strings.Contains(out.String(), "Signal key stored") {
			t.Fatal("the winning legacy init did not complete")
		}
		assertLegacyInitCredential(t, a, "")
		f := signalVerificationFixture(t)
		code, _, errOut := runSignalApply(t.Context(), t, f, applyArgs(dir, "@fixture"), applyInput(t), nil)
		if code == 0 || f.opened || !strings.Contains(errOut, "no existing collector config") {
			t.Fatal("apply did not keep the established legacy config")
		}
	})
}

type collectorPromptHook struct {
	io.Writer
	atPrompt func()
}

func (w *collectorPromptHook) Write(b []byte) (int, error) {
	if bytes.HasPrefix(b, []byte("Signal key for ")) {
		w.atPrompt()
	}
	return w.Writer.Write(b)
}

func assertLegacyInitCredential(t *testing.T, a *app, path string) {
	t.Helper()
	cfg, err := a.loadConfig(path)
	must(t, err)
	if cfg.Mode != config.DryRun || len(cfg.Sources) != 0 {
		t.Fatal("legacy collector init did not remain source-free and in dry-run")
	}
	store, err := a.collectorStore(cfg, false)
	must(t, err)
	got, err := store.Get(cfg.KeychainAccount())
	must(t, err)
	defer got.Clear()
	want, err := keychain.FromString(checkRelayKey)
	must(t, err)
	defer want.Clear()
	if !got.Same(want) {
		t.Fatal("legacy collector did not reopen its fixture key")
	}
}

func TestLegacyInitIsIndependentOfOtherStateAndApprovalLocks(t *testing.T) {
	paired, _ := signalApplyFixture(t)
	if code, _, _ := runSignalApply(t.Context(), t, signalVerificationFixture(t), applyArgs(paired, "@fixture"), applyInput(t), nil); code != 0 {
		t.Fatal("fixture could not apply the pair")
	}
	before := fixtureTree(t, paired)
	separate := t.TempDir()
	a, _, _ := backingCollectorCLI()
	a.vaultDir = filepath.Join(separate, "vault")
	a.stdin = strings.NewReader(checkRelayKey + "\n")
	path := filepath.Join(separate, "config.yaml")
	if code := a.main(t.Context(), legacyInitArgs("--config", path)); code != 0 {
		t.Fatal("the paired default state blocked an independent legacy config and vault")
	}
	assertLegacyInitCredential(t, a, path)

	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, "config"))
	t.Setenv("XDG_STATE_HOME", filepath.Join(home, "state"))
	assertPairedCLIRefusal(t, legacyInitArgs(), paired, checkRelayKey+"\n", "beacon init cannot be used on an instance with paired Signal setup")
	legacy, err := localstate.DefaultDir()
	must(t, err)
	a, _, _ = backingCollectorCLI()
	a.stdin = strings.NewReader(checkRelayKey + "\n")
	if code := a.main(t.Context(), legacyInitArgs()); code != 0 {
		t.Fatal("an independent, legacy-only collector could not initialize")
	}
	assertLegacyInitCredential(t, a, "")
	lock, err := approval.LockInstance(legacy)
	must(t, err)
	defer approval.CloseInstance(legacy, lock)
	a.stdin = strings.NewReader(checkRelayKey + "\n")
	if code := a.main(t.Context(), legacyInitArgs("--force")); code != 0 {
		t.Fatal("the approval lock blocked initialization of its unpaired legacy collector")
	}
	if !reflect.DeepEqual(before, fixtureTree(t, paired)) {
		t.Fatal("legacy init changed the other instance")
	}
}

func TestPairedTailOriginGuardRechecksBeforeReplay(t *testing.T) {
	dir, _ := signalApplyFixture(t)
	a, out, stderr := backingCollectorCLI()
	a.stdin = strings.NewReader("yes\n")
	a.tailscale = func(context.Context, ...string) ([]byte, error) {
		t.Fatal("Tailscale discovery ran after apply won the instance lock")
		return nil, nil
	}
	var before map[string]string
	a.now = func() time.Time {
		// The origin command has done its read-only check, but hasn't locked or replayed the journal.
		if code, _, _ := runSignalApply(t.Context(), t, signalVerificationFixture(t), applyArgs(dir, "@fixture"), applyInput(t), nil); code != 0 {
			t.Fatal("fixture failed to publish between the origin checks")
		}
		before = fixtureTree(t, dir)
		return signalSetupTime
	}
	if code := a.main(t.Context(), []string{"setup", "--origin", "tailscale"}); code == 0 || out.Len() != 0 ||
		stderr.String() != "beacon: paired Signal setup requires localhost; cannot switch the review origin to Tailscale\n" ||
		!reflect.DeepEqual(before, fixtureTree(t, dir)) {
		t.Fatal("the origin command missed the newly published preparation")
	}
}

func TestPairedLocalhostPasskeyRecoveryStillWorks(t *testing.T) {
	dir, code := signalApplyFixture(t)
	_ = enrollSignalApplyFixture(t, dir, code)
	if code, _, _ := runSignalApply(t.Context(), t, signalVerificationFixture(t), applyArgs(dir, "@fixture"), applyInput(t), nil); code != 0 {
		t.Fatal("fixture could not apply the pair")
	}
	db := readSignalFixture(t, dir, signalDB)
	a, out, _ := previewCLI() // the fixture never selects a login keystore
	a.euid = func() int { return 501 }
	a.now = func() time.Time { return signalSetupTime }
	if exit := a.main(t.Context(), []string{"setup", "--origin", "localhost", "--reset-passkeys"}); exit != 0 || out.Len() == 0 {
		t.Fatal("paired setup refused explicit localhost passkey recovery")
	}
	open := enrollSignalApplyFixture(t, dir, strings.TrimSpace(out.String()))
	key := open()
	defer clear(key)
	if len(key) != 32 || !bytes.Equal(db, readSignalFixture(t, dir, signalDB)) {
		t.Fatal("localhost passkey recovery did not preserve the unattended pair")
	}
	assertSignalApplyReaders(t, dir, true)
}
