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

## `beacon run`

Collects, applies rules and projection, and records what it decided.

- With `mode: dry-run`, or with `--dry-run`, or with `--first`, it prints each item's tier, the rule that decided
  it and the body it would send, and writes nothing except `--first`, which records in `first.json` which sources
  completed that summary. It may contact source APIs to collect. It does not contact Signal.
- `--first` is that dry run with a summary by source and type. A source with no cursor reads open state (DEC-23).
  Sending for that source stays off until `beacon accept-first`. A collect that failed is not written to `first.json`.
- With `mode: send`, it appends send items to the outbox, holds and drops the rest locally, takes the lease, and
  sends pending bodies while it holds it. A source whose first run has not been accepted is printed and not
  recorded, and its cursor is not moved. It prints each new decision (tier, key, rule) without its body, the
  lease, each answer from Signal, and how many event bodies are still pending. `--bodies` adds the body of each
  new decision. Decisions from earlier passes are not printed again.

With a Google source, a send pass first reads Signal's restricted prefixes. A source whose prefix is not restricted
has its send items held, and `run` prints `source <name>: N items held, not sent: <prefix> is not restricted in
Signal …` and which prefix to ask Signal's admin to restrict. If the restrictions cannot be read, those items are
printed as not sent and not recorded, the source's cursor stays, and `run` says why (`no_read_scope` when the key
lacks `read`). Pending bodies on such subjects stay pending and are counted. When Gmail's change history has expired,
`run` says so and what was re-read. See [`sources.md`](sources.md#google).

It exits non-zero when the pass cannot go on: Signal refused the key (401) or the lease claim, another process holds
the outbox, the key is not in the keychain, or the config, rules or a source cannot be loaded. The message names the
reason and the command that shows details (`beacon rules check`, `beacon outbox`); it never repeats an error's text,
which can name item keys or rule patterns. An unreachable Signal is not an error: bodies stay pending for the next
pass.

`--once` makes one pass and exits; without it, `run` makes a pass every `poll_interval` until interrupted. The log
goes to standard error.

## `beacon explain KEY`

Shows, for one item key: its source, subject and type; the rule that matched and its line, or the default; the
tier; the classifier's answer (`not configured` until Phase 6); whether it is held; and the body. For a key in the
outbox the body is the exact stored bytes, with the state and Signal's event id or refusal. Otherwise it is the body
the item would be sent as now.

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

Validates the rules file and reports rules that can never match. See [`rules.md`](rules.md#beacon-rules-check).

## `beacon source add`

Stores a source credential in the keychain and enables the source in the config. The token is not printed and is
not written to the config.

```bash
beacon source add github --repo owner/name [--repo owner/*] [--project KEY] [--host HOST] [--from-gh] [--yes]
beacon source add jira --site SITE --project KEY [--project KEY]
beacon source add google --account EMAIL [--client-file FILE] [--gmail] [--calendar] [--drive] [--bodies] [--open]
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
in the config; `--bodies` asks for
`gmail.readonly` instead of `gmail.metadata`. It prints the scopes granted, never a token. The token exchange goes
through the egress guard to `oauth2.googleapis.com`; the browser, not Beacon, opens the consent page. See
[`sources.md`](sources.md#google).

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
a cursor. It does not send by itself.

## `beacon version`

Prints the version and commit set at link time (or the VCS revision Go recorded), the Go version and platform, and
the build flags Go recorded (`-trimpath`, `-ldflags`, `-tags`, `CGO_ENABLED`, `-buildmode`).

## The log

JSON lines on standard error. Each line has `time`, `level`, `msg` (the event name) and attributes that are counts,
source names, the mode, outcomes, HTTP statuses, Signal error codes, lease periods and a fixed `reason` code. A log
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
| `history_gap` | A source's change history expired; it re-read what it could. The source only. |
| `run_failed` | `beacon run` or `beacon outbox retry` stopped with an error. |

Reason codes: `config`, `source_failed`, `unauthorized`, `unavailable`, `lease_refused`, `lease_expired`,
`egress_denied`, `outbox_locked`, `state_files`, `keychain`, `canceled`, `internal`, `rate_limited`,
`source_unauthorized`, `scope_denied`, `identity_mismatch`, `item_invalid`, `project_failed`, `prefix_unrestricted`,
`restrictions_unknown`, `no_read_scope`. A source that was not collected is also printed by
`run`, with the reason and not the error text.
