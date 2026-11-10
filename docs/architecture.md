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

1. **Collect.** Each enabled source yields items since its committed cursor, and an opaque next cursor. A source
   with no cursor reads open state, not history (DEC-23). Items that do not validate (wrong source, a field outside
   the source's field list, a subject or key Signal would refuse) are counted and skipped. Items are sorted by
   `occurred_at`, then key.
2. **Skip what is decided.** A key already in the outbox, or in the hold queue, is not decided again: one event per
   change of state (DEC-9). A `400` with no stored item is not a decision: Signal never stored it, so the same key
   is decided again and appended as a new attempt. A `409`, and anything Signal accepted, still blocks. A subject a
   seed file marks `never` is dropped before the rules file (DEC-24). `beacon outbox retry --rejected` re-sends a
   refusal that still has its decided item, and does not touch a `409`.
3. **Decide.** The first rule that matches sets the tier. No match is `never` (DEC-3).
4. **Record.** `never` adds the key and rule name to `dropped.jsonl`. `hold` adds the item to `held.jsonl`. `send`
   builds the request body with the projection and appends it to the outbox, with the listed fields, the event title,
   and the ids and times needed to decide it again, synced to disk, before anything is sent (DEC-8). An unlisted field is not
   stored. The stored decision does not leave the machine. A body or request over Signal's limit is projected again without the body and marked truncated; if
   that still does not fit, the item is held, not dropped. A source whose first run has not been accepted is
   reported and not recorded.
5. **Commit.** The source's cursor is stored only after every item of the pass has been recorded, and only on a
   send pass. A crash before that collects the same items again; keys make that harmless.
6. **Send.** Only while this instance holds the lease (DEC-25). Pending bodies are sent oldest first, as stored,
   each under a deadline at the end of the lease. If Signal is unavailable, or the lease runs out, the pass stops
   and the rest stay pending, because a later body may close an ask an earlier one opens.

A dry run does steps 1 to 3, prints each item's tier, rule and body, and writes nothing except `beacon run --first`,
which records in `first.json` which sources completed that summary. It does not write cursors, the outbox, or the
queue. It may contact source APIs to collect; it does not contact Signal. It reads the outbox, the hold queue and
the cursor, so it shows what a send pass would do next.

## Packages

| Package | Role |
| --- | --- |
| `cmd/beacon` | The CLI. Builds everything from the config; the only place that wires the keychain and the transport. |
| `internal/source` | The `Item` shape, the `Source` interface, and the fixture source. |
| `internal/github`, `internal/jira` | The GitHub and Jira sources. See [`sources.md`](sources.md). |
| `internal/cursor` | Committed cursors, which sources have completed `beacon run --first`, and which of those have been accepted. |
| `internal/seed` | The seed file (DEC-24). |
| `internal/limited` | Rate-limit backoff for source HTTP. |
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
| `outbox.jsonl` | Every request body, exactly as sent, Signal's answers, and the decided item for each event append | The bodies, once each. The decided item stays |
| `held.jsonl` | Held items, in full | No |
| `dropped.jsonl` | Keys of dropped items and the rule that dropped them; one line per key, again only if another rule drops it | No |
| `instance` | This instance's random id, used in lease claims | Inside lease claims |
| `cursors.json` | Each source's committed cursor, opaque | No |
| `first.json` | Sources whose `beacon run --first` completed. `accept-first` requires it | No |
| `accepted.json` | Sources whose first run was accepted (DEC-23) | No |
| `seed` | Imported seed lines (DEC-24) | No |

The directory is created `0700` and the files `0600`, and it is synced when a file in it is created. A writer holds
an exclusive `flock` on the outbox, so two `beacon run` processes cannot share a state directory, and the queue
files are opened for writing only under that lock. A line torn by a crash is cut off when a send pass opens the
file under the lock. A dry run and `beacon explain` take no lock and change nothing: they read the outbox and the
queue files, skip a torn last line (it may be a record a sending process is appending), and create no file.

## Adding a source

A source implements `source.Source`: a name, a fixed field list, a fixed state field list (DEC-28), the hosts it
contacts, and `Collect`. `Collect` takes the committed cursor and returns the next one; the runner stores it after
deciding. Its hosts appear in `beacon egress` and in the transport guard without other changes. A source keeps its
credentials in the keychain through `internal/keychain`, and reads them per pass. GitHub and Jira are
[`sources.md`](sources.md).
