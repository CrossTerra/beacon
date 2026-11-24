# Install Beacon on macOS or Linux

Use macOS 14 or newer, or Linux with a systemd user manager, on arm64 or x86-64. Install **Go 1.24.3** from
[go.dev/dl](https://go.dev/dl/). Clone your fork and check out the full audited commit in Terminal. There is
no signing identity, Homebrew, or `sudo` in this setup.

```sh
./scripts/install.sh --origin localhost
```

The installer refuses tracked edits, builds from `git archive`, and checks `beacon version` for that commit.
It installs `~/.local/bin/beacon`. If it is not on `PATH`, use that absolute path or accept the offered profile
line. On macOS it bootstraps `~/Library/LaunchAgents/dev.beacon.serve.plist` in your GUI domain. On Linux it
enables `beacon.service` in `${XDG_CONFIG_HOME:-$HOME/.config}/systemd/user` with `KillMode=control-group`.
Both start at login. An update builds first, stops the job, renames the new binary into place and checks the
Go socket when the job starts. If the replacement fails to start, it restores the previous Go binary and job.

### Origin and passkeys

On first install, choose `--origin localhost` or `--origin tailscale`. For Tailscale install it separately on
both devices, enable MagicDNS and HTTPS certificates in your own tailnet, and agree to the printed Serve
command. Setup reads the local device's login and name, rejects tags and Funnel, refuses to overwrite an
unrelated proxy on port 443, and verifies the Serve configuration before saving it. There is no fallback to
localhost for credentials registered on the tailnet name. Run `beacon status --review` to check Serve. Do not use
Funnel. Limit access to the machine's port 443 to your devices in a shared tailnet.

Use the printed code at `/passkeys/new` within ten minutes. For Winton, save the Touch ID passkey in iCloud
Keychain and sign in with the same passkey from the iPhone over Tailscale. With the service stopped, `beacon
setup` will issue another code only before enrollment or after a reset. Updates never run setup again. To
change the origin, stop the service and run `beacon setup --origin …` explicitly. Confirming removes every
passkey and the write vault, but not the journal, unattended credentials or Signal key. After restarting,
register again and re-enter the write credentials. Changing to localhost does not delete other Tailscale routes.

`beacon open` prints a temporary sign-in link; open it yourself. The collector's `beacon init` and
`beacon source add` commands store credentials in `unattended.kdbx`. The key file is `unattended.keyx` on
macOS (with a Time Machine exclusion). On Linux, setup uses an available Secret Service, otherwise it writes
the key file. Back up the key separately. `beacon vault key-file PATH` writes an exclusive `0600` copy for
KeePassXC. Delete that copy when done. See [the vault instructions](go-approvals.md#unattended-vault).

### Service controls

On macOS, from Terminal:

```sh
launchctl print "gui/$(id -u)/dev.beacon.serve"
launchctl bootout "gui/$(id -u)/dev.beacon.serve"
launchctl bootstrap "gui/$(id -u)" "$HOME/Library/LaunchAgents/dev.beacon.serve.plist"
```

On Linux use `systemctl --user status beacon`, `systemctl --user stop beacon`, and `systemctl --user start beacon`.
`beacon status --coverage` shows collector progress, and `beacon logs` prints the collector summary log. Rerun
`scripts/install.sh` at the next audited commit without changing the origin. `scripts/install.sh --uninstall`
stops and removes the service and the binary. It keeps state, credentials, configuration and Tailscale settings.

## One-time cutover from Beacon.app (D16)

Leave the old signed CLI and app in place while draining. Do not run these steps against live services from CI
or an agent session. Keep proposers paused until the last step.

1. **Pause new proposals**, without changing Bosun's route. Resolve or withdraw every pending request, finish
   started work and active runs, and reconcile unknown outcomes by reading back. Return final results to callers.
   Wait for Signal to acknowledge the last Swift record. If anything cannot drain, stay on Swift. Do not repeat
   the operation or copy an old approval into the Go journal.
2. **Quit Swift from its menu.** Do not use socket quit or SIGTERM against a build that predates deferred quit.
   If `launchctl print "gui/$(id -u)/dev.beacon.agent"` shows the retired collector job, run
   `launchctl bootout "gui/$(id -u)/dev.beacon.agent"`. Move any `~/Library/LaunchAgents/dev.beacon.agent.plist`
   out of LaunchAgents so it cannot start at login. Keep the collector configuration and state in place.
3. In the audited fork on the new version run:

   ```sh
   ./scripts/install.sh --migrate-swift --origin tailscale
   ```

   Use localhost instead if wanted. The installer does not quit or relaunch Swift. It checks the old journal
   again after shutdown and refuses unconsumed approvals, starts without results, unclosed runs and undelivered
   records. If Quit produced a final record, return to Swift to finish draining with its old key; then try again.
   The installer archives an owner-only SQLite snapshot including committed WAL as
   `~/Library/Application Support/beacon/swift-archive/journal.sqlite`. The Go journal is strictly newer and
   starts with an empty outbox. No Swift proposal, approval, ID or action is imported or replayed.
4. Register the passkey with the printed link and code. In Signal `/beacon`, **rotate the key of the same app
   actor**, revoking the previous key. Stop the Go job, connect the new code in hidden input, and import the
   existing collector items if you have a collector config:

   ```sh
   launchctl bootout "gui/$(id -u)/dev.beacon.serve"
   ~/.local/bin/beacon setup --signal
   ~/.local/bin/beacon vault import-keychain
   launchctl bootstrap "gui/$(id -u)" "$HOME/Library/LaunchAgents/dev.beacon.serve.plist"
   ```

   Confirm the collector tokens are read-only and distinct from the Swift write grants. If you previously used a
   broad token, issue a new read token and enter it instead of importing. The import reads only the collector
   entries named by its config. macOS may ask once for each old item. It
   does not read the Swift app's vault key or provider write credentials, replace a Go entry, or delete an old
   item. If a read is refused, re-enter the credential with the collector commands or in KeePassXC. The old
   approval-app key is not imported; the new code is verified against the archived actor and person.
5. Configure the same required types in `action-providers.json` and, for runs, `agent-runs.json` as described in
   [approval setup](go-approvals.md). Stop the job to edit the provider or run bindings, then bootstrap it again.
   Open Settings with `beacon open` and save distinct provider read and write tokens with an assertion. Importing
   collector credentials does not enable provider writes. The write tokens are re-entered after enrollment, not
   copied into the unattended vault.
6. Run `beacon act types` at the original socket path and confirm it covers every switched type. Check the
   collector still has its config and cursors, and confirm on stage that the new journal's first record is
   accepted at sequence **1** and a new record under the old journal is refused. Do not generate a fresh write
   with the old app. Once verified, remove Beacon.app, its old `app/` vault files and `act.json`, and remove the
   old items in Keychain Access. Keep `swift-archive/` and all collector state. Resume proposals without changing
   the routes. Rerunning the Go installer should require no Keychain prompt.
