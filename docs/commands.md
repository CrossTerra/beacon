# Commands

Every command but `version` takes `--config PATH`. See [`configuration.md`](configuration.md) for where the config
is found otherwise.

## `beacon init`

```bash
beacon init --signal-url https://signal.example.com --handle @winton
```

Writes the config (mode `dry-run`, no sources), an empty rules file beside it and the state directory, and stores
the Signal key, read from standard input, in the keychain. Echo is off while it reads from a terminal, and Ctrl+C
restores it and ends `init` without writing anything. It refuses to overwrite a config without `--force`.
`--keychain-account` names the keychain account.

## `beacon key set`

```bash
beacon key set [--force]
```

Replaces the Signal key in the keychain, in the same account `init` used. It reads the new key from standard input,
with echo off when that is a terminal, and does not print it. Before storing, it checks the key with `GET /api/me`.
Signal must accept it, and the key's actor must be the configured person or their agent: the handle in
`person.handle`, the actor the stored key already is, or an agent of that person. If the stored key is refused,
it cannot name its actor, so an agent key is accepted and a different person is not. `--force` stores it anyway. A refusal, a mismatch, or no
answer stores nothing and leaves the previous key. It prints the actor and the scopes, so a rotation or a wider key
can be confirmed, and never the key.

## `beacon run`

Collects, applies rules and projection, and records what it decided.

- With `mode: dry-run`, or with `--dry-run`, or with `--first`, it prints each item's tier, the rule that decided
  it and the body it would send, and writes nothing except `--first`, which records in `first.json` which sources
  completed that summary. The launchd agent does not print bodies, unless `--bodies` is set: those lines are the
  service log. It may contact source APIs to collect. It does not contact Signal.
- `--first` is that dry run with a summary by source and type. A source with no cursor reads open state (DEC-23).
  Sending for that source stays off until `beacon accept-first`. A collect that failed is not written to `first.json`.
- With `mode: send`, it appends send items to the outbox, holds and drops the rest locally, takes the lease, and
  sends pending bodies while it holds it. A source whose first run has not been accepted is printed and not
  recorded, and its cursor is not moved. It prints each new decision (tier, key, rule) without its body, the
  lease, each answer from Signal, and how many event bodies are still pending. `--bodies` adds the body of each
  new decision. Decisions from earlier passes are not printed again.

With a Google or Slack source, a send pass first reads Signal's restricted prefixes. A source whose prefix is not restricted
has its send items held, and `run` prints `source <name>: N items held, not sent: <prefix> is not restricted in
Signal …` and which prefix to ask Signal's admin to restrict. If the restrictions cannot be read, those items are
printed as not sent and not recorded, the source's cursor stays, and `run` says why (`no_read_scope` when the key
lacks `read`). Pending bodies on such subjects stay pending and are counted. A Slack source whose token lacks
`users:read` or `im:read` says so once a pass: its threads keep generic titles until the app has them. When a pass
renamed threads Beacon opened earlier, it prints how many. When Gmail's change history has expired,
`run` says so and what was re-read, and when a read stopped at its limit, it says that too. When a Gmail open-state
scan stops at a rate limit, `run` says the next pass continues from where it stopped. Only a send pass stores that
place. A dry run does not, so the next send reads the window again. See [`sources.md`](sources.md#google).

A sealed item is printed with `sealed at owner` or `sealed at owner+agents`, and `(raised; beacon explain shows why)`
when something raised it; never what. An item held for sealing says so: `body_needs_seal` (its rule would send a
personal body plain) or `sensitive_shared` (it would be sealed on a shared subject, which Signal cannot seal). A rules
error, a DEC-32 personal body without `seal` included, stops `run`.

It exits non-zero when the pass cannot go on: Signal refused the key (401; store another with `beacon key set`) or the lease claim, another process holds
the outbox, the key is not in the keychain, the binary is not the signing identity `beacon service install` recorded,
or the config, rules or a source cannot be loaded. The message names the
reason and the command that shows details (`beacon rules check`, `beacon outbox`); it never repeats an error's text,
which can name item keys or rule patterns. An unreachable Signal is not an error: bodies stay pending for the next
pass.

Without `--once` or `--first`, that non-zero exit waits first. The wait is one `poll_interval`, then double that, up
to 30 minutes, and the reason is in `pass.json` before the wait, so `beacon service status` shows it. A rules or
source error still has a config, so that record uses the state directory and the configured interval. launchd's
10-second throttle is on top of that wait, not instead of it. SIGTERM during the wait exits 0. SIGINT does not: the
failure is still reported. `--once` and `--first` do not wait.

`--once` makes one pass and exits. `--first` does too. Without either, `run` makes a pass every `poll_interval`
until interrupted, in dry-run and in send mode. A dry-run loop still writes nothing. The log goes to standard error.

On SIGTERM the poll loop finishes the send already in progress, records it, and exits 0 without starting another
send or another pass. That is the stop launchd uses. A lease renewal already underway finishes; no further event
send starts after it. SIGINT still cancels the in-flight request. `beacon run` refuses to start as root.

## `beacon explain KEY`

Shows, for one item key: its source, subject and type; the rule that matched and its line, or the default; the
tier; for a send or hold item its level (`plain`, `owner+agents` or `owner`), the rule's own level, and what raised it
(the `seal.owner` word, sender or label and where it matched, its meeting's level, or the subject's floor) and whether
it is held as `body_needs_seal` or `sensitive_shared`; the classifier's answer (`not configured` until Phase 6);
whether it is held; and the body. For a key in the outbox the body is the exact stored bytes, with the state and
Signal's event id or refusal. Otherwise it is the body the item would be sent as now. A sealed body is shown with each
sealed part replaced by its size unless `--bodies` is given. What raised an item is read on this machine and shown
only here: `run` prints "raised" and the log a count.

## `beacon outbox`

Lists every outbox entry in order: when it was appended and last updated, its state, kind (`event` or `lease`),
key, Signal event id (or the status and code that refused it) and rule. `--bodies` adds each body; `--json`
prints one JSON object per entry, with the body. Neither prints the decided item stored with an append.

## `beacon outbox retry --rejected`

Runs the current rules on each stored item and sends only what they still send. A field leaves only when both the
stored decision and the current rule list it. If the rules now hold or drop the item, the entry is recorded
`superseded` and not sent. It requires `mode: send` and the lease. It does not send an entry Signal accepted, and
it does not touch a `409`. An entry with no stored item is not sent by this command. A `400` of that kind is
decided again when a later collect yields the key. An entry on a personal subject whose prefix Signal does not
restrict is not sent and is counted as withheld. The log and the summary are counts only: sent, replayed, refused,
conflict, missing, failed, hold, never, withheld, pending. See [`events.md`](events.md#retrying-a-refusal).

```bash
beacon outbox retry --rejected
```

## `beacon cursor reset`

```bash
beacon cursor reset github
```

Removes the committed cursor for each named source, so the next collect reads open state, not history (DEC-23).
It does not send, and it does not clear acceptance. A source with no cursor is already reading open state.

This is the recovery for a `400` recorded before the decided item was stored. Signal never stored those events, so
they do not block a later decision. Then:

```bash
beacon run --once
```

That pass reads open state, not history (DEC-23). For GitHub that is open pull requests the person wrote or was
asked to review, the current review requests, and checks on their own open pull requests. For Gmail it is inbox
threads and starred mail from the last 14 days, for Calendar the next 7 days, for Drive the last 7 days. A `400` with no stored
item is sent once if that collect yields the key, with the current projection. The refused lines stay in the
outbox. Timeline history (a review already submitted, a comment, a push) is not read again. A `409`, and anything
Signal accepted, is not sent again.

## `beacon egress`

Prints every host Beacon may contact under the current config, as `host:port` and why: the Signal host, and each
enabled source's hosts. A hosted classifier would be listed here (DEC-7); Phase 1 has none. Beacon's transport
refuses every other host.

## `beacon rules check`

Validates the rules file and reports rules that can never match, personal bodies without `seal` (DEC-32), seals no
personal subject can take, and, as warnings, plaintext personal content in `send` rules (DEC-36). See
[`rules.md`](rules.md#beacon-rules-check).

## `beacon source add`

Stores a source credential in the keychain and enables the source in the config. The token is not printed and is
not written to the config.

```bash
beacon source add github --repo owner/name [--repo owner/*] [--project KEY] [--host HOST] [--from-gh] [--yes]
beacon source add jira --site SITE --project KEY [--project KEY]
beacon source add google --account EMAIL [--client-file FILE] [--gmail] [--calendar] [--drive] [--bodies] [--open]
beacon source add slack [--channel ID]... [--name NAME]
```

`--from-gh` imports the token `gh auth token` prints. With `--host`, that is `gh auth token --hostname <host>`. It
asks for confirmation; `--yes` is that confirmation when input is not a terminal. Jira reads the account email, then
the API token with echo off. On a pipe both lines are read by one reader, so the token is not lost. The keychain
account is `source:<name>` under the service `beacon`. `--name` overrides the default, which is the kind.

`google` enables Gmail, Calendar and Drive, or only those named with `--gmail`, `--calendar` and `--drive`, and
authorises read-only access to the person's own OAuth client, for the Google account `--account` names (required;
written to each source as `account`). `--client-file` is the JSON of a *Desktop app* client from the person's
Google Cloud project; its id and secret are stored as `google:<account>:client`. Without it, that account's stored
client is used. The command prints the consent URL (`--open` also opens it in the default browser; `login_hint` is
the account), waits up to five minutes on `127.0.0.1` for the redirect, exchanges the code with PKCE, and stores the
refresh token as `google:<account>:refresh`. The grant asks for the scopes of every Google source of that account
in the config; `--bodies` asks for `gmail.readonly` instead of `gmail.metadata`. It prints the scopes granted, never a token. The token exchange goes
through the egress guard to `oauth2.googleapis.com`; the browser, not Beacon, opens the consent page. See
[`sources.md`](sources.md#google).

`slack` reads the person's user token (`xoxp-…`) with echo off, and refuses any other kind of token. It calls
`auth.test` through the egress guard to `slack.com`, and nothing else. If the token lacks one of the read scopes
(`search:read`, `channels:history`, `groups:history`, `im:history`, `mpim:history`, `channels:read`, `groups:read`,
`mpim:read`, `usergroups:read`, `users:read`, `im:read`), it names them and stores nothing. A token already stored
without `users:read` and `im:read` still collects; see [`sources.md`](sources.md#slack). Otherwise it stores the token as `slack:<workspace id>:<user id>`, writes the source with that `workspace`
and `user` and each `--channel`, names any other scopes the token holds (Beacon never uses them), and, when
`person.identities` has no entry for the source, prints the one to add. See [`sources.md`](sources.md#slack).

## `beacon seed FILE`

Imports a seed file into the state directory (DEC-24). A line is `<subject> never|send`. A `#` at the start of a
line is a comment; a subject may itself contain `#`. Blank lines are ignored. A subject may appear once.

A `never` line drops that subject before the rules file, so an item already dismissed stays dismissed. A `send`
line is recorded and does not force a send.

## `beacon accept-first`

```bash
beacon accept-first [source...]
```

Records that the named sources' first runs were accepted. With no names, every source but a fixture. Each named
source must have a completed `beacon run --first` for that source; accepting without that summary is an error. A
collect that failed is not a completed summary. The next send pass may send their open-state items and then commit
a cursor. It does not send by itself. A running agent (`beacon service`) reads the acceptance before each pass, so
it does not need a restart.

## `beacon service`

A per-user launchd agent on macOS. It is not a LaunchDaemon, it sets no user name, and it refuses to install or to
run as root. The job is `dev.beacon.agent`, in the GUI domain of the user who installed it
(`~/Library/LaunchAgents/dev.beacon.agent.plist`).

```bash
beacon service install [--binary PATH] [--force]
beacon service uninstall
beacon service status
beacon service logs [--stdout]
```

`install` writes the plist and bootstraps it. The job runs the signed binary with `run` and `--config` set to the
absolute config path: the poll loop, not `--once`. It starts at load. The process reads the config when it starts
and does not reload it: there is no SIGHUP handler and no watch on the file. Restart the agent after a config
change. The cursors and the first-run acceptance are state, not config: they are read again before every pass. An invalid config is not applied over a running one; the process exits, and launchd restarts it only when it exits
non-zero, and not more than once every 10 seconds. A clean exit after SIGTERM stays stopped. launchd waits 180
seconds after SIGTERM before killing the process, which covers one send. The plist sets no environment variables
and no credential; the agent reads keys from the keychain when a pass sends, the same as a terminal `beacon run`.
Its umask is 077.

Standard output and the JSON log go to `service.stdout.log` and `service.stderr.log` in the state directory, mode
`0600`. The service does not print item bodies there unless `run` was given `--bodies`. It still prints keys and
rule names, as a terminal send pass does. The JSON log has neither. A file of a mebibyte or more is copied aside
(one generation, `*.1`) and truncated, as soon as the config is loaded and again before each pass, so a restart
that fails its checks cannot grow the log without a cap. launchd holds the descriptor open, so the file is
truncated in place rather than renamed.

The binary must be code-signed with a stable identity. An ad-hoc signature, which is what `go build` produces, is
refused and nothing is written: each rebuild would be a different program to the keychain.
[`scripts/build-local.sh`](../scripts/build-local.sh) signs with your Apple Development identity, or the self-signed
one from [`scripts/dev-signing-setup.sh`](../scripts/dev-signing-setup.sh), and the identifier `dev.beacon.cli`. A
Developer ID signature is also a stable identity. Only an Apple-issued certificate keeps keychain access across
rebuilds; see [Local builds and the keychain](development.md#local-builds-and-the-keychain). `--binary` names a binary other than the one running the command. The path
in the plist is absolute and symlink-resolved, and it is the path that was checked. Install records the SHA-256 of
that certificate's leaf in the state directory. A later `beacon run` refuses a binary whose leaf does not match, and
does not contact Signal. `status` prints `identity: mismatch` when that is so, and does not print the hash.

`install` refuses to replace an existing plist without `--force`. `--force` stops the running job and waits until
launchd reports it unloaded, up to the 180 seconds the agent is given to finish a send, then writes the new plist
and bootstraps it. If that wait does not finish, the old plist is left in place. If bootstrap fails, the new plist is removed so the next login does not start a half-installed
agent. `uninstall` stops the job and removes the plist. It does not delete the state directory.

`status` prints whether the job is loaded and running, and the last pass from `pass.json` in the state directory:
the time, the outcome (`ok`, `stopped`, or a reason code), and the lease. A failed start records its reason there
even when the pass did not send, so a persistent 401 or a refused lease is visible while the agent is backing off.
When this instance held the lease, the holder is the instance id; the claim's `metadata.run` is that id plus
`:lease`. Another holder is reported as `another instance`. A personal source the last pass did not send for is a
`withheld: <source> <prefix> <reason>` line: `prefix_unrestricted` (ask Signal's admin to restrict the prefix) or
`restrictions_unknown` (the restrictions could not be read; Beacon's key needs `read`). Status does not contact Signal, and it does not print a
pass record that is not in that fixed set. `logs` prints the JSON log. `--stdout` prints the other file. Neither
follows.

On a system that is not macOS, install, uninstall and status report that the agent is a launchd job and do nothing
else.

## `beacon keychain repair`

Re-creates every keychain item under the service `beacon`, so each one has the access a new item gets from the
running build and none of what earlier builds added to it: only code with this build's identifier and signing
certificate reads it without asking, and the item's partition list holds only this build's partition. Run it once
after switching the signing certificate, with the build you will keep (see
[Local builds and the keychain](development.md#local-builds-and-the-keychain)).

It lists the items by attributes, which does not prompt, then for each one reads the value once (macOS asks once per
item; choose Always Allow), stores a copy as `repair:<account>`, deletes the item, writes it back, reads it back to
check the bytes, and deletes the copy. If it stops part way, the copy keeps the value and the next run restores it.
It prints the account names it repaired and never a value. It takes no `--config`: it repairs every Beacon item. A
build that is not signed with a certificate refuses it, as it refuses every keychain read.

## `beacon version`

Prints the version and commit set at link time (or the VCS revision Go recorded), the Go version and platform, and
the build flags Go recorded (`-trimpath`, `-ldflags`, `-tags`, `CGO_ENABLED`, `-buildmode`).

## The log

JSON lines on standard error. Each line has `time`, `level`, `msg` (the event name) and attributes that are counts,
source names, the mode, outcomes, HTTP statuses, Signal error codes, lease periods, a fixed `reason` code, and on
`rate_limited` the source API's own rate-limit reason code from a fixed list and the `Retry-After` it named. A log
line never holds item content, a rule's match, an item key, a request body, a credential or an error's text.

| Event | When |
| --- | --- |
| `run_started`, `run_finished` | Each pass. |
| `collected`, `collect_failed`, `item_invalid` | Per source. `item_invalid` is a count of dropped items and the reason `item_invalid`. |
| `decided` | Counts per tier. |
| `project_failed` | A count of items a send or hold rule matched that could not be built, with the reason `project_failed`. The cursor still moves. |
| `dry_run` | A dry run ended without sending. |
| `lease_held`, `lease_not_held`, `lease_failed`, `lease_expired` | The lease; `lease_expired` is a pass stopped because the lease ran out. |
| `send_result`, `send_stopped` | Each answer from Signal; a pass that stopped sending. |
| `retry` | `beacon outbox retry --rejected` finished. Counts only: sent, replayed, refused, conflict, missing, failed, hold, never, withheld, pending. |
| `withheld` | A personal source's send items were held (`prefix_unrestricted`) or not recorded (`restrictions_unknown`). A count and the source. |
| `restrictions_unread` | A send pass could not read Signal's restricted prefixes, with the reason. |
| `names_unavailable` | A source collected without display names: `names_scope` (the token lacks the scopes) or `names_failed` (a lookup failed this pass). Its items are complete; they carry no names or titles. The source and the reason. |
| `retitled` | A send pass checked threads Beacon opened for a new title. Counts only: checked, titled, left. |
| `sealed` | Items a pass decided to seal, by level: `owner` and `owner_agents` counts. |
| `raised` | Items the local check, a meeting's level or the subject floor raised: a count. Never why. |
| `seal_held` | Items held instead of sent, with the reason `body_needs_seal` or `sensitive_shared`, and a count. |
| `history_gap` | A source's change history expired; it re-read what it could. The source, and `cut` 1 when the re-read stopped at its limit. |
| `read_cut` | A source's read stopped at its limit (Gmail's first run, a Slack search); older items in its window were not read. The source only. |
| `read_paused` | A Gmail open-state scan stopped at a rate limit and will continue next pass. Not a failure. The source, and `reason` `rate_limited`. |
| `rate_limited` | A source's API answered with its rate limit and that stopped the source, before its `read_paused` or `collect_failed`. The source, the HTTP `status`, the API's `limit_reason` (a reason code from a fixed list, `other`, or empty), `retry_after` in seconds (`-1` when none was named), and `units`, the quota units the source spent in the last minute (Gmail; 0 for the others). See [sources](sources.md#rate-limits). |
| `run_failed` | `beacon run` or `beacon outbox retry` stopped with an error. |
| `run_stopped` | The poll loop exited after SIGTERM. Not a failure. |

Reason codes: `config`, `source_failed`, `unauthorized`, `unavailable`, `lease_refused`, `lease_expired`,
`egress_denied`, `outbox_locked`, `state_files`, `keychain`, `canceled`, `internal`, `rate_limited`,
`source_unauthorized`, `scope_denied`, `identity_mismatch`, `signing_mismatch`, `item_invalid`, `project_failed`,
`prefix_unrestricted`, `restrictions_unknown`, `no_read_scope`, `names_scope`, `names_failed`, `body_needs_seal`,
`sensitive_shared`. A source that was not collected is also printed by
`run`, with the reason and not the error text.
