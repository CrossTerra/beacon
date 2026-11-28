package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"

	"github.com/CrossTerra/beacon/internal/actionexec"
	"github.com/CrossTerra/beacon/internal/approval"
	"github.com/CrossTerra/beacon/internal/config"
	"github.com/CrossTerra/beacon/internal/cutover"
	"github.com/CrossTerra/beacon/internal/keychain"
	"github.com/CrossTerra/beacon/internal/localstate"
)

func (a *app) cmdVerifySignal(ctx context.Context, args []string) error {
	fs, state := a.reviewFlags("setup verify-signal")
	collector := fs.String("config", "", "existing collector config to check (if present)")
	origin := fs.String("signal-origin", "", "trusted Signal HTTPS origin, obtained independently of the setup code")
	person := fs.String("person", "", "your Signal @handle or act_ actor ID, obtained independently of either credential")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 0 {
		return errors.New("setup verify-signal takes no positional arguments")
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
	if err := checkSignalPins(&target, *state, *collector); err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	fmt.Fprintf(a.stderr, "Verification only for Signal person %s at %s. Paste credentials provisioned for this origin only.\n", *person, *origin)
	app, relay, err := keychain.ReadSignalPair(ctx, a.stdin, a.stderr, target.Origin)
	if err != nil {
		return err
	}
	defer app.Clear()
	defer relay.Clear()
	if err := actionexec.VerifySignalPair(ctx, target, app, relay, a.transport); err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	_, err = fmt.Fprintln(a.stdout, "Verified distinct Signal approval-app and collector credentials for the intended person. Nothing was saved or activated.")
	return err
}

// Read only the files that can pin identity. No journal replay, instance lock file creation,
// vault access, or service management is needed for this point-in-time check.
func checkSignalPins(t *actionexec.SignalCheckTarget, state, collector string) error {
	dir, err := localstate.Dir(state, false)
	if err == nil {
		review, err := localstate.Load(dir)
		if errors.Is(err, os.ErrNotExist) {
			entries, scanErr := os.ReadDir(dir)
			if scanErr != nil || len(entries) != 0 {
				return errors.New("existing Beacon state does not have a configured localhost review origin")
			}
		} else if err != nil || review.URL() != approval.LocalOrigin {
			return errors.New("this Signal check is only for a Beacon with a localhost review origin")
		}
		s, err := actionexec.LoadSettings(dir)
		if err != nil {
			return errors.New("cannot verify existing approval provider settings")
		}
		if s.Signal != nil {
			if err := pinApprovalApp(t, s.Signal.Origin, s.Signal.Actor, s.Signal.Person); err != nil {
				return err
			}
			if err := t.PinPerson("@" + s.Signal.Handle); err != nil {
				return err
			}
			t.CollectorActor = s.Signal.CollectorActor()
		}
		archive, err := cutover.Load(dir)
		if err != nil {
			return errors.New("cannot verify existing Swift cutover state")
		}
		if old := archive.Signal; old != nil {
			if err := pinApprovalApp(t, old.Origin, old.Actor, old.Person); err != nil {
				return err
			}
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return errors.New("cannot verify the local Beacon state directory")
	}

	path, err := configPath(collector)
	if err != nil {
		return errors.New("cannot locate the collector config")
	}
	f, err := approval.OpenPrivate(path, os.O_RDONLY)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return errors.New("cannot read the private collector config")
	}
	defer f.Close()
	const limit = 1 << 20
	body, err := io.ReadAll(io.LimitReader(f, limit+1))
	if err != nil || len(body) > limit {
		return errors.New("cannot read the private collector config")
	}
	cfg, err := config.Parse(body)
	if err != nil {
		return errors.New("cannot verify existing collector settings")
	}
	u, err := config.SignalURL(cfg.Signal.URL)
	if err != nil || u.Scheme+"://"+u.Host != t.Origin {
		return errors.New("the trusted Signal origin conflicts with the collector config")
	}
	if err := t.PinPerson(cfg.Person.Handle); err != nil {
		return err
	}
	for _, s := range cfg.Sources {
		if s.Kind == "fixture" {
			continue // Fixtures take their item source name from test data, not a provider binding.
		}
		// Providers use SourceName for their items; projection looks up the same name for by on own acts.
		if identity := cfg.Person.Identities[s.SourceName()]; identity != "" {
			t.RelayIdentities = append(t.RelayIdentities, identity)
		}
	}
	return nil
}

func pinApprovalApp(t *actionexec.SignalCheckTarget, origin, actor, person string) error {
	if origin != t.Origin || t.ApprovalActor != "" && t.ApprovalActor != actor {
		return errors.New("the trusted Signal origin or approval actor conflicts with an existing binding")
	}
	t.ApprovalActor = actor
	return t.PinPerson(person)
}
