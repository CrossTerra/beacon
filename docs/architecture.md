# Architecture

Beacon is one Go binary. This file says how an item travels from a source to Signal, and which package does what.
The properties this design protects are in [`trust-model.md`](trust-model.md).

## The path of an item

```
source.Collect ──► rules.Decide ──► never: dropped.jsonl (key and rule only)
                        │
                        ├──► hold: held.jsonl (local review queue; approval is Phase 5)
                        │
                        └──► send: project.Project ──► outbox.Append (synced) ──► lease held? ──► signal.Write
                                                                                       │
                                                                          no: stays pending, sent later
```

One pass of `beacon run`:

1. **Collect.** Each enabled source yields items. Items that do not validate (wrong source, a field outside the
   source's field list, a subject or key Signal would refuse) are counted and skipped. Items are sorted by
   `occurred_at`, then key.
2. **Skip what is decided.** A key already in the outbox or the hold queue is not decided again: one event per
   change of state (DEC-9).
3. **Decide.** The first rule that matches sets the tier. No match is `never` (DEC-3).
4. **Record.** `never` adds the key and rule name to `dropped.jsonl`. `hold` adds the item to `held.jsonl`. `send`
   builds the request body with the projection and appends it to the outbox, synced to disk, before anything is
   sent (DEC-8).
5. **Send.** Only while this instance holds the lease (DEC-25). Pending bodies are sent oldest first, as stored. If
   Signal is unavailable the pass stops and the rest stay pending, because a later body may close an ask an earlier
   one opens.

A dry run does steps 1 to 3, prints each item's tier, rule and body, and writes and contacts nothing. It reads the
outbox and the hold queue, so it shows what a send pass would do next.

## Packages

| Package | Role |
| --- | --- |
| `cmd/beacon` | The CLI. Builds everything from the config; the only place that wires the keychain and the transport. |
| `internal/source` | The `Item` shape, the `Source` interface, and the fixture source. |
| `internal/config` | Reads and validates the config file. Holds no secrets. |
| `internal/keychain` | The only code that reads or writes a credential. macOS Keychain through the Security framework, and an in-memory store for tests. |
| `internal/rules` | Parses the rules file, decides tiers, and checks for rules that can never match. |
| `internal/project` | Builds the request body from an item and its rule. |
| `internal/outbox` | The append-only record of every body, and each key's state. |
| `internal/queue` | The hold queue and the record of dropped keys. |
| `internal/signal` | The Signal client: `POST /api/events`, nothing else. |
| `internal/lease` | The send lease. |
| `internal/runner` | One pass: collect, decide, record, send. The only code that hands item content to the client. |
| `internal/egress` | The list of hosts Beacon may contact, and the transport guard that enforces it. |
| `internal/logs` | The log: JSON lines with fixed event names. |

Test helpers: `internal/signal/signaltest` (a fake Signal with Signal's key rules) and
`internal/egress/egresstest` (a transport that fails the test on any host not on the egress list).

## The state directory

| File | Holds | Leaves the machine |
| --- | --- | --- |
| `outbox.jsonl` | Every request body, exactly as sent, and Signal's answers | The bodies, once each |
| `held.jsonl` | Held items, in full | No |
| `dropped.jsonl` | Keys of dropped items and the rule that dropped them | No |
| `instance` | This instance's random id, used in lease claims | Inside lease claims |

The directory is created `0700` and the files `0600`. A writer holds an exclusive `flock` on the outbox, so two
`beacon run` processes cannot share a state directory.

## Adding a source

A source implements `source.Source`: a name, a fixed field list, the hosts it contacts, and `Collect`. Its hosts
appear in `beacon egress` and in the transport guard without other changes. A source keeps its own credentials in
the keychain through `internal/keychain`, and its own cursor.
