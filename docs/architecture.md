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
   refusal that still has its decided item, and does not touch a `409`. An item that `covers` another (a GitHub or
   Calendar `snapshot`) is decided after the rest of the pass, and skipped when the change it covers is sent or waiting to be.
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
queue, `pass.json` or `coverage.json`. It may contact source APIs to collect; it does not contact Signal. It reads the
outbox, the hold queue and the cursor, so it shows what a send pass would do next. Without `--once` or `--first`, the
passes repeat on the source loops below, and a dry run still writes nothing.

## Source loops

`beacon run --once` makes one pass over every source in turn. The running agent instead runs each source on its own
loop (DEC-41, `internal/schedule`): a source's pass is step 1 for that source alone (`runner.Gather`), then steps 2 to
6 for what it collected (`runner.Process`), then a wait of the source's own interval before its next pass. Step 1
touches nothing shared, so every source's collect runs at the same time as the others'. Steps 2 to 6 share the outbox,
the hold queue, the cursors, the subject floor and the lease, so they run for one source at a time, under one lock, and
read the cursors and the first-run acceptance again first. Step 6 sends every pending body, whichever source's pass
decided it. A pass's thread retitles are of its own source's threads, since another source may be collecting.

```
slack    ──collect──┐ decide/record/send ┐ wait 15s ──collect──┐ decide/record/send ┐ wait 15s …
gmail    ──collect──┼─────────┐ decide/record/send ┐ wait 30s ──collect──┼─ …
github   ──────collect────────┼─────────────┐ decide/record/send ┐ wait 60s …
                    one at a time, under the lock
```

The scheduler compares the wall clock with the monotonic clock every five seconds. A wall clock that moved 30 seconds
or more further is a sleep: every loop makes a pass at once, and the sleep is logged and recorded as one, not as a
long pass. A send loop then records each source's coverage (`internal/coverage`): locally in `coverage.json`, which
`beacon status --coverage` prints, and in Signal while this instance holds the lease, as a heartbeat lease per source
and a state snapshot when the coverage changes ([events](events.md#coverage)).

## Packages

| Package | Role |
| --- | --- |
| `cmd/beacon` | CLI, setup and user service entry points. Builds the collector from the config and wires the bound vaults and transports. |
| `internal/source` | The `Item` shape, the `Source` interface, and the fixture source. |
| `internal/github`, `internal/jira` | The GitHub and Jira sources. See [`sources.md`](sources.md). |
| `internal/google` | The Gmail, Calendar and Drive sources, the OAuth grant they share, and the installed-app flow `beacon source add google` runs. |
| `internal/slack` | The Slack source, and the `auth.test` check `beacon source add slack` runs before it stores the token. |
| `internal/cursor` | Committed cursors, which sources have completed `beacon run --first`, and which of those have been accepted. |
| `internal/seed` | The seed file (DEC-24). |
| `internal/limited` | Rate-limit backoff for source HTTP. |
| `internal/config` | Reads and validates the config file. Holds no secrets. |
| `internal/keychain`, `internal/vault` | Redacting secrets, bound read and write vaults, and an in-memory store for tests. The only legacy Keychain reader is a one-time operator command. |
| `internal/rules` | Parses the rules file, decides tiers, and checks for rules that can never match. |
| `internal/project` | Builds the request body from an item and its rule. |
| `internal/outbox` | The append-only record of every body, and each key's state. |
| `internal/queue` | The hold queue and the record of dropped keys. |
| `internal/signal` | The Signal client: `POST /api/events`, `POST /api/leases` (the send lease and the sources' heartbeats), `GET /api/restrictions` when a personal source is enabled, `GET /api/events/{id}` and `GET /api/threads/{id}` for a retitle, and `GET /api/me` when `beacon key set` checks a key. Nothing else. |
| `internal/lease` | The send lease: a lease in Signal's lease API, else a claim event per period. |
| `internal/runner` | One pass: collect, decide, record, send. `Gather` collects one source and touches nothing shared; `Process` decides, records and sends what it collected. The only code that hands item content to the client. |
| `internal/schedule` | The source loops: each source's interval, the wake from sleep, one failing pass stopping them all. |
| `internal/coverage` | Each source's coverage: `coverage.json`, `beacon status --coverage`, and the heartbeat leases and snapshots it sends to Signal through the outbox. No item content. |
| `internal/egress` | The list of hosts Beacon may contact, and the transport guard that enforces it. |
| `internal/logs` | The log: JSON lines with fixed event names. |
| `internal/service` | The last-pass record and the retired LaunchAgent's status and log rotation. It does not read a credential and it does not send. |
| `internal/approvalserve`, `internal/approval`, `internal/review`, `internal/actsocket` | [Approvals](approvals.md), the append-only journal and outbox, verified passkey actions, and the same-user socket. The [Go reaper](supervision.md) controls child lifetime. |
| `internal/actsocket` peer checks | The client checks socket and directory ownership and permissions, then the listener's kernel UID on the connection that carries the request. The server checks the caller's UID before accepting any request. Neither checks code signatures. |

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
| `pass.json` | The last pass (with the source loops, the last source's pass; a personal source another pass withheld stays listed until its own next pass): time, outcome, whether this instance held the lease, and this instance's id when it did. A failed start records the reason here even when it did not send, and the failure streak that sets the backoff. Personal sources the pass withheld, with the prefix and a fixed reason. No item content. A successful dry run does not write it | No |
| `coverage.json` | Each source's coverage from the send loop: its interval, its last pass and how long the collect took, its last success, its last failure and reason code, how many passes in a row have failed, the last pass's cut and rate-limit flags, Slack's channel counts and oldest read, and the last sleep. Names, times, counts, flags and fixed codes only. A dry run and `--once` do not write it | As coverage snapshots ([events](events.md#coverage)) |
| `service.stdout.log`, `service.stderr.log` | The retired LaunchAgent's logs. Standard error is the JSON log. Rotated by size, one generation kept | No |

The supervised collector's pass summaries are in `collector.log` in the approval service's state directory,
with one previous file, `collector.log.1` ([supervision](supervision.md)). No certificate pin protects the
installed binary. The user audits the commit and controls the binary and its service definition.

The directory is created `0700` and the files `0600`, and it is synced when a file in it is created. A writer holds
an exclusive `flock` on the outbox, so two `beacon run` processes cannot share a state directory, and the queue
files are opened for writing only under that lock. A line torn by a crash is cut off when a send pass opens the
file under the lock. A dry run and `beacon explain` take no lock. They read the outbox and the
queue files, skip a torn last line (it may be a record a sending process is appending), and create no file.

## Adding a source

A source implements `source.Source`: a name, a fixed field list, a fixed state field list (DEC-28), the hosts it
contacts, and `Collect`. `Collect` takes the committed cursor and returns the next one; the runner stores it after
deciding. Its hosts appear in `beacon egress` and in the transport guard without other changes. A source keeps its
its credentials in the unattended vault through `internal/keychain`, and reads them per pass. A source that can skip reading
bodies implements `SetBodies`; the app tells it whether any rule could send one. A source whose subjects can be
personal implements `PersonalStem`, and its stem must be in `source.PersonalStems`. Every source but the fixture reads
open state on its first run and waits for `beacon accept-first`. The sources are [`sources.md`](sources.md).
