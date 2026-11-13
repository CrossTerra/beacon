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
6. **Send.** Only while this instance holds the lease (DEC-25). A body on a personal subject (`gmail:`, `calendar:`,
   `drive:`, `slack:dm:`) is sent only when Signal restricts its prefix; the restrictions are read at the start of the pass, and
   a send item of a source whose prefix is not restricted was held at step 4 instead. Pending bodies are sent oldest first, as stored,
   each under a deadline at the end of the lease. If Signal is unavailable, or the lease runs out, the pass stops
   and the rest stay pending, because a later body may close an ask an earlier one opens.

A dry run does steps 1 to 3, prints each item's tier, rule and body, and writes nothing except `beacon run --first`,
which records in `first.json` which sources completed that summary. It does not write a cursor, the outbox, the
queue, or `pass.json`. It may contact source APIs to collect; it does not contact Signal. It reads the outbox, the
hold queue and the cursor, so it shows what a send pass would do next. Without `--once` or `--first`, the pass
repeats every `poll_interval`, and it still writes nothing.

## Packages

| Package | Role |
| --- | --- |
| `cmd/beacon` | The CLI. Builds everything from the config; the only place that wires the keychain and the transport. |
| `internal/source` | The `Item` shape, the `Source` interface, and the fixture source. |
| `internal/github`, `internal/jira` | The GitHub and Jira sources. See [`sources.md`](sources.md). |
| `internal/google` | The Gmail, Calendar and Drive sources, the OAuth grant they share, and the installed-app flow `beacon source add google` runs. |
| `internal/slack` | The Slack source, and the `auth.test` check `beacon source add slack` runs before it stores the token. |
| `internal/cursor` | Committed cursors, which sources have completed `beacon run --first`, and which of those have been accepted. |
| `internal/seed` | The seed file (DEC-24). |
| `internal/limited` | Rate-limit backoff for source HTTP. |
| `internal/config` | Reads and validates the config file. Holds no secrets. |
| `internal/keychain` | The only code that reads or writes a credential. macOS Keychain through the Security framework, refused to a build not signed with a certificate, with items that trust the creating build's designated requirement and `Repair` to re-create them; and an in-memory store for tests. It also builds OAuth token requests and reads token responses, so a token's bytes stay in it. |
| `internal/rules` | Parses the rules file, decides tiers, and checks for rules that can never match. |
| `internal/project` | Builds the request body from an item and its rule. |
| `internal/outbox` | The append-only record of every body, and each key's state. |
| `internal/queue` | The hold queue and the record of dropped keys. |
| `internal/signal` | The Signal client: `POST /api/events`, `GET /api/restrictions` when a personal source is enabled, and `GET /api/me` when `beacon key set` checks a key. Nothing else. |
| `internal/lease` | The send lease: a lease in Signal's lease API, else a claim event per period. |
| `internal/runner` | One pass: collect, decide, record, send. The only code that hands item content to the client. |
| `internal/egress` | The list of hosts Beacon may contact, and the transport guard that enforces it. |
| `internal/logs` | The log: JSON lines with fixed event names. |
| `internal/service` | The per-user launchd agent: the plist, the last-pass record, log rotation. It does not read a credential and it does not send. |

Test helpers: `internal/signal/signaltest` (a fake Signal with Signal's key rules),
`internal/egress/egresstest` (a transport that fails the test on any host not on the egress list),
`internal/google/googletest` (a fake Google: the token endpoint and recorded Gmail, Calendar and Drive responses), and
`internal/slack/slacktest` (a fake Slack Web API with recorded answers for every Slack item).

## The state directory

| File | Holds | Leaves the machine |
| --- | --- | --- |
| `outbox.jsonl` | Every request body, exactly as sent, Signal's answers, and the decided item for each event append | The bodies, once each. The decided item stays |
| `held.jsonl` | Held items, in full | No |
| `dropped.jsonl` | Keys of dropped items and the rule that dropped them; one line per key, again only if another rule drops it | No |
| `instance` | This instance's random id, used in the lease | As the lease's run |
| `cursors.json` | Each source's committed cursor, opaque. A Gmail resume cursor, while a send pass's open-state scan is unfinished, is stored here too. A dry run does not write it | No |
| `first.json` | Sources whose `beacon run --first` completed. `accept-first` requires it | No |
| `accepted.json` | Sources whose first run was accepted (DEC-23) | No |
| `seed` | Imported seed lines (DEC-24) | No |
| `slack-names-<source>.json` | Slack display names and channel names by id, with when each was read, for a day. Only when a rule lists `title`, `channel_name` or `author_name`. A dry run does not write it | Only as fields and titles a rule lists |
| `retitled.json` | Subjects whose threads a send pass checked for a retitle and left alone, with a fixed reason (`not_opened`, `not_found`, `moved`, `renamed`, `same`, `unreadable`). No title | No |
| `pass.json` | The last pass: time, outcome, whether this instance held the lease, and this instance's id when it did. A failed start records the reason here even when it did not send, and the failure streak that sets the backoff. Personal sources the pass withheld, with the prefix and a fixed reason. No item content. A successful dry run does not write it | No |
| `service.leaf` | The SHA-256 of the signing certificate recorded by `beacon service install`. `beacon run` refuses a binary whose leaf does not match | No |
| `service.stdout.log`, `service.stderr.log` | What the launchd agent prints. Standard error is the JSON log. Rotated by size, one generation kept | No |

The directory is created `0700` and the files `0600`, and it is synced when a file in it is created. A writer holds
an exclusive `flock` on the outbox, so two `beacon run` processes cannot share a state directory, and the queue
files are opened for writing only under that lock. A line torn by a crash is cut off when a send pass opens the
file under the lock. A dry run and `beacon explain` take no lock. They read the outbox and the
queue files, skip a torn last line (it may be a record a sending process is appending), and create no file.

## Adding a source

A source implements `source.Source`: a name, a fixed field list, a fixed state field list (DEC-28), the hosts it
contacts, and `Collect`. `Collect` takes the committed cursor and returns the next one; the runner stores it after
deciding. Its hosts appear in `beacon egress` and in the transport guard without other changes. A source keeps its
credentials in the keychain through `internal/keychain`, and reads them per pass. A source that can skip reading
bodies implements `SetBodies`; the app tells it whether any rule could send one. A source whose subjects can be
personal implements `PersonalStem`, and its stem must be in `source.PersonalStems`. Every source but the fixture reads
open state on its first run and waits for `beacon accept-first`. The sources are [`sources.md`](sources.md).
