package main

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"runtime"
	"slices"
	"strings"

	"github.com/CrossTerra/beacon/internal/approval"
	"github.com/CrossTerra/beacon/internal/config"
	"github.com/CrossTerra/beacon/internal/keychain"
	"github.com/CrossTerra/beacon/internal/localstate"
	"github.com/CrossTerra/beacon/internal/slack"
	"github.com/CrossTerra/beacon/internal/vault"
)

func collectorBindings(cfg *config.Config) map[string]vault.Binding {
	u, _ := config.SignalURL(cfg.Signal.URL)
	account := cfg.KeychainAccount() // historical account name is also the collector's stable slot
	out := map[string]vault.Binding{account: {Service: "signal-collector", Origin: u.Scheme + "://" + u.Host, Account: account, Use: "signal"}}
	for _, s := range cfg.Sources {
		if s.IsGoogle() {
			for _, slot := range []string{keychain.GoogleClientAccount(s.Account), keychain.GoogleRefreshAccount(s.Account)} {
				out[slot] = vault.Binding{Service: "google", Origin: "https://oauth2.googleapis.com", Account: slot, Use: "read"}
			}
			continue
		}
		slot := keychain.SourceAccount(s.SourceName())
		switch s.Kind {
		case "github":
			host := strings.ToLower(s.Host)
			if host == "" {
				host = "api.github.com"
			}
			out[slot] = vault.Binding{Service: "github-collector", Origin: "https://" + host, Account: slot, Use: "read"}
		case "jira":
			out[slot] = vault.Binding{Service: "jira", Origin: "https://" + s.Site + ".atlassian.net", Account: slot, Use: "read"}
		case "slack":
			out[keychain.SlackAccount(s.Workspace, s.User)] = vault.Binding{Service: "slack-collector", Origin: slack.Base,
				Account: "slack:user:" + s.User, Workspace: s.Workspace, Use: "read"}
		}
	}
	return out
}

func (a *app) collectorStore(cfg *config.Config, create bool) (keychain.Store, error) {
	if a.keychain != nil { // in-memory fixtures, never the system Keychain
		return a.keychain()
	}
	dir, err := localstate.Dir(a.vaultDir, create)
	if err != nil {
		return nil, err
	}
	return keychain.Unattended{Dir: dir, Bindings: collectorBindings(cfg)}, nil
}

func (a *app) cmdVault(ctx context.Context, args []string) error {
	if len(args) == 0 {
		return errors.New("usage: beacon vault import-keychain | beacon vault key-file PATH")
	}
	if err := a.refuseRoot(); err != nil {
		return err
	}
	fs, path := a.reviewFlags("vault " + args[0])
	if args[0] == "key-file" {
		if err := fs.Parse(args[1:]); err != nil {
			return err
		}
		if fs.NArg() != 1 {
			return errors.New("usage: beacon vault key-file [--state-dir DIR] PATH")
		}
		dir, err := localstate.Dir(*path, false)
		if err != nil {
			return err
		}
		return vault.ExportKey(dir, fs.Arg(0))
	}
	if args[0] != "import-keychain" || runtime.GOOS != "darwin" && a.legacyRead == nil {
		return errors.New("vault import-keychain is only available on macOS")
	}
	configPath := fs.String("config", "", "collector config naming the legacy items")
	if err := fs.Parse(args[1:]); err != nil {
		return err
	}
	if fs.NArg() != 0 {
		return errors.New("vault import-keychain takes no positional arguments")
	}
	cfg, err := a.loadConfig(*configPath)
	if err != nil {
		return err
	}
	dir, err := localstate.Dir(*path, false)
	if err != nil {
		return err
	}
	if _, err := localstate.Load(dir); err != nil {
		return err
	}
	lock, err := approval.LockInstance(dir)
	if err != nil {
		return err
	}
	defer approval.CloseInstance(dir, lock)
	read := a.legacyRead
	if read == nil {
		if !isTerminal(a.stdin) {
			return errors.New("run the one-time Keychain import yourself in a terminal with the service stopped")
		}
		if err := a.confirm("Import the configured collector items? GitHub and Slack must report read-only grants. [y/N] ", false); err != nil {
			return err
		}
		read = keychain.LegacyRead
	}
	bindings := collectorBindings(cfg)
	slots := make([]string, 0, len(bindings))
	for slot := range bindings {
		slots = append(slots, slot)
	}
	slices.Sort(slots)
	store := keychain.Unattended{Dir: dir, Bindings: bindings}
	var failures int
	for _, slot := range slots {
		if s, err := store.Get(slot); err == nil {
			s.Clear()
			continue // an existing Go credential always wins
		} else if !errors.Is(err, keychain.ErrNotFound) {
			return err
		}
		secret, err := read(ctx, slot)
		if err != nil {
			fmt.Fprintf(a.stderr, "Could not import collector item %s; re-enter it if needed.\n", slot)
			failures++
			continue
		}
		binding := bindings[slot]
		switch binding.Service {
		case "github-collector":
			err = a.checkGitHubReadGrant(ctx, binding.Origin, secret, false)
		case "slack-collector":
			var id slack.Identity
			id, err = a.checkSlackReadGrant(ctx, secret)
			if err == nil && (id.Workspace != binding.Workspace || "slack:user:"+id.User != binding.Account) {
				err = errors.New("the Slack token is for a different workspace or user")
			}
		}
		if err != nil {
			secret.Clear()
			fmt.Fprintf(a.stderr, "Skipped collector item %s: %v.\n", slot, err)
			failures++
			continue
		}
		err = keychain.ImportBound(dir, binding, secret)
		secret.Clear()
		if err != nil {
			return err
		}
		fmt.Fprintf(a.stdout, "Imported collector item %s.\n", slot)
	}
	fmt.Fprintf(a.stdout, "The old Keychain items are unchanged. Verify the collector before removing them.\nVault: %s\n", filepath.Join(dir, "unattended.kdbx"))
	if failures != 0 {
		return fmt.Errorf("%d collector items could not be imported", failures)
	}
	return nil
}
