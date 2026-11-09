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
  it and the body it would send, and writes nothing. It may contact source APIs to collect. It does not contact
  Signal.
- `--first` is that dry run with a summary by source and type. A source with no cursor reads open state (DEC-23).
  Sending for that source stays off until `beacon accept-first`.
- With `mode: send`, it appends send items to the outbox, holds and drops the rest locally, takes the lease, and
  sends pending bodies while it holds it. A source whose first run has not been accepted is printed and not
  recorded, and its cursor is not moved. It prints each new decision (tier, key, rule) without its body, the
  lease, each answer from Signal, and how many event bodies are still pending. `--bodies` adds the body of each
  new decision. Decisions from earlier passes are not printed again.

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
prints one JSON object per entry, with the body.

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
```

`--from-gh` imports the token `gh auth token` prints. With `--host`, that is `gh auth token --hostname <host>`. It
asks for confirmation; `--yes` is that confirmation when input is not a terminal. Jira reads the account email, then
the API token with echo off. On a pipe both lines are read by one reader, so the token is not lost. The keychain
account is `source:<name>` under the service `beacon`. `--name` overrides the default, which is the kind.

## `beacon seed FILE`

Imports a seed file into the state directory (DEC-24). A line is `<subject> never|send`. A `#` at the start of a
line is a comment; a subject may itself contain `#`. Blank lines are ignored. A subject may appear once.

A `never` line drops that subject before the rules file, so an item already dismissed stays dismissed. A `send`
line is recorded and does not force a send.

## `beacon accept-first`

```bash
beacon accept-first [source...]
```

Records that the named sources' first runs were accepted. With no names, every GitHub and Jira source. Each named
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
| `run_failed` | `beacon run` stopped with an error. |

Reason codes: `config`, `source_failed`, `unauthorized`, `unavailable`, `lease_refused`, `lease_expired`,
`egress_denied`, `outbox_locked`, `state_files`, `keychain`, `canceled`, `internal`, `rate_limited`,
`source_unauthorized`, `scope_denied`, `identity_mismatch`, `item_invalid`, `project_failed`. A source that was not collected is also printed by
`run`, with the reason and not the error text.
