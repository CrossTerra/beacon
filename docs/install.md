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
command. Setup reads the local device's login and name, rejects tags and Funnel, and requires the canonical
origin to serve Beacon exclusively: no other paths on that origin, even if `/` points at Beacon. It does not
overwrite an unrelated proxy on port 443 and verifies the Serve configuration before saving it. There is no fallback to
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
If setup reports an incomplete vault, restore the matching database or key before continuing; setup will not
generate a replacement beside the remaining file.

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
stops the job by its service identity, even if its unit file was removed. It removes the unit and the binary
recorded by this installer; an unrecognized binary or symlink stays in place. It keeps state, credentials,
configuration and Tailscale settings.

### Approval handler inventory

The Go service has these handlers. `beacon act types` lists the types enabled by the running service.

| Type | How to enable it |
| --- | --- |
| `github.merge` | Configure the GitHub account in `action-providers.json` and save separate read and write credentials in [Settings](go-approvals.md#provider-credentials-and-github). |
| `slack.post` | Configure the Slack workspace and user in `action-providers.json` and save separate read and write credentials in [Settings](go-approvals.md#slack-posts). |
| `agent.run` | With the service stopped, create `agent-runs.json` (`0600`) in the service state directory with the absolute path to OpenCode, the allowed models, and the named checkouts. Restart the service to enable it. See the [example configuration](go-approvals.md#agent-runs-and-checkouts). Runs need their own passkey assertion, not a provider token. |

## One-time cutover from Beacon.app (D16)

Leave the old signed CLI and app in place while draining. Do not run these steps against live services from CI
or an agent session. Keep proposers paused until the last step.

If `~/Library/Application Support/beacon/act.json` remains but there is no Swift journal (`app/journal.sqlite`)
or unfinished tether session, use the ordinary installer without `--migrate-swift`. Go leaves this old client
trust file untouched; you can remove just `act.json` once the old CLI and app are retired. If a Swift journal is
missing unexpectedly, recover it before switching.

1. **Inventory the switched types while the old app and key still work.** Record the enabled types with the old
   `beacon act types` and check which of `github.merge`, `slack.post`, and `agent.run` Bosun routes to Beacon.
   Compare them with the audited Go build and the [handler inventory](#approval-handler-inventory); have the
   required account and checkout configuration ready before proceeding. Do not revoke the old Signal key if any
   required handler is missing. **Pause new proposals** without changing
   Bosun's route. Resolve or withdraw every pending request, finish
   started work and active runs, and reconcile unknown outcomes by reading back. Return final results to callers.
   Wait for Signal to acknowledge the last Swift record. If anything cannot drain, stay on Swift. Do not repeat
   the operation or copy an old approval into the Go journal.
2. **Quit Swift from its menu.** Do not use socket quit or SIGTERM against a build that predates deferred quit.
   Remove Beacon.app from Login Items and disable any custom startup job or other copy. If `launchctl print
   "gui/$(id -u)/dev.beacon.agent"` shows the retired collector job, run
   `launchctl bootout "gui/$(id -u)/dev.beacon.agent"`. Check `user/$(id -u)/dev.beacon.agent` too and boot it out
   there if loaded. Keep the collector configuration and state in place.
3. In the audited fork on the new version run:

   ```sh
   ./scripts/install.sh --migrate-swift --origin tailscale
   ```

   Use localhost instead if wanted. The installer does not quit or relaunch Swift. On macOS it checks both launchd
   domains even on an ordinary upgrade. Before snapshotting it moves the unloaded plist out of LaunchAgents to
   `~/Library/dev.beacon.agent.plist.disabled`. For a Swift cutover it also renames the stopped installed bundle's
   `Contents/MacOS/Beacon` executable to `Beacon.disabled`. It checks again before starting Go. The disabled
   paths stay parked even after a failed install. If draining failed before
   archiving, with Go stopped and the old key still valid you can move the executable back and finish in Swift.
   The journal must have no pending work, starts without results, unclosed runs or undelivered records.
   The installer archives an owner-only SQLite snapshot including committed WAL as
   `~/Library/Application Support/beacon/swift-archive/journal.sqlite`. The Go journal is strictly newer and
   starts with an empty outbox. No Swift proposal, approval, ID or action is imported or replayed. Before handoff
   and on retry the installer checks the archive checksum and compares it with the closed source including committed
   WAL. If either changed, stop and keep both copies; do not discard the archive to force a retry.
4. Register the passkey with the printed link and code. **Before rotating**, confirm the Go build covers the type
   inventory in step 1 and that the required configuration is ready. Then in Signal `/beacon`, **rotate the key of
   the same app actor**, revoking the previous key. Stop the Go job, connect the new code in hidden input, and import the
   existing collector items if you have a collector config:

   ```sh
   launchctl bootout "gui/$(id -u)/dev.beacon.serve"
   ~/.local/bin/beacon setup --signal
   ~/.local/bin/beacon vault import-keychain
   launchctl bootstrap "gui/$(id -u)" "$HOME/Library/LaunchAgents/dev.beacon.serve.plist"
   ```

   The import refuses GitHub and Slack grants that are not demonstrably read-only. If denied, issue a new read
   token and use the [source setup commands](commands.md#beacon-source-add); for a new GitHub fine-grained PAT
   follow the `--new-read-token` instructions. The import reads only collector entries named by the config.
   macOS may ask once for each old item. Import does not read the Swift app's vault key or provider write
   credentials, replace a Go entry, or delete an old item. The old approval-app key is not imported; the new code
   is verified against the archived actor and person.
5. With the job stopped, configure the provider bindings in `action-providers.json` and, to enable `agent.run`, put
   `agent-runs.json` in the same state directory, as described in the [handler inventory](#approval-handler-inventory).
   Bootstrap the job again.
   Open Settings with `beacon open` and save distinct provider read and write tokens with an assertion. Importing
   collector credentials does not enable provider writes. The write tokens are re-entered after enrollment, not
   copied into the unattended vault.
6. Run `beacon act types` at the original socket path and confirm it covers every switched type. Check the
   collector still has its config and cursors. On **stage only**, with an old journal already acknowledged for
   this actor and no other proposals entering the new journal:

   - Submit one controlled `beacon act propose < stage-proposal.json` for a fixture PR you own. Do not approve it.
     Confirm Signal accepts the proposal as a new `201` record at `beacon:<new jrn_…>:1`.
   - Using the **rotated key for that same app actor**, submit a valid, synthetic `app.action.proposed` to stage's
     `POST /api/app-records` with the archived journal ID and its next unused sequence (last acknowledged plus one).
     Follow the [Signal record contract](https://github.com/CrossTerra/signal/blob/main/docs/approval-app.md#writing-a-record):
     use a fresh proposal ID, current time with a valid expiry, matching stage person, valid target, correctly salted
     `params_hash`, and a fresh replay nonce. Expect `409 conflict` with `details.reason: "journal_order"`. An exact replay, a revoked
     key, or an invalid body does not test ordering. Keep the key out of shell history and command arguments.
   - Withdraw the controlled proposal. Do not launch Swift or make a provider write to test rejection.

   Once verified, remove Beacon.app, its old `app/` vault files and `act.json`, and remove the
   old items in Keychain Access. Keep `swift-archive/` and all collector state. Resume proposals without changing
   the routes. Rerunning the Go installer should require no Keychain prompt.
