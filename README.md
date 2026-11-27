# Beacon

Beacon runs on macOS 14+ and on Linux with systemd (arm64 or x86-64) as one Go binary. Its collector reads
GitHub, Jira, Gmail, Calendar, Drive and Slack. Your rules decide which changes and fields reach Signal.
Agents can propose actions over a local socket; only you can approve them, with a passkey on Beacon's review page.

## Install or update

Use your own fork. Audit the exact commit and keep configurations, credentials and state outside the checkout.
Put **Go 1.21 or newer** on `PATH`. The installer builds with **Go 1.24.3**, which Go fetches and verifies if
needed. No signing identity, Xcode project, Homebrew, or root installation is needed.

```sh
./scripts/install.sh --origin localhost
```

Use `--origin tailscale` for review on your Mac and phone through private Tailscale Serve. Follow the printed
registration link and enter the one-time code. `beacon open` prints a one-time sign-in link; proposals return a
`review_url`. See [installation and Swift cutover](docs/install.md) before replacing a previous Beacon.app.

On update, review the diff and run `./scripts/install.sh` again from the new audited commit. Passkeys, credentials,
pending work, and collector cursors stay in place. `./scripts/install.sh --uninstall` stops the job and removes the
job definition and binary, not your state.

## Configure the collector

Start with sending off. Enter credentials directly at the terminal, in the browser consent flow, or in Beacon's
Settings; never paste them into an agent session. Provider write tokens are separate from unattended read tokens.

```sh
beacon init --signal-url https://signal.example.com --handle @you
beacon egress
beacon rules check
beacon run --first
```

Use [source setup](docs/commands.md#beacon-source-add), [rules](docs/rules.md) and the local dry-run summary to
verify scope, fields and audience. Approve the first collection with `beacon accept-first` before turning on
`mode: send`. Restart the user service if the collector config did not yet exist at service startup. The
collector's Signal key is separate from the approval app key used for journal records.

## For maintainers and audit agents

Read [AGENTS.md](AGENTS.md), the [trust model](docs/trust-model.md), and the [audit brief](docs/audit.md).
Work with fixture credentials and temporary state. Only the person runs the real installer, connects live
providers, or imports from the login Keychain. Report the fork URL, full audited commit ID and relevant file paths
before proposing a live installation or update.

- [Approvals](docs/approvals.md) and [supervision](docs/supervision.md)
- [Commands](docs/commands.md), [configuration](docs/configuration.md) and [sources](docs/sources.md)
- [Architecture](docs/architecture.md), [events](docs/events.md) and [development gates](docs/development.md)
- [Plans](plans/README.md) and [adding sources](docs/adapters.md)
