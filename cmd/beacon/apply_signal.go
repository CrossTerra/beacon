package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/CrossTerra/beacon/internal/actionexec"
	"github.com/CrossTerra/beacon/internal/approval"
	"github.com/CrossTerra/beacon/internal/config"
	"github.com/CrossTerra/beacon/internal/keychain"
	"github.com/CrossTerra/beacon/internal/localstate"
	"github.com/CrossTerra/beacon/internal/passkey"
	"github.com/CrossTerra/beacon/internal/vault"
)

var errSignalApplyChanged = errors.New("offline Signal setup found changed or conflicting state; leave the files in place and keep the service stopped")

const (
	signalConfigReady   = "config candidate synced"
	signalConfigRenamed = "config published"
	signalConfigSynced  = "config directory synced"
	signalVaultReady    = "vault candidate synced"
	signalVaultRenamed  = "vault published"
	signalVaultSynced   = "vault directory synced"
)

func (a *app) cmdApplySignal(ctx context.Context, args []string) error {
	fs, state := a.reviewFlags("setup apply-signal")
	origin := fs.String("signal-origin", "", "trusted Signal HTTPS origin, obtained independently of both credentials")
	person := fs.String("person", "", "intended Signal @handle or act_ actor ID, obtained independently of both credentials")
	fs.SetOutput(io.Discard) // a mistyped flag can contain pasted credential data
	if err := fs.Parse(args); errors.Is(err, flag.ErrHelp) {
		fs.SetOutput(a.stderr)
		fs.Usage()
		return nil
	} else if err != nil || fs.NArg() != 0 {
		return errors.New("use setup apply-signal --signal-origin HTTPS_ORIGIN --person SELECTOR [--state-dir DIR]; credentials go in hidden prompts only")
	}
	if err := a.refuseRoot(); err != nil {
		return err
	}
	if _, err := actionexec.SignalVerificationEgress(*origin); err != nil {
		return err
	}
	target := actionexec.SignalCheckTarget{Origin: *origin}
	if err := target.PinPerson(*person); err != nil {
		return err
	}
	dir, err := localstate.Dir(*state, false)
	if err != nil {
		return errors.New("initialize the private Beacon instance before applying Signal setup")
	}
	lock, err := approval.LockOffline(dir)
	if err != nil {
		if errors.Is(err, approval.ErrLocked) {
			return errors.New("stop the Beacon service before applying Signal setup; the instance lock is held or changed")
		}
		return errors.New("could not lock the existing private Beacon instance")
	}
	defer lock.Close()
	collectorLock, err := localstate.LockCollectorSetup(dir)
	if err != nil {
		return err
	}
	defer collectorLock.Close()
	journal, localBase, err := signalApplyBase(lock)
	if err != nil {
		return err
	}
	pending, err := localstate.LoadSignalPreparation(dir)
	if err != nil {
		return err
	}
	update, err := vault.BeginSignalPair(lock)
	if err != nil {
		return fmt.Errorf("cannot open the existing unattended vault for Signal apply: %w", signalApplyVaultError(err))
	}
	defer update.Close()
	vaultBase := update.Base()
	vaultDigest := hex.EncodeToString(vaultBase[:])
	var expected *vault.SignalPair
	if pending != nil {
		if pending.LocalBase != localBase || pending.Journal != journal || pending.Origin != target.Origin ||
			target.PinPerson(pending.Person) != nil || target.PinPerson("@"+pending.Handle) != nil {
			return errSignalApplyChanged
		}
		target.ApprovalActor, target.CollectorActor = pending.ApprovalActor, pending.CollectorActor
		pair := pending.Pair()
		expected = &pair
	}
	complete, err := update.Existing(expected)
	if err != nil || pending != nil && !complete && pending.VaultBase != vaultDigest {
		return errSignalApplyChanged
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	fmt.Fprintln(a.stderr, "Offline Signal connection: Beacon will verify both keys before saving them. Paste only credentials for the trusted origin you supplied.")
	app, collector, err := keychain.ReadSignalPair(ctx, a.stdin, a.stderr, target.Origin)
	if err != nil {
		return err
	}
	defer app.Clear()
	defer collector.Clear()
	verified, err := actionexec.CheckSignalPair(ctx, target, app, collector, a.transport)
	if err != nil {
		return err
	}
	plan := localstate.SignalPreparation{Version: 1, Origin: target.Origin, Person: verified.Person, Handle: verified.Handle,
		ApprovalActor: verified.ApprovalActor, CollectorActor: verified.CollectorActor, Journal: journal, LocalBase: localBase, VaultBase: vaultDigest}
	if pending != nil {
		plan.VaultBase = pending.VaultBase
		if plan != *pending {
			return errSignalApplyChanged
		}
	}
	check := func(expect *localstate.SignalPreparation) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		_, base, err := signalApplyBase(lock)
		current, readErr := localstate.LoadSignalPreparation(dir)
		if err != nil || readErr != nil || base != localBase ||
			(expect == nil) != (current == nil) || expect != nil && current != nil && *expect != *current {
			return errSignalApplyChanged
		}
		return nil
	}
	noChange, err := keychain.CommitSignalPair(ctx, update, plan.Pair(), app, collector, vault.SignalPairCommit{
		Prepare: func(exists bool) error {
			if complete != exists {
				return errSignalApplyChanged
			}
			if err := check(pending); err != nil {
				return err
			}
			if pending == nil {
				if err := localstate.PrepareSignal(lock, plan, func() error {
					if err := a.signalApplyStep(signalConfigReady); err != nil {
						return err
					}
					return check(nil)
				}, func() error { return a.signalApplyStep(signalConfigRenamed) }); err != nil {
					return err
				}
			} else if err := approval.FlushDirectory(dir); err != nil {
				return err // an earlier rename might not have been flushed
			}
			return a.signalApplyStep(signalConfigSynced)
		},
		Check: func() error {
			if !complete {
				if err := a.signalApplyStep(signalVaultReady); err != nil {
					return err
				}
			}
			return check(&plan)
		},
		Renamed: func() error { return a.signalApplyStep(signalVaultRenamed) },
	})
	if err != nil {
		return signalApplyCommitError(err)
	}
	if noChange {
		_, err := fmt.Fprintln(a.stdout, "Signal is already connected with this exact pair. No files were changed or services started.")
		return err
	}
	if err := a.signalApplyStep(signalVaultSynced); err != nil {
		return signalApplyCommitError(err)
	}
	_, err = fmt.Fprintln(a.stdout, "Signal connected. Both keys were saved together. No service was started; the collector starter is dry-run with no sources.")
	return err
}

func (a *app) signalApplyStep(step string) error {
	if a.signalApplyBoundary != nil {
		return a.signalApplyBoundary(step)
	}
	return nil
}

func signalApplyVaultError(err error) error {
	for _, code := range []error{vault.ErrPartial, vault.ErrKeyMissing, vault.ErrUnreadable, vault.ErrLocked, vault.ErrAmbiguous} {
		if errors.Is(err, code) {
			return code
		}
	}
	return errors.New("vault is unavailable or not private")
}

func signalApplyCommitError(err error) error {
	switch {
	case errors.Is(err, vault.ErrConflict), errors.Is(err, vault.ErrAmbiguous), errors.Is(err, os.ErrExist),
		errors.Is(err, approval.ErrLocked), errors.Is(err, errSignalApplyChanged):
		return errSignalApplyChanged
	case errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded):
		return err
	default:
		return errors.New("could not confirm Signal setup; keep the service stopped and retry with the same settings and keys")
	}
}

func signalApplyBase(lock *approval.OfflineLock) (string, string, error) {
	if err := lock.Check(); err != nil {
		return "", "", err
	}
	dir := lock.Dir()
	if os.Getenv("BEACON_CONFIG") != "" {
		return "", "", errors.New("a custom collector config is not supported by offline Signal setup")
	}
	defaultConfig, err := config.DefaultPath()
	if err != nil {
		return "", "", errors.New("cannot locate the standard collector config")
	}
	for _, path := range []string{defaultConfig, filepath.Join(dir, "config.yaml")} {
		if _, err := os.Lstat(path); !errors.Is(err, os.ErrNotExist) {
			return "", "", errors.New("offline Signal setup requires no existing collector config; keep the existing config unchanged")
		}
	}
	if _, err := approval.CheckPrivate(filepath.Join(dir, "run"), os.ModeDir|0o700); err != nil && !errors.Is(err, os.ErrNotExist) {
		return "", "", errors.New("offline Signal setup requires private service state")
	}
	for _, name := range []string{"swift-archive", "app", "tether.session", "tether.lock", "supervisor.session", "run/act.sock"} {
		if _, err := os.Lstat(filepath.Join(dir, name)); !errors.Is(err, os.ErrNotExist) {
			return "", "", errors.New("offline Signal setup does not support cutover or unconfirmed service shutdown state")
		}
	}
	o, origin, err := localstate.LoadSnapshot(dir)
	if err != nil || o.URL() != approval.LocalOrigin {
		return "", "", errors.New("offline Signal setup requires an existing localhost review origin")
	}
	providers, err := actionexec.UnboundSettingsDigest(dir)
	if err != nil {
		return "", "", err
	}
	history, err := approval.ReadSetupHistory(lock)
	if err != nil {
		return "", "", err
	}
	passkeys, err := passkey.CheckSetupState(dir, history.VaultHash)
	if err != nil {
		return "", "", err
	}
	h := sha256.New()
	for _, d := range [][32]byte{origin, providers, history.Digest, passkeys} {
		h.Write(d[:])
	}
	return history.ID, hex.EncodeToString(h.Sum(nil)), nil
}
