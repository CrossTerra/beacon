# Items, events, the outbox and the lease

## Items

Every source yields the same shape (`internal/source.Item`):

| Field | Meaning |
| --- | --- |
| `source` | The source name, lowercase (`github`). |
| `subject` | The source's stable id for the object (DEC-10): 1 to 200 printable ASCII characters. |
| `key` | `<source>:<native id>:<version>` (DEC-9). The same change always has the same key. |
| `type` | The change, lowercase words joined by `_` and `.` (`review_requested`). |
| `occurred_at` | When it happened at the source. |
| `actor` | The identity of who caused it (`github:user:dana`). |
| `own` | The person caused it (DEC-19). |
| `addressed` | The person was asked or mentioned. |
| `fields` | The source's fixed fields; only names in the source's field list. |
| `references` | Explicit references to other subjects (DEC-17). |
| `due_at` | The source's own due time, when it has one (DEC-21). |

## The request body

`internal/project` builds `POST /api/events` from an item and the rule that matched it. Field order is fixed and
map keys are sorted, so the same item, rule and outbox state give the same bytes.

```json
{
  "subject": "github:pr:acme/app#7",
  "type": "github.review_requested",
  "key": "github:pr:acme/app#7:42",
  "occurred_at": "2026-11-07T11:00:00.123Z",
  "title": "Add retries",
  "open": {"title": "Add retries"},
  "to": ["@winton"],
  "priority": "high",
  "due_at": "2026-11-09T17:00:00.000Z",
  "metadata": {"actor": "github:user:dana", "fields": {"repository": "acme/app", "url": "https://…"}}
}
```

| Field | Rule |
| --- | --- |
| `subject`, `key`, `occurred_at` | Always, from the item. `occurred_at` is UTC with milliseconds. |
| `type` | Always: `<source>.<item type>`. |
| `open` | Always (DEC-17): the event lands in the thread for its subject, opening it if needed. `open.title` is the title when the rule lists `title`. |
| `title`, `body`, `metadata` | Only what the rule lists; see [`rules.md`](rules.md#fields). |
| `by` | Only on the person's own acts, as `{"identity": person.identities[source]}`; omitted when none is configured (DEC-27). |
| `to` | `[person.handle]` when the rule has `ask: me` and the item is not the person's own act. Never `me`: Signal reads `me` as the acting actor (DEC-27). |
| `priority` | The rule's priority, with `to`. |
| `due_at` | The item's due time, with `to`. |
| `closes` | On the person's own acts, and on items a `close_by_others` rule sends: the keys of open asks on the same subject, oldest first, at most 20. |

Open asks are read from the outbox: bodies on the subject with `to` that are pending, sent or replayed, minus
those any such body closes. Beacon writes every ask without `by`, so its integration actor is the asker; an own
act closes as the addressee (through `by`), and a `close_by_others` event closes as the asker (DEC-27).

A body over Signal's 64 KB request limit, or with a body over 64 KB, is not appended; the pass reports the item.

## The outbox

`outbox.jsonl` in the state directory. Each line is one JSON object. The first line for a key appends its body:

```json
{"key":"github:pr:acme/app#7:42","state":"pending","at":"…","kind":"event","subject":"github:pr:acme/app#7","rule":"review-requests","body":"{\"subject\":…}","sha256":"…"}
```

`body` is the exact request body as a JSON string; `sha256` is its digest, checked on every load. Each later line
records Signal's answer and is the key's state from then on:

```json
{"key":"github:pr:acme/app#7:42","state":"sent","at":"…","event_id":"evt_…","status":201}
```

| State | Meaning | Next |
| --- | --- | --- |
| `pending` | Appended and synced; not yet answered. | Sent, with the stored bytes, by the next pass that holds the lease. |
| `sent` | `201`: Signal wrote it. | Final. |
| `replayed` | `200` with `replayed: true`: Signal already had these bytes under this key, for example after a crash between send and record. | Final. |
| `rejected` | Signal refused it: `409 idempotency_conflict` (the key holds another request), or another final 4xx. `status`, `code` and `message` say why. | Final. Never sent again, never rewritten. |

`kind` is `event` for items and `lease` for lease claims. A line torn by a crash while it was written is dropped on
the next open; it was never synced, so its body was never sent.

### How Signal's answers are handled

| Answer | Beacon |
| --- | --- |
| `201` | `sent`. |
| `200`, `replayed: true` | `replayed`. |
| `409 idempotency_conflict` | `rejected`; reported by `run` and `outbox`. |
| `409 conflict` on `/by/identity` | Retried: the identity moved while Signal wrote, and Signal asks for the same request again. |
| `429` | Waits for `Retry-After` (at most two minutes), then retries. |
| `5xx`, network error | Retries with backoff (1 s doubling to 30 s, five attempts). Then the body stays pending and the pass stops sending. |
| `401` | The pass stops; the body stays pending. |
| Any other 4xx | `rejected`. |

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
- When the holder stops, another instance takes over within two periods.
- A restarted instance has the same instance id and outbox, so its claims are the same requests and replay.
- Each claim closes the holder's previous claim, so at most two of its asks to itself stay open.
- Claims go through the outbox like any body. Beacon does not read Signal's log for the lease: the answer to a
  claim says who holds the period.
- A pass stops sending when the lease it holds runs out mid-pass.

Periods come from the local clock. Two machines whose clocks differ by more than a period can disagree about which
period is current; the key still allows one holder per period.
