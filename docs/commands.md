# Commands

Every command but `version` takes `--config PATH`. See [`configuration.md`](configuration.md) for where the config
is found otherwise.

## `beacon init`

```bash
beacon init --signal-url https://signal.example.com --handle @winton
```

Writes the config (mode `dry-run`, no sources), an empty rules file beside it and the state directory, and stores
the Signal key, read from standard input, in the keychain. It refuses to overwrite a config without `--force`.
`--keychain-account` names the keychain account.

## `beacon run`

Collects, applies rules and projection, and records what it decided.

- With `mode: dry-run`, or with `--dry-run`, it prints each item's tier, the rule that decided it and the body it
  would send, and writes and contacts nothing.
- With `mode: send`, it appends send items to the outbox, holds and drops the rest locally, takes the lease, and
  sends pending bodies while it holds it. It prints each answer and how many bodies are still pending.

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

## `beacon version`

Prints the version and commit set at link time (or the VCS revision Go recorded), the Go version and platform, and
the build flags Go recorded (`-trimpath`, `-ldflags`, `-tags`, `CGO_ENABLED`, `-buildmode`).

## The log

JSON lines on standard error. Each line has `time`, `level`, `msg` (the event name) and attributes that are counts,
source names, the mode, outcomes, HTTP statuses, Signal error codes and lease periods. A log line never holds item
content, a rule's match, an item key, a request body or a credential.

| Event | When |
| --- | --- |
| `run_started`, `run_finished` | Each pass. |
| `collected`, `collect_failed`, `item_invalid` | Per source. |
| `decided` | Counts per tier. |
| `project_failed` | An item a send rule matched could not be built. |
| `dry_run` | A dry run ended without sending. |
| `lease_held`, `lease_not_held`, `lease_failed` | The lease. |
| `send_result`, `send_stopped` | Each answer from Signal; a pass that stopped sending. |
