# Beacon

If someone gave you this repository's Git URL and asked you to install Beacon, this brief is for you, their
installing LLM. Your job is to establish that the exact software they will run meets the [trust model](docs/trust-model.md).
Audit it before installation. Tell the person in plain language what it reads, where it sends data, what it can
do, and what blocks installation. Give them the fork URL, the full audited commit ID and `file:line` evidence.
Follow the [audit checklist](docs/audit.md) and the [install and signing guide](docs/signing.md).

Beacon runs on the person's Mac. Its Go collector reads GitHub, Jira, Gmail, Google Calendar, Google Drive and
Slack. Rules owned by the person select changes and the fields that reach [Signal](#signal). The
[macOS menu-bar app](docs/development.md#macos-app) runs the bundled collector through [tether](docs/tether.md)
while the app is open. Signed, it takes proposals over an authenticated local socket into its journal and shows
them in its [approval window](docs/approvals.md). With a GitHub account in its Settings it merges approved pull
requests, and it records what it did in Signal.

## Quick start

From a checkout of the person's fork, with no changes to tracked files, the person runs, in their own Terminal:

```bash
scripts/install.sh
```

It builds `Beacon.app` from that commit, signs it with their code-signing identity, installs it in
`~/Applications`, points `~/.local/bin/beacon` at its CLI, removes the old collector LaunchAgent and opens the
app. Then, in Beacon's **Settings…**:

- **GitHub:** **Create a token on GitHub** opens GitHub's page for a classic token with the `repo` scope, for their
  own account; they paste it back. If the organization uses SAML single sign-on, they click **Configure SSO →
  Authorize** next to the new token. A fine-grained token also works: the organization as resource owner, with
  Metadata read, Contents and Pull requests read and write, and Checks, Commit statuses and Administration read
  ([which token](docs/approvals.md#merges)).
- **Signal:** they paste the Beacon setup code from Signal's `/beacon` page.

See [installing and signing](docs/signing.md).

## Install from the person's fork

1. **Fork and clone.** Use the person's existing fork if they have one. Otherwise fork the supplied repository
   into their own GitHub account, then clone the fork with their authorized Git access. Keep `origin` pointing
   at that fork and record the supplied repository as `upstream`. Keep the config, private rules, downloaded
   OAuth files, credentials and state outside the clone. Record the fork URL and full commit ID
   (`git rev-parse --verify HEAD^{commit}`).
2. **Audit.** Read [AGENTS.md](AGENTS.md) and the [trust model](docs/trust-model.md). Review source,
   dependencies, the build and install scripts, the intended configuration and any existing state you are asked to
   reuse. Work from code and synthetic fixtures. Keep credentials out of the agent session.
3. **Report and get agreement.** Give the person the report before any installation. Resolve blocking findings
   and audit the resulting commit. The person explicitly approves the full commit ID and the proposed setup.
   A checkout at a different commit needs its own approval.
4. **Have the person install.** They run `scripts/install.sh` from a checkout of the approved commit
   ([installing and signing](docs/signing.md)). It builds, signs with their identity, verifies and installs.
5. **Configure with sending off.** The person enters credentials directly in their terminal or Google's
   consent page. Use the workflow below, starting with `mode: dry-run` and rules that keep items local.
   The person reviews egress, audiences and sample output before allowing sends or starting a background job.

Agents use `go build`, `go test` and `scripts/install.sh --dry-run`, with fixtures and in-memory credentials. Only
the person runs `scripts/install.sh` or performs operations that use the login keychain, including credential
setup, live dry runs and any keychain repair.

## Configure and try ingestion

The person runs these commands using the verified signed CLI. Use a private config directory outside the fork.
`beacon init` creates an empty rules file, a state directory and a dry-run config. It prompts for the Signal
integration key with echo off. See [configuration](docs/configuration.md#the-signal-key) for its scopes and
the identity prefixes needed for attribution.

```sh
beacon init --signal-url https://signal.example.com --handle @you
beacon egress
beacon rules check
```

Choose the accounts, repositories, Jira projects and Slack channels with the person, then enable only those
sources. [Sources](docs/sources.md) lists what each reads and how to issue and revoke a credential.
[`beacon source add`](docs/commands.md#beacon-source-add) documents the GitHub, Jira, Google and Slack setup
commands. Prefer a narrowly scoped credential entered at the terminal prompt. Review the setup destinations
before entering it, then run `beacon egress` again after changing sources. Google consent opens in the person's
browser. Slack setup calls `auth.test` to verify the user token. `beacon key set` checks a replacement Signal key
with `GET /api/me` before storing it.

Write the person's [ingestion rules](docs/rules.md) with them. Start at `never`, put specific rules before broad
ones and preview each proposed field with synthetic fixtures. Add a narrow `hold` rule for local review before
proposing a `send` rule. A `hold` puts the full item in the local review queue during a send pass. Approval from
that queue is planned. A personal body requires sealing and a personal subject requires its restricted prefix
in Signal. Sealed data reaches Signal over TLS as plaintext and Signal encrypts it at rest. Confirm who can
read the plain envelope and the sealed part before approving a rule.

```sh
beacon rules check
beacon run --first
beacon explain 'ITEM_KEY'
```

`run --first` collects open state and prints the candidate decisions and a summary locally. A live dry run
contacts source APIs and may print private data in the terminal. Keep that output local. After the person
accepts the reviewed rules and each named source's completed summary, they can use `beacon accept-first`
with those source names, change to `mode: send` and try `beacon run --once`. A standalone preview can always be
forced with `beacon run --once --dry-run`. `beacon outbox` lists send outcomes and `--bodies` shows the stored
request bytes. `beacon status --coverage` shows coverage recorded by the continuous send loop.

The collector LaunchAgent installer is retired. Inspect or remove an existing job with
[`beacon service`](docs/commands.md#beacon-service). For continuous collection on macOS, the person opens the
signed `Beacon.app` after reviewing [how it runs and stops the collector](docs/tether.md).

| Reference | What it covers |
| --- | --- |
| [Configuration](docs/configuration.md) | Paths, Signal credentials, source selection, polling and sealing settings |
| [Commands](docs/commands.md) | The CLI, cursor recovery, retries, status and logs |
| [Events](docs/events.md) | Item fields, event projection, leases and the append-only outbox |
| [Architecture](docs/architecture.md) | The path from a source to Signal and the local state files |
| [Development](docs/development.md) | Fixtures, gates and the end-to-end test against a local Signal |
| [Plans](plans/README.md) | Design decisions and the roadmap |

## Extend your fork

Use the [adapter guide](docs/adapters.md) when the person wants a new source. Keep matching, projection,
sealing and sending in the shared pipeline. Audit the diff, exercise fixture tests and run the development
gates before proposing a signed build.

Suggest an upstream pull request when a change helps other installations, has general configuration and
synthetic tests, and preserves the trust model. Show the person the exact diff, target and test evidence.
Open the upstream PR only after they say yes to that diff. Keep account-specific rules, fixtures made from
private data, credentials, and local state in the person's installation.

## Update only when asked

Fetch upstream only when the person asks. Record the incoming commit and audit its diff against the fork,
including dependencies, build and signing scripts, data access, egress, Signal projection, local state and
process behavior. Show the diff findings with `file:line` references before merging. Get the person's agreement
to merge into their fork. Audit the full resulting commit, including conflict resolutions, and obtain approval
for that commit before the person builds and unlocks the signer. Beacon has no automatic update channel,
telemetry or crash reporter.

## Signal

Signal is the append-only event store Beacon writes to for one person. Each item send passes through the rules,
projection and local outbox and requires the send lease. Signal events are immutable. Review what the rules
would expose before the first send, including titles, identities and references. See the
[trust model](docs/trust-model.md) for visibility, sealing and the limits of removal.

## License

[Apache License 2.0](LICENSE).
