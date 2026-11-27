package main

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"runtime"
	"strings"

	"github.com/CrossTerra/beacon/internal/actionexec"
	"github.com/CrossTerra/beacon/internal/approval"
	"github.com/CrossTerra/beacon/internal/cutover"
	"github.com/CrossTerra/beacon/internal/keychain"
	"github.com/CrossTerra/beacon/internal/localstate"
	"github.com/CrossTerra/beacon/internal/passkey"
	"github.com/CrossTerra/beacon/internal/vault"
)

func (a *app) cmdSetup(ctx context.Context, args []string) error {
	fs, path := a.reviewFlags("setup")
	reset := fs.Bool("reset-passkeys", false, "record a passkey reset and remove the passkey list and write vault")
	origin := fs.String("origin", "", "localhost or tailscale (default: preserve existing origin, else localhost)")
	show := fs.Bool("show-origin", false, "print the configured browser origin without changing it")
	connect := fs.Bool("signal", false, "verify the Signal approval-app setup code in hidden input (service stopped)")
	stopped := fs.Bool("check-stopped", false, "check the instance lock without changing settings")
	reissue := fs.Bool("reissue-code", false, "issue a new passkey setup code without changing settings (service stopped)")
	checkReissue := fs.Bool("check-reissue-code", false, "check whether an existing install can reissue a code, without issuing one")
	if err := fs.Parse(args); err != nil {
		return err
	}
	var exclusive int
	for _, enabled := range []bool{*show, *stopped, *connect, *reissue, *checkReissue} {
		if enabled {
			exclusive++
		}
	}
	if fs.NArg() != 0 || exclusive > 1 || exclusive == 1 && (*reset || *origin != "") {
		return errors.New("use setup --show-origin, --check-stopped, --signal, --check-reissue-code or --reissue-code separately")
	}
	if err := a.refuseRoot(); err != nil {
		return err
	}
	if *origin != "" && *origin != "localhost" && *origin != "tailscale" {
		return errors.New("setup --origin must be localhost or tailscale")
	}
	dir, err := localstate.Dir(*path, exclusive == 0)
	if err != nil {
		return err
	}
	if *checkReissue || *reissue {
		if _, err := localstate.Load(dir); err != nil {
			return err
		}
		if _, err := cutover.Load(dir); err != nil {
			return err
		}
		if *checkReissue {
			return passkey.CheckReissue(dir)
		}
		code, err := passkey.Reissue(dir, a.now())
		if err != nil {
			return err
		}
		_, err = fmt.Fprintln(a.stdout, code)
		return err
	}
	if *show {
		o, err := localstate.Load(dir)
		if err != nil {
			return err
		}
		_, err = fmt.Fprintln(a.stdout, o.URL())
		return err
	}
	if *stopped {
		lock, err := approval.LockInstance(dir)
		if err != nil {
			return err
		}
		return approval.CloseInstance(dir, lock)
	}
	archive, err := cutover.Load(dir)
	if err != nil {
		return err
	}
	if *connect {
		if _, err := localstate.Load(dir); err != nil {
			return err
		}
		lock, err := approval.LockInstance(dir)
		if err != nil {
			return err
		}
		defer approval.CloseInstance(dir, lock)
		fmt.Fprint(a.stderr, "Paste the new code from Signal’s /beacon page (input hidden): ")
		origin, key, err := keychain.ReadSignalSetup(ctx, a.stdin)
		fmt.Fprintln(a.stderr)
		if err != nil {
			return err
		}
		defer key.Clear()
		if err := actionexec.RegisterSignal(ctx, dir, origin, key, archive.Signal, a.transport); err != nil {
			return err
		}
		fmt.Fprintln(a.stdout, "Signal approval app verified. Start the service to resume delivery.")
		return nil
	}
	j, err := approval.OpenJournal(dir, a.now(), archive.Journal)
	if err != nil {
		return err
	}
	defer j.Close()
	old, err := localstate.Load(dir)
	first := errors.Is(err, os.ErrNotExist)
	if err != nil && !first {
		return err
	}
	next := old
	if *origin == "tailscale" {
		next, err = localstate.TailOrigin(ctx, a.tailscale)
		if err != nil {
			return err
		}
	} else if *origin == "localhost" || first {
		next, _ = approval.ParseOrigin(approval.LocalOrigin, "")
	}
	in := bufio.NewReader(a.stdin)
	confirm := func(question string) error {
		fmt.Fprint(a.stderr, question, " [y/N] ")
		if a.stdin != nil {
			answer, _ := in.ReadString('\n')
			if s := strings.TrimSpace(answer); strings.EqualFold(s, "y") || strings.EqualFold(s, "yes") {
				return nil
			}
		}
		return errors.New("setup was not confirmed")
	}
	changed := !first && old != next
	if changed {
		if err := confirm("Change origin? This removes every registered passkey and destroys the write vault. Re-register and re-enter the write credentials. The journal and unattended vault stay."); err != nil {
			return err
		}
	}
	create := vault.Setup
	if a.keychain != nil { // fixture, not a login service or a D-Bus session
		create = func(_ context.Context, dir string) error { return vault.Create(dir) }
	}
	if err := create(ctx, dir); err != nil && !errors.Is(err, vault.ErrExists) {
		return err
	}
	if next.Secure() && *origin == "tailscale" {
		if err := localstate.EnsureServe(ctx, next, a.tailscale, confirm); err != nil {
			return err
		}
	}
	if *reset || changed {
		if err := passkey.Reset(dir, a.now(), j.RecordLocal); err != nil {
			return err
		}
		if err := vault.RemoveWriteCredentials(dir); err != nil {
			return err
		}
		if changed {
			if err := j.RecordLocal("origin_changed", a.now()); err != nil {
				return err
			}
		}
	}
	code, err := passkey.IssueLocked(dir, a.now())
	if err != nil {
		return err
	}
	if first || changed {
		if err := localstate.Save(dir, next); err != nil {
			return err
		}
	}
	fmt.Fprintf(a.stderr, "Unattended vault key: %s.\n", vault.KeyStore(dir))
	if runtime.GOOS == "darwin" {
		fmt.Fprintln(a.stderr, "The key file is excluded from Time Machine; back it up separately.")
	} else if vault.KeyStore(dir) != "Secret Service" {
		fmt.Fprintln(a.stderr, "No Secret Service answered at vault creation; keep the key file separate from backups.")
	}
	fmt.Fprintf(a.stderr, "Start the service and register at %s/passkeys/new within ten minutes.\n", next.URL())
	_, err = fmt.Fprintln(a.stdout, code)
	return err
}

// The live cutover is an explicit offline operation. No executable or Keychain item from Swift is used.
func (a *app) cmdMigrateSwift(ctx context.Context, args []string) error {
	fs, path := a.reviewFlags("migrate-swift")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if runtime.GOOS != "darwin" || fs.NArg() != 0 {
		return errors.New("migrate-swift runs only on macOS and takes no positional arguments")
	}
	if err := a.refuseRoot(); err != nil {
		return err
	}
	dir, err := localstate.Dir(*path, false)
	if err != nil {
		return err
	}
	lock, err := approval.LockInstance(dir)
	if err != nil {
		return err
	}
	defer approval.CloseInstance(dir, lock)
	archive, err := cutover.Migrate(ctx, dir, "/usr/bin/sqlite3")
	if err == nil && archive.Journal != "" {
		_, err = io.WriteString(a.stdout, "Archived "+archive.Journal+". No Swift actions will be replayed.\n")
	}
	return err
}
