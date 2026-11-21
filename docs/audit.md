# Audit before install or update

Use this checklist for the exact commit the person would build. Read [AGENTS.md](../AGENTS.md) and the
[trust model](trust-model.md) first. The evidence below starts at the current collector. If a fork adds another
process, inspect its entry point and follow its file, credential and network access too. Keep the signer locked.
Use synthetic fixtures and in-memory test credentials. Agents never use the login keychain or run live collectors.

## 1. Establish what will run

- Record the fork remote, upstream remote and full commit ID. Review the tracked tree and the proposed config,
  including the Signal URL and actor, source accounts, collection scope, rules, state paths and whether
  `mode` is `dry-run` or `send`. Check config selection, path resolution and validation in
  [`cmd/beacon/app.go`](../cmd/beacon/app.go) and [`internal/config/config.go`](../internal/config/config.go).
- Follow the CLI entry point in [`cmd/beacon/main.go`](../cmd/beacon/main.go) and dispatch in
  [`cmd/beacon/app.go`](../cmd/beacon/app.go). Inventory every subprocess and listener, including setup commands.
  This tree's service runs the collector. For a future approval app also inspect the socket and peer identity,
  Touch ID binding, provider reads and writes, vault and journal, and supervision on Quit and crash. Check that
  the installed files and signer are outside the write access of the accounts they supervise.

## 2. Trace what Beacon reads and where it connects

Check the configured scope and the scopes the credential itself grants. Rules select outgoing fields. Collection
can read a broader set of metadata or text to identify a change, match it or resume from a cursor. Inspect initial
reads, later passes, pagination and the request scope checks. Record the actual host and port for each enabled source.

| Destination | What to trace |
| --- | --- |
| Configured Signal host and port | HTTPS, with HTTP accepted for loopback. The Signal bearer key authorizes event writes and send and coverage leases, `GET /api/me` on key replacement, restricted-prefix checks for personal subjects, and event/thread reads for retitles. Review [URL validation](../internal/config/config.go) and the [Signal client](../internal/signal/client.go). |
| `api.github.com:443` or the configured GitHub Enterprise host and port | Account and teams, configured repositories or owner patterns, pull requests, issue search and timelines, reviews, comments and checks. See [requests and pagination](../internal/github/github.go) and [scope](../internal/github/scope.go). |
| `<site>.atlassian.net:443` | Account lookup, JQL search, and issue fields, changelogs and comments for configured projects. The JQL search is a read using POST. See [collection](../internal/jira/jira.go) and [scope](../internal/jira/scope.go). |
| `gmail.googleapis.com:443` | The signed-in mailbox profile, labels, inbox and starred threads, and history including own mail. A body read needs a sealed rule for the source listing `body` and the broader Gmail grant. See [Gmail](../internal/google/gmail.go) and [Google request scope](../internal/google/api.go). |
| `www.googleapis.com:443` | Calendar events and attendee/state data, and Drive file metadata, comments, replies and meeting notes or transcripts. Audit exactly which fetches include descriptions or text. See [Calendar](../internal/google/calendar.go), [Drive](../internal/google/drive.go) and [Google request scope](../internal/google/api.go). |
| `oauth2.googleapis.com:443` | Google code exchange and access-token refresh. Verify the read scopes for the enabled kinds, PKCE and `state` on the one temporary `127.0.0.1` redirect listener. `accounts.google.com` is the consent destination opened by the person or system browser, outside the collector's HTTP transport. See [OAuth setup](../internal/google/authorize.go), [refresh](../internal/google/auth.go) and [token handling](../internal/keychain/oauth.go). |
| `slack.com:443` | `auth.test` at setup and during collection, search for DMs, mentions and own messages, replies and history for admitted conversations, membership and names when needed. Slack includes message text in those reads. Track when `channels: member` expands the channel set. See [collection](../internal/slack/slack.go), [methods and scope](../internal/slack/api.go) and [name lookups](../internal/slack/names.go). |
| Local fixture path | A JSON file has a declared field list and yields recorded items without contacting a source. The Signal destination remains listed. See [fixtures](../internal/source/fixture.go). |

- Compare `beacon egress` on a synthetic config with the hosts in [`internal/egress/egress.go`](../internal/egress/egress.go)
  and its use in [`cmd/beacon/app.go`](../cmd/beacon/app.go). Trace each HTTP client to the guarded transport
  and verify the request checks before it attaches credentials. Check origins (scheme, hostname and port),
  allowed paths and methods, server-provided pagination links, proxy behavior and redirect refusal.
  `beacon egress` lists the configured collector and Signal hosts. Review setup as well. For instance,
  `source add slack` can call Slack before Slack is enabled in the config, through the same host declaration.
  See [`cmd/beacon/sources.go`](../cmd/beacon/sources.go), [`internal/limited/limited.go`](../internal/limited/limited.go)
  and [`internal/signal/client.go`](../internal/signal/client.go).
- Trace helpers and outbound calls outside the pass, including `gh auth token`, the browser opener and macOS
  service/signature commands. Distinguish any build dependency downloads and operator Git commands from
  runtime egress. Check for a telemetry, crash reporting, update or classifier destination in the tree.
  See [`cmd/beacon/sources.go`](../cmd/beacon/sources.go), [`cmd/beacon/service.go`](../cmd/beacon/service.go)
  and [`internal/service/service.go`](../internal/service/service.go).

## 3. Trace what reaches Signal and what stays local

- Follow one item and one nonmatching item from [validation](../internal/source/source.go) through
  [first-match rules](../internal/rules/rules.go), [rules checks](../internal/rules/check.go),
  [the pass](../internal/runner/runner.go) and [projection](../internal/project/project.go). Check omitted and
  blank conditions, the default `never`, the first-run acceptance state and seed overrides. Inspect what the
  rule lists and what the envelope adds, including the item key and subject, event versus thread titles, actor
  attribution, asks, closes, references, state and opaque native IDs. A held item stays in the local queue.
- Review [sealed projection](../internal/project/sealed.go), [raises](../internal/runner/sealing.go) and
  [the subject floor](../internal/seal/floor.go). Check `seal.owner`, inherited meeting levels and the
  plain envelope. Signal receives the request in plaintext inside TLS and encrypts the sealed part at rest.
  Describe who can see the plain envelope, `owner` and `owner+agents`, using the intended Signal host's
  access policy. Verify the fixed personal stems and the restriction check for new, pending and rejected events,
  even after an adapter is removed. See [`internal/source/source.go`](../internal/source/source.go),
  [`internal/runner/runner.go`](../internal/runner/runner.go) and [`internal/runner/retry.go`](../internal/runner/retry.go).
- Follow the signed-in actor's writes, including the send lease, its fallback claim and cleanup, coverage
  snapshots and heartbeats, and thread retitles. Check the lease boundary, permitted title changes and minimum
  data for each control record. Compare [`internal/lease/lease.go`](../internal/lease/lease.go),
  [`internal/coverage/report.go`](../internal/coverage/report.go) and [`internal/runner/retitle.go`](../internal/runner/retitle.go).
- Check that an event body is synced to disk before send, that a pending replay uses the stored bytes and that
  accepted or conflicting entries stay settled. Rejected retry must run the current rules and be able only to
  narrow the stored decision. Review [`internal/outbox/outbox.go`](../internal/outbox/outbox.go) and
  [`internal/runner/retry.go`](../internal/runner/retry.go). Inspect local file modes and who can read held
  items, payloads, cursors, names and reports. The [state inventory](architecture.md#the-state-directory)
  points to those files. On an update, include pending payloads in the audit because a new rule does not
  rewrite bytes already in the outbox.

## 4. Trace credentials and output

- Follow each credential from input to storage and the request that uses it. Review echo suppression, redaction,
  bearer/basic headers, Google token handling and repair in [`internal/keychain/keychain.go`](../internal/keychain/keychain.go),
  [`internal/keychain/oauth.go`](../internal/keychain/oauth.go) and [`internal/keychain/darwin.go`](../internal/keychain/darwin.go).
  Check whether provisioning passes raw credential bytes outside that package. Keep service credentials away from
  Signal and other service origins. Check that repair and any platform or test overrides are unavailable to agents.
- Trace JSON log fields back to their inputs in [`internal/logs/logs.go`](../internal/logs/logs.go).
  Check provider-derived error codes, client error paths, terminal dry-run and `explain` output, and what ends
  up in the LaunchAgent's stdout and stderr files. Check both files for secrets, item content, request bodies or
  matches on content. See [`cmd/beacon/app.go`](../cmd/beacon/app.go) and
  [`internal/service/service.go`](../internal/service/service.go). Keep live output off the agent's transcript.

## 5. Verify build, launch and updates

- Review [`go.mod`](../go.mod), [`go.sum`](../go.sum), [`scripts/repro.sh`](../scripts/repro.sh),
  [`scripts/build-local.sh`](../scripts/build-local.sh), [`scripts/dev-signing-setup.sh`](../scripts/dev-signing-setup.sh)
  and the [CI workflow](../.github/workflows/ci.yml). Identify the toolchain, dependency pins, scripts and
  artifact paths. Run the reproducibility check for the audited commit. It compares two builds in different
  directories with the same toolchain. Report that scope and the outcome of the fixture tests or CI gates.
  Follow [signing](signing.md) for the person's clean-checkout and staged-artifact verification and disposable rebuild test.
- Review the plist, executable and certificate checks, privileges, config path, log destinations and
  restart/stop semantics in [`cmd/beacon/service.go`](../cmd/beacon/service.go) and
  [`internal/service/service.go`](../internal/service/service.go). Trace SIGTERM and SIGINT in
  [`cmd/beacon/main.go`](../cmd/beacon/main.go) and source loops in [`internal/schedule/schedule.go`](../internal/schedule/schedule.go).
  The current optional job is per-user and has no credential or environment variables in the plist. Check both
  install and replacement paths, including what happens to pending sends during shutdown.
- Check for anything that fetches or executes new software after install, including service restarts, scripts or
  CI artifacts offered to users. Review [CLI dispatch](../cmd/beacon/app.go), [the LaunchAgent](../internal/service/service.go)
  and [the workflow](../.github/workflows/ci.yml). The expected update channel is a person-requested Git fetch
  and a reviewed merge into their fork. For updates, audit the full diff before merging and the final commit
  before signing. See [the update procedure](../README.md#update-only-when-asked).

## Report and decide

Lead with the fork URL, full audited commit ID, intended configuration and a plain-language decision on whether
installation can proceed. For each finding include `path:line`, what the person would observe or expose, affected
configurations and what would clear any blocker. Include the host and data inventory, who can read what reaches
Signal, which processes run and how they stop, the checks you ran and their results, and any checks awaiting the
person. Use synthetic examples. Leave credentials, private config, item keys and actual payloads out of the report.

Block the affected installation or feature when a trust property fails or you cannot verify it. That includes
undeclared connections, credentials outside their allowed path, sensitive logs, an implicit send or excess
projection, unverified personal visibility, a dirty or mismatched build, unverified signer or artifact, or
unapproved privileges or update behavior. Report narrower findings with the affected path and conditions.
Keep real credentials out until the signing and Keychain checks pass on the person's Mac. Ask the person for
explicit agreement to the exact audited commit and setup after blocking findings are resolved.
