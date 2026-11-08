# Phase 1 — Trust core

Status: complete. What was built is described in [`docs/`](../docs/); see [Outcome](#outcome).

## Goal

Everything between a collected item and the network, with no real source yet: config, keychain, rules, projection,
the outbox, the Signal client, and the commands that let a person see what Beacon would do. When this phase ends, a
source is only a function that yields items.

## Scope

- **Module and layout.** `cmd/beacon` (CLI), `internal/config`, `internal/keychain`, `internal/rules`,
  `internal/project` (projection), `internal/outbox`, `internal/signal` (client), `internal/source` (the interface
  and a fixture source), `internal/egress`.
- **Item.** The common shape every source yields: `source`, `subject` (DEC-10), `key` (DEC-9), `type`,
  `occurred_at`, `actor` (identity of who caused it), `own` (the person caused it), `addressed` (the person was
  asked or mentioned), `fields` (map of the source's fixed fields), `references`.
- **Config.** One file: Signal URL, enabled sources, path to the rules file, send mode (`dry-run` until turned
  off). Secrets are never in it.
- **Keychain.** macOS Keychain through the `security` framework; an interface so tests use an in-memory store.
- **Rules.** YAML, first match wins, default `never`. A rule matches on source, type, any field (exact, glob, or
  regular expression), `own` and `addressed`. A rule sets `tier`, `fields` (what to send), and optionally `ask`
  (`me`), `priority` and `close_by_others`. Unknown keys are errors. `beacon rules check` validates the file and
  reports rules that can never match.
- **Projection.** Build the Signal request body from the item and the matching rule: only listed fields, with
  `subject`, `type`, `key`, `occurred_at`, `open` (DEC-17), `by` (DEC-11), `to` and `priority` (DEC-21), and
  `closes` for own acts (DEC-20).
- **Outbox.** Append-only file of JSON lines: key, body bytes (exactly as sent), state (`pending`, `sent`,
  `replayed`, `rejected`), Signal event id, times. A pending entry is retried with its stored bytes. A 409 is a
  stop for that key and is reported, never rewritten.
- **Signal client.** `POST /api/events` with the bearer key from the keychain; handle 201, 200 `replayed`, 409,
  429 (respect the rate limit), 5xx with backoff. Read `GET /api/log` only for the lease (DEC-25).
- **Lease.** Take and renew a claim; refuse to send without it.
- **Commands.** `init`, `run` (`--dry-run` default, `--once`), `explain <key>`, `outbox`, `egress`,
  `rules check`, `version` (with the commit and build flags).
- **Logging.** JSON lines with fixed event names. No item content, no bodies, no tokens.

## Out of scope

Real sources, the review queue UI, the classifier, releases and signing, the menu bar.

## Tests

- Rules: every match type; first match wins; default `never`; unknown keys rejected; unreachable rules reported.
- Projection: a field not listed never appears in the body; `by`, `to`, `closes` set per DEC-11, DEC-20, DEC-21.
- Outbox: kill the process between append and send, restart, and the same bytes are sent once; a replayed key is
  recorded as `replayed`; a 409 stops that key.
- Egress: a test transport fails the suite on any host not in the egress list.
- Lease: a second instance with the lease held sends nothing.
- End to end against a local Signal (its Docker image) with a fixture source: dry run sends nothing; send mode
  writes the expected events and threads.

## Exit gate

All gates green. With the fixture source: every item lands in its expected tier, held and dropped items never
reach the network, a crash between append and send produces exactly one event, and `beacon egress` lists every
host the test transport saw.

## Outcome

Built as scoped, with these differences from the text above:

- **DEC-27.** Signal accepts `by` only for identities an actor holds, reads `me` in `to` as the acting actor, and
  lets only the addressee or the asker close an ask. The projection therefore writes `to` as the person's handle,
  sets `by` only on own acts, records other people in `metadata.actor` when a rule lists `actor`, and closes as the
  asker for `close_by_others`. DEC-11 is narrowed accordingly, and external identities are a new dependency on
  Signal.
- **Lease.** One claim key per period, with the next period claimed ahead. The answer to a claim says who holds the
  period, so Beacon does not read `GET /api/log` at all.
- **Packages.** Beside the listed layout: `internal/runner` (one pass, the only path from an item to the client),
  `internal/queue` (hold queue and dropped record), `internal/logs` (fixed event names, typed attributes).
- **Hold.** Held items are kept in the local queue. Approving them is Phase 5.
- **First run.** `mode: dry-run` is the default and the person sets `mode: send`. The first-run window and summary
  acceptance of DEC-23 belong to the real sources.

Exit gate, and how each line was checked:

| Line | Check |
| --- | --- |
| All gates green | `scripts/gates.sh`: gofmt, go vet, staticcheck, `go test -race`, `scripts/repro.sh`. Run locally on macOS and in CI on macOS and Linux. |
| Every item lands in its expected tier | `TestExitGateTiersAndEgress`: seven fixture items, each with an expected tier and rule, checked in the dry-run output, the outbox, the hold queue and the fake Signal. `TestE2E` repeats it against Signal `main` in Docker. |
| Held and dropped items never reach the network | The same tests: no request body names a held or dropped key or carries their content markers, and against real Signal their subjects have no thread and their keys no event. |
| A crash between append and send produces exactly one event | `TestCrashBetweenAppendAndSend` kills a real process after the append is synced and before the send, restarts, and finds one request and one event with the stored bytes. `TestE2E` repeats it against real Signal. |
| `beacon egress` lists every host the test transport saw | `TestExitGateTiersAndEgress` builds the egress-recording transport from the output of `beacon egress`; it fails on any other host, and every host it saw is checked against the list. |
