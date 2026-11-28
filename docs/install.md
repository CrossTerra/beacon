# Install Beacon on macOS or Linux

Use macOS 14 or newer, or Linux with a systemd user manager, on arm64 or x86-64. Put **Go 1.21 or newer** on
`PATH`; a newer Go you already have is fine. The installer builds with **Go 1.24.3**. Go downloads that exact
toolchain if needed and verifies it through its checksum database (`sum.golang.org` by default). If the
download is unavailable (for example offline or with `GOPROXY=off`), install **Go 1.24.3** from
[go.dev/dl](https://go.dev/dl/) and put it on `PATH`. An offline build also needs the module dependencies cached.
Clone your fork and check out the full audited commit in Terminal. No signing, Homebrew, or `sudo` is required.

```sh
./scripts/install.sh --origin localhost
```

The installer refuses tracked edits, builds from `git archive`, and checks `beacon version` for that commit.
It installs `~/.local/bin/beacon`. If it is not on `PATH`, use that absolute path or accept the offered profile
line. On macOS it bootstraps `~/Library/LaunchAgents/dev.beacon.serve.plist` in your GUI domain. On Linux it
enables `beacon.service` in `${XDG_CONFIG_HOME:-$HOME/.config}/systemd/user` with `KillMode=control-group`.
Both start at login. An update builds first, requests a stop, and waits up to 60 seconds for the manager to
report the job stopped, the service PID to exit, and the approval instance lock to become free. A stop command
error (such as launchd's “Operation now in progress”) or a failed status check does not end the wait. If the
installer cannot confirm shutdown, it leaves the installed files unchanged. It checks the Go socket after
starting the new job. If the replacement fails, rollback waits for the same checks before restoring and
restarting the previous Go service. On macOS, bootstrap error 5 gets up to two retries with a short backoff.

### Origin and passkeys

On first install, choose `--origin localhost` or `--origin tailscale`. Install and connect Tailscale on both
devices, enable MagicDNS and HTTPS certificates in your own tailnet, and confirm the Serve command in setup.
On macOS, if `tailscale` is not on `PATH`, the installer and `beacon setup` look for the app's executable at
`/Applications/Tailscale.app/Contents/MacOS/Tailscale`, then `~/Applications/Tailscale.app/Contents/MacOS/Tailscale`.
The standalone or App Store app in either location needs no separate CLI installation. On Linux, put `tailscale`
on `PATH`.

Setup reads the local device's login and name, rejects tags and Funnel, and requires the canonical
origin to serve Beacon exclusively: no other paths on that origin, even if `/` points at Beacon. It does not
overwrite an unrelated proxy on port 443 and verifies the Serve configuration before saving it. There is no fallback to
localhost for credentials registered on the tailnet name. Run `beacon status --review` to check Serve. Do not use
Funnel. Limit access to the machine's port 443 to your devices in a shared tailnet.

Use the printed code at `/passkeys/new` within ten minutes. For approval from a phone over Tailscale,
use the same PRF-capable passkey synced to both devices. If the setup code expired or was already used
and no passkey is enrolled, run this from the audited Beacon checkout in a terminal on the host:

```sh
./scripts/install.sh --reissue-code
```

This is recovery only: it requires a matching, unmodified installer-managed service and a binary that supports
offline reissue. It refuses an enrolled installation. It briefly stops and restarts a running, enabled service;
open review sessions end. It does not start or enable a previously stopped service. Enter the replacement code
at the printed address; do not redirect or log it. If the restart cannot be confirmed, the helper reports the
failure and does not display the code. For an expired **sign-in link**, run `beacon open` instead.

If the helper cannot verify a custom unit but the binary supports reissue, use
`beacon setup --reissue-code` only after deliberately stopping that service. Start it again when ready to register.

When passkey registration fails, the page shows the browser's error name and message, PRF status, and browser
user agent. Only a fixed reason code is logged to `serve.log` on macOS (the systemd user journal on Linux).

Updates never run setup again. To change the origin, stop the service and run `beacon setup --origin …`
explicitly. Confirming removes every passkey and the write vault, but not the journal, unattended credentials
or Signal key. After restarting,
register again and re-enter the write credentials. Changing to localhost does not delete other Tailscale routes.

`beacon open` prints a temporary sign-in link; open it yourself. The collector's `beacon init` and
`beacon source add` commands store credentials in `unattended.kdbx`. The key file is `unattended.keyx` on
macOS (with a Time Machine exclusion). On Linux, setup uses an available Secret Service, otherwise it writes
the key file. Back up the key separately. `beacon vault key-file PATH` writes an exclusive `0600` copy for
KeePassXC. Delete that copy when done. See [the vault instructions](go-approvals.md#unattended-vault).
If setup reports an incomplete vault, restore the matching database or key before continuing; setup will not
generate a replacement beside the remaining file.

### Verify Signal credentials for localhost Beacon

Before first setup or on an existing localhost installation, run this in a terminal:

```sh
beacon setup verify-signal --signal-origin https://signal.example.com --person @your-handle
```

Use the HTTPS origin and your own Signal handle (or `act_…` person ID) from your trusted Signal account,
not from either credential. Paste the `beacon-setup:v1:` code from Signal’s `/beacon` page and then the
separately provisioned collector key at the hidden prompts. The raw collector key contains no origin; only
paste one provisioned for the origin you supplied. If the setup code names a different origin, neither key
is sent. Both keys must match Signal’s API-key format and be different before Beacon makes any request.
Format alone does not verify ownership or scopes. Existing approval and collector settings must agree with
the supplied origin and person.
Use `--state-dir DIR` or `--config PATH` if this installation uses nondefault paths.

The approval-app integration must hold exactly `read` and `write`. The collector must be a different
integration for the same active person with exactly `read`, `write`, and `relay`. `read` lets Beacon find
the supplied key’s grant in Signal’s key list. The relay grant must list exact identities attached to that
person (`github:user:`, `jira:account:`, `slack:user:`, or `email:`), not wildcard prefixes. Neither
integration may be an executor, reconciler, or directory integration. Beacon reads only the pinned Signal
origin; `beacon egress --signal-origin https://signal.example.com` lists those reads without contacting it.

The grant must cover each nonempty `person.identities` binding for an enabled provider source, keyed by
the source’s configured name. Unused entries and test fixtures are not checked. With no such bindings,
success verifies ownership and the grant only, not readiness for an unconfigured data source.

This command **only verifies the credentials you enter**. It does not open the vault, save credentials,
create a collector config, change a passkey, or activate delivery. You can repeat the check without stopping
the service. `beacon init` and `beacon setup --signal` remain separate configuration commands; the verifier
does not run either one.

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
| `slack.post` | Configure the Slack workspace and user in `action-providers.json` and save separate read and write credentials in [Settings](go-approvals.md#slack-posts). The read token does not need `metadata.message:read`. |
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
    WAL. Verification is silent on success; the archive notice appears only when a snapshot is first saved.
    If either changed, stop and keep both copies; do not discard the archive to force a retry.
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
