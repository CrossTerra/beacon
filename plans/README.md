# beacon — plan of plans

This directory holds work to be done. `docs/` holds how the system works. When a phase completes, its durable
content (config format, rule grammar, event shapes) moves into `docs/`, and the phase is marked `complete`.

Read this file in full before writing code, especially **Decisions**. A decision here overrides anything older.

## Goal

One agent per person that turns activity in the services they use into a small number of actionable events in
Signal, without handing those services' credentials or unfiltered content to anyone. An agent that reads Signal
(for example a sweep that tells a person what needs them) then reads one log instead of polling every service.

## Decisions

| # | Decision |
| --- | --- |
| DEC-1 | Go. One static binary with a CLI and a background mode. A native menu-bar shell for macOS can wrap it later. Few dependencies, each one justified in its pull request. |
| DEC-2 | Source credentials live in the OS keychain and never leave the machine. Each source uses the person's own OAuth or API token. |
| DEC-3 | Default deny. Nothing is sent unless a rule matches it with `send` or `hold`. |
| DEC-4 | Three tiers per item: `send` (automatic), `hold` (local review queue, sent on approval), `never` (dropped, logged locally). |
| DEC-5 | Rules are a YAML file the person owns and reviews like code. First match wins. Rules match on source fields (source, project, repository, channel, sender, label, type, whether the person is addressed). |
| DEC-6 | Each source has a fixed field list. A rule names the fields it sends. Full bodies leave when a rule names `body`. Attachments and file contents are out of scope for the first release. |
| DEC-7 | The classifier is optional and local by default. It sees only items the rules allowed and may only narrow them (`send` to `hold` or `never`, `hold` to `never`). A hosted model is an explicit setting listed by `beacon egress`. |
| DEC-8 | Every request body is appended to a local outbox before it is sent. A retry resends the stored bytes, because Signal answers a retried key with a different body with `409 idempotency_conflict`. |
| DEC-9 | One event per change of state. The event key is `<source>:<native id>:<version>`, where the version is the source's own marker (an `updated` time, a status, a message id). The same change always produces the same key. |
| DEC-10 | Subjects are the source's stable id, so other writers about the same thing land on the same thread: `github:pr:<owner>/<repo>#<n>`, `jira:<KEY>`, `slack:<channel>:<thread ts>` for channels. Personal sources carry the person, because their ids are per mailbox or shared between participants: `gmail:<person>:thread:<id>`, `calendar:<person>:<event id>`, `drive:<person>:<file id>`, `slack:dm:<person>:<channel>:<ts>`. Signal restricts these prefixes to their owner and refuses a write naming another person (Signal phase 11). Event keys are unique per writing actor, so two people's Beacons can record the same message. |
| DEC-11 | Beacon writes as one Signal integration actor per person, with `write` and `relay` scoped to the identity prefixes of its sources (`github:*`, `slack:*`, `email:*`). Narrowed by DEC-27: `by` records the person's own acts only; anyone else who caused a change is recorded in `metadata.actor` when a rule lists `actor`. |
| DEC-12 | Beacon asks for attention only for its own person, and only when a rule says so (`ask: me`). Routing work to other people belongs to agents that read Signal. |
| DEC-13 | Rule proposals, not silent learning. When the person dismisses items, Beacon can propose a rule. A proposal changes nothing until it is accepted, and an accepted proposal is a diff to the rules file. |
| DEC-14 | No telemetry, update check or crash reporting. Egress is the source APIs, the Signal host, and a hosted classifier only if configured. |
| DEC-15 | Nothing is specific to one organization. The Signal URL, sources, identities and rules are configuration. |
| DEC-16 | Releases are reproducible from a tag, signed, notarized on macOS, and ship an SBOM. |
| DEC-17 | Threads follow the native object. Beacon writes with `open` on the object's subject, so each pull request, issue, mail thread, Slack thread, calendar event and document is one thread. Links across sources are recorded only from explicit references (an issue key in a pull request's title or branch, a URL in a message) as `metadata.references`. Beacon never merges threads; a filer may. |
| DEC-18 | A change of state is defined per source (see **Change markers**). Edits, reactions, read receipts and label churn are not events. |
| DEC-19 | The person's own acts are events: a reply sent, a review submitted, an issue transitioned, a message posted in a thread Beacon follows. They are what closes an ask. Without them a reader has to guess whether something was answered. |
| DEC-20 | An own act on a subject with an open ask to the person closes that ask (`closes`, outcome `handled`). Another person's answer in the same thread closes it only when a rule says so. |
| DEC-21 | `ask: me` in a rule sets `to: me`, with priority from the rule and `due_at` from the source when the source has one (an issue's due date, a calendar start). Without `ask`, an event is recorded and asks no one. |
| DEC-22 | The classifier is a local OpenAI-compatible endpoint (Ollama or llama.cpp) serving a small open-weights instruct model. Each decision is kept in a local decisions log with the model digest, the prompt hash and the answer. Decisions are never sent. Until Phase 6, rules and `hold` do the work. |
| DEC-23 | The first run reads open state, not history: open pull requests the person wrote or was asked to review, unresolved issues assigned to them, inbox and starred mail from the last 14 days, direct messages, mentions and followed threads from the last 7 days, and calendar events in the next 7 days. It is always a dry run. Sending starts only after the person accepts its summary. |
| DEC-24 | A seed file can carry decisions from an earlier triage system (`<source id>`: `never` or `send`), so items already dismissed stay dismissed. |
| DEC-25 | One Beacon per person, one active instance. The instance holds a lease in Signal (a claim event renewed every few minutes); another machine collects but does not send while the lease is held. Cursors, the outbox, the queue and the decisions log stay local. |
| DEC-26 | Local agent sessions are a source. A coding agent's session that finishes, fails or stops for a decision is an event on the work it was started for, so whoever delegated the work does not have to poll it. |
| DEC-27 | Signal resolves `by` only to an identity an actor already holds (otherwise `422 identity_not_found`), reads `me` in `to` as the acting actor (Signal DEC-122), and lets only the addressee or the asker close an ask. So: `to` names the person's Signal handle from the config, never `me`. `by` is set only on the person's own acts, from the identities the config lists for the person. Anyone else who caused a change is recorded in `metadata.actor`, and only when the rule lists `actor`. Beacon writes every ask without `by`, so Beacon is the asker, and an event written for `close_by_others` (also without `by`) closes the ask as the asker. Email identities are `email:`, not `mailto:`. When Signal records external identities (Dependencies on Signal, item 5), attribution can move from `metadata.actor` back to `by`. |
| DEC-28 | Events carry a state snapshot. When a source returns the object's current state, the event's `state` holds the fields that describe it now (for a pull request: state, head commit, review decision at head, mergeable, checks; for an issue: status, assignee, due date), with `observed_at` and the source's version marker. The fields that may appear in `state` are listed per source like any other field, and a rule names them. |
| DEC-29 | One authoritative event per act. When an executor or an agent already recorded an act the person's credentials performed (a merge, a comment, a message), Beacon does not record it again. It matches the act by the source's native id in `metadata.native`; when it cannot match, it records a plain observation linked to the authoritative event. |

## Change markers

| Source | Events | Version marker |
| --- | --- | --- |
| GitHub | pull request opened, closed, merged, marked ready; review requested or removed; review submitted (state and commit); new head commit on a pull request the person reviewed; check suite conclusion changed on the person's own pull request; comment by someone else; the person's own review or comment | `updated_at` plus the change's own id |
| Jira | assigned or unassigned; status transition; comment by someone else; mention; due date changed; the person's own transition or comment | changelog id or comment id |
| Gmail | new message in a thread (from someone else, or sent by the person); starred | message id |
| Calendar | created, changed or cancelled; the person's response needed; the person's response changed | event `updated` and `etag` |
| Drive | comment or reply that mentions the person or is assigned to them; meeting notes created for a meeting the person attended | comment id, file id |
| Slack | direct message; mention; reply in a thread the person posted in; the person's own message in a followed conversation | message `ts` |
| Agent sessions | started, finished, failed, waiting for input | session id plus state |

## Phases

| Phase | Plan | Status | Depends on |
| --- | --- | --- | --- |
| 1 | Trust core: config, keychain, rules engine, projection, outbox, Signal client, `explain`, `egress`, dry run | complete | — |
| 2 | [First sources: GitHub and Jira](phase-2.md) | pending | 1 |
| 3 | Google: Gmail, Calendar, Drive (comments and mentions) | pending | 1 |
| 4 | Slack: direct messages, mentions, threads the person is in | pending | 1 |
| 5 | Review: the hold queue and rule proposals, in the terminal first | pending | 2 |
| 6 | Local classifier | pending | 5 |
| 7 | Release engineering: reproducible builds, signing, notarization, SBOM, install docs | pending | 2 |
| 8 | macOS menu-bar shell | pending | 5, 7 |
| 9 | Local agent sessions as a source (DEC-26) | pending | 1 |

Each phase gets its own plan file with goal, scope, out of scope, design notes, tests and an exit gate before work
starts. Phase 1 is [`phase-1.md`](phase-1.md).

### Phase 1 sketch

- `beacon init`: writes a config with the Signal URL and an empty rules file, and stores the Signal key in the
  keychain.
- `beacon run --dry-run`: collects, applies rules and projection, and prints what would be sent and why, without
  sending. It is the default until the person turns sending on.
- `beacon explain <item>`: shows the matching rule, the classifier's answer if any, and the exact body.
- `beacon outbox`: lists what was sent, with keys, times and the Signal event ids.
- `beacon egress`: prints every host Beacon may contact under the current config.
- Exit gate: against recorded fixtures, every item lands in the expected tier, held and dropped items never reach
  the network, a killed process resumes without double sends, and a retry resends the stored bytes.

## Dependencies on Signal

| Need | Why | State in Signal |
| --- | --- | --- |
| Per-event visibility | Rules may send full bodies from personal sources. Without visibility, any read key can read them. | Designed, not built |
| Batch append | A first sync can produce thousands of events. | Deferred |
| Redaction | Recovery from a rule that sent too much. | Designed, not built |
| An integration actor and key per person, with `relay` prefixes | DEC-11 | Built |
| External identities | `by` for people who are not Signal actors (DEC-27) | Not designed |

## Dependencies on Signal, in order

1. **Visibility for personal subjects.** Events on `gmail:`, `slack:` direct-message, `drive:` and `calendar:`
   subjects are visible only to the person whose Beacon wrote them and to actors that person grants. This is a
   first slice of Signal's subject visibility design, and it comes before any rule that sends a body from those
   sources.
2. **Batch append**, before the first real run of Phase 3.
3. **Redaction**, before Beacon is offered to anyone else.
4. **State snapshots and one authoritative event per act** (Signal phase 12), for DEC-28 and DEC-29.
5. **External identities.** Signal records people who are not actors, for example by registering an identity the
   first time a relay names it in `by`, so a relayed event can say who caused it without an admin attaching every
   outside identity first. Until then Beacon records other people in `metadata.actor` (DEC-27); with it,
   attribution moves back to `by`.

## How a reading agent changes

Written for a sweep agent that today polls each service and keeps its own ledger:

- The sweep reads the log after its cursor and the person's open asks. It does not call the services.
- Its triage (priority, next step, draft reply) is written back as events on the same thread, so the next run and
  every other reader see it. The private ledger becomes a query.
- What closed an item is an event: the person's own reply, a merge, a transition. Reconciling stops being a
  heuristic.
- Its own actions (a message sent, a pull request merged, a runner started) are events caused by the item they
  answer, so the record of what was done and why is in one place.
- A watcher on the log replaces the daily batch: a new ask for the person can be triaged within minutes.
