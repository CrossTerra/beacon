# Items, events, the outbox and the lease

## Items

Every source yields the same shape (`internal/source.Item`):

| Field | Meaning |
| --- | --- |
| `source` | The source name, lowercase (`github`). |
| `subject` | The source's stable id for the object (DEC-10): 1 to 200 printable ASCII characters. |
| `key` | `<source>:<native id>:<version>` (DEC-9). The same change always has the same key. |
| `type` | The change, lowercase words joined by `_` and `.` (`review_requested`). |
| `event_title` | A short description of this change, used as the event's `title`. Not a source field, and not the object's title. GitHub and Jira set it. Empty means the event title is the `title` field when a rule lists it. |
| `occurred_at` | When it happened at the source. |
| `actor` | The identity of who caused it (`github:user:dana`). |
| `own` | The person caused it (DEC-19). |
| `addressed` | The person was asked or mentioned. |
| `fields` | The source's fixed fields; only names in the source's field list. |
| `references` | Explicit references to other subjects (DEC-17). |
| `due_at` | The source's own due time, when it has one (DEC-21). |
| `state` | The object's current state, when the source returned it (DEC-28): only names in the source's state field list. |
| `observed_at` | When the source returned `state`. Required with `state`. |
| `version` | The source's version marker for this change: the last part of `key` (DEC-9). Required with `state`. |

## The request body

`internal/project` builds `POST /api/events` from an item and the rule that matched it. Field order is fixed and
map keys are sorted, so the same item, rule and outbox state give the same bytes.

```json
{
  "subject": "github:pr:acme/app#7",
  "type": "github.review_requested",
  "key": "github:pr:acme/app#7:42",
  "occurred_at": "2026-11-07T11:00:00.123Z",
  "title": "Review requested from dana",
  "open": {"title": "Add retries"},
  "to": ["@winton"],
  "priority": "high",
  "due_at": "2026-11-09T17:00:00.000Z",
  "metadata": {"actor": "github:user:dana", "fields": {"repository": "acme/app", "url": "https://…"}},
  "state": {"fields": {"status": "open"}, "observed_at": "2026-11-07T11:00:05.000Z", "version": "42"}
}
```

| Field | Rule |
| --- | --- |
| `subject`, `key`, `occurred_at` | Always, from the item. `occurred_at` is UTC with milliseconds. |
| `type` | Always: `<source>.<item type>`. |
| `open` | Always (DEC-17): the event lands in the thread for its subject, opening it if needed. `open.title` is the object's title: the `title` field when the rule lists `title`, or the `summary` field when the rule lists `summary` and did not list `title`. It is not a description of this change. |
| `title` | For GitHub and Jira, a short description of the change ([`sources.md`](sources.md)). It is not the object's title, it is not a source field, and it never includes comment or review text. For any other source, the `title` field when the rule lists it. |
| `body`, `metadata` | Only what the rule lists; see [`rules.md`](rules.md#fields). `metadata.actor` is never set on the person's own acts, whose identity leaves only as `by`. |
| `by` | Only on the person's own acts, as `{"identity": person.identities[source]}`; omitted when none is configured (DEC-27). |
| `to` | `[person.handle]` when the rule has `ask: me` and the item is not the person's own act. Never `me`: Signal reads `me` as the acting actor (DEC-27). |
| `priority` | The rule's priority, with `to`. |
| `due_at` | The item's due time, with `to`. |
| `closes` | On the person's own acts, and on items a `close_by_others` rule sends: the keys of open asks on the same subject, oldest first, at most 20. |
| `state` | The state snapshot (DEC-28), in the shape Signal stores: `fields` holds the state fields the rule lists that the item has, and `observed_at` and `version` sit beside `fields`. Omitted when the rule lists none, the item has no snapshot, or none of the listed fields is present. Any other key in `state` is refused. |

Signal plan 12.01 stores the snapshot and rejects a `state` object with any key other than `fields`, `observed_at`
and `version`. The listed fields are nested under `state.fields`. Signal's replay check covers the whole request, so
a retry of a key Signal already holds, with different bytes, is `409 idempotency_conflict`. A key Signal refused and
did not store can be sent again with a corrected body.

DEC-29 (one authoritative event per act, matched by `metadata.native`) needs Signal phase 12 and is not part of
Phase 1 or Phase 2: Beacon records every own act it is sent.

Open asks are read from the outbox: bodies on the subject with `to` that are pending, sent or replayed, minus
those any such body closes. Beacon writes every ask without `by`, so its integration actor is the asker; an own
act closes as the addressee (through `by`), and a `close_by_others` event closes as the asker (DEC-27).

A body over Signal's 64 KB request limit, or with a body over 64 KB, is not appended; the pass reports the item.

## The outbox

`outbox.jsonl` in the state directory. Each line is one JSON object. The first line for a key appends its body:

```json
{"key":"github:pr:acme/app#7:42","state":"pending","at":"…","kind":"event","subject":"github:pr:acme/app#7","rule":"review-requests","body":"{\"subject\":…}","sha256":"…","decision":"{…}","decision_sha256":"…"}
```

`body` is the exact request body as a JSON string; `sha256` is its digest, checked on every load. `decision` is the
ids, times, listed fields, and event title used to project the body, also checked against `decision_sha256`. An
unlisted field, including a body, is not stored. It stays on the machine and is not part of the request. `beacon outbox` does not
print it. An append from before this was stored has no `decision`. Each later line records Signal's answer and is the key's state from then on:

```json
{"key":"github:pr:acme/app#7:42","state":"sent","at":"…","event_id":"evt_…","status":201}
```

| State | Meaning | Next |
| --- | --- | --- |
| `pending` | Appended and synced; not yet answered. | Sent, with the stored bytes, by the next pass that holds the lease. |
| `sent` | `201`: Signal wrote it. | Final. |
| `replayed` | `200` with `replayed: true`: Signal already had these bytes under this key, for example after a crash between send and record. | Final. |
| `rejected` | Signal refused it: `409` (the key holds another request, or another conflict), or another final 4xx. `status`, `code` and `message` say why. | A `409` is final. A `400` with no stored item is not a decision: the next collect that yields the key decides it again and appends a new attempt. Any other refusal may be re-decided by `beacon outbox retry --rejected` when `decision` was stored. The refused lines stay in the file. |
| `superseded` | The current rules no longer send a refusal. `code` is `hold` or `never`. | Final. Not sent. |
| `expired` | Lease claims only: the claim's period ended before Signal answered. | Final. Never sent again. |

`kind` is `event` for items and `lease` for lease claims. Pending counts (`run`, the report) count event bodies
only. A line torn by a crash while it was written is cut off the file when a send pass next opens it under the lock (a dry run skips it and changes nothing); it was never synced, so its
body was never sent. The directory is synced when the outbox file is created, so the file itself survives a crash.

### How Signal's answers are handled

| Answer | Beacon |
| --- | --- |
| `201` | `sent`. |
| `200`, `replayed: true` | `replayed`. |
| `409 idempotency_conflict` | `rejected`; reported by `run` and `outbox`. Never retried. |
| `409 conflict` on `/by/identity` | Retried, while the body is still pending: the identity moved while Signal wrote, and Signal asks for the same request again. A recorded `409` is not retried. |
| `429` | Waits for `Retry-After` (at most two minutes), then retries. |
| `5xx`, network error | Retries with backoff (1 s doubling to 30 s, five attempts). Then the body stays pending and the pass stops sending. |
| The lease runs out | Every write, with its retries and waits, runs under a deadline at the end of the lease. A write still waiting then is stopped, its body stays pending, and the pass ends (`lease_expired`). |
| `401` | The pass stops and `beacon run` exits non-zero; the body stays pending. |
| `400` with no stored item | `rejected`, but not a decision. The next collect that yields the key decides it again. |
| Any other 4xx | `rejected`. `beacon run` does not send it again. `beacon outbox retry --rejected` may re-project it from the stored decision and send the new body. |

### Retrying a refusal

`beacon outbox retry --rejected` runs the current rules on the stored item and sends only what they still send. It
requires `mode: send` and the lease. It logs counts only.

- An entry Signal accepted (`sent`, `replayed`) is not sent again.
- A `409` is not touched. Signal already holds a request under that key, or refused a conflict a new body must not paper over.
- A rejected event with no stored item is counted and not sent by this command. The refused body is not enough to rebuild the item.
- The current rules decide. `send` projects again, and a field leaves only when both the stored decision and the current rule list it. `hold` or `never`, including a seed `never`, records the entry `superseded` and does not send. A rule above the one that matched that could still apply, and that needs a field or actor the stored item does not have, cannot be proved, so the entry is `superseded` as `never` and not sent. A condition the item does have that already fails that rule does not block the send.
- The new body is appended and synced before it is sent. The refused body stays in the file. A pending retry, including one left pending because the lease ran out, is sent later as those exact bytes.
- `closes` includes an ask the outbox still has open, including one an earlier retry in this pass sent. A refusal is not an open ask.

A `400` with no stored item does not count as decided. Signal never stored it, so it must not block a later
decision. When a collect yields that key again, Beacon decides it, appends a new attempt with the decided item, and
sends that body. The refused lines stay in the file. A `409`, and anything Signal accepted, still blocks.

The 15 events refused before the item was stored are that case, and the GitHub cursor has already moved, so they
are not collected again until the cursor is cleared. Recovery is:

```bash
beacon cursor reset github
beacon run --once
```

`cursor reset` removes the committed cursor and does not send. The next send pass reads open state (DEC-23), not
history: open pull requests the person wrote or was asked to review, the current review requests, and checks on
their own open pull requests. Acceptance is left as it is, so the pass records and may send. A `400` with no stored
item is decided again and sent once, with the current projection, if that collect yields the key. The refused lines
stay in the file. A review already submitted, a comment, or a push is not read again. A `409`, and anything Signal
accepted, is not sent again.

## The lease

One instance per person sends (DEC-25). Time is cut into periods of `lease.period`. Period `N` is held by whoever
wrote the claim with key `claim:beacon:lease:<handle>:<N>`; Signal keeps one event per key, so one instance holds
each period. A claim is a Signal claim (a keyed ask to the writer itself, with `metadata.run`):

```json
{
  "subject": "beacon:lease:winton",
  "type": "beacon.lease_claimed",
  "key": "claim:beacon:lease:winton:5893412",
  "open": {"title": "Beacon lease for @winton"},
  "to": ["me"],
  "closes": [{"key": "claim:beacon:lease:winton:5893411"}],
  "metadata": {"run": "<instance id>:lease", "period_start": "2026-11-07T12:00:00Z", "period_seconds": 300}
}
```

- Each pass that sends first claims the current period, then the next one. The holder therefore always owns the
  period after the current one, and keeps the lease for as long as it runs.
- Another instance's claim on a held period differs in `metadata.run` and gets `409 idempotency_conflict`: it does
  not hold the lease, and sends nothing. It still collects and appends to its own outbox.
- Any other answer is not "held by another instance". A refused key (`401`) or a refused claim (another 4xx) fails
  the pass, and `beacon run` exits non-zero with the reason; an unreachable Signal ends the pass with the lease not
  taken, and the next pass tries again.
- A claim's write, with its retries, ends with its period. A claim still pending when its period has passed is
  marked `expired`, since no one can hold that period any more.
- When the holder stops, another instance takes over within two periods.
- A restarted instance has the same instance id and outbox, so its claims are the same requests and replay.
- Each claim closes the holder's previous claim, so at most two of its asks to itself stay open.
- Claims go through the outbox like any body. Beacon does not read Signal's log for the lease: the answer to a
  claim says who holds the period.
- A pass stops sending when the lease it holds runs out mid-pass.

Periods come from the local clock. Two machines whose clocks differ by more than a period can disagree about which
period is current; the key still allows one holder per period.
