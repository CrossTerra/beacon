# Phase 10 — Sealed rules

Status: pending (approved by Winton, 2026-11-11). Signal's side is Signal plan 12.08, "Sealed events"
([`plans/phase-12/12.08-sealed.md`](https://github.com/CrossTerra/signal/blob/main/plans/phase-12/12.08-sealed.md),
Signal DEC-250 to DEC-262). Read [`README.md`](README.md), [`../docs/trust-model.md`](../docs/trust-model.md) and
[`../docs/rules.md`](../docs/rules.md) first.

## Goal

A rule can mark what it sends as **sealed**: `seal: owner` (only the person reads it) or `seal: owner+agents` (the
person and the agents that work for them). Signal encrypts sealed parts at rest with KMS and opens them only for the
callers the level allows (Signal DEC-251, DEC-253, DEC-254). Beacon handles no keys: it sends over TLS as today, puts
the content in `sealed`, and keeps the envelope fixed. Bodies from personal sources leave only sealed, and a local word,
sender and label check can raise an item's level, never lower it. With that, a sealed rule may `send` without a
per-item hold, and Winton's mail and Slack bodies can reach Signal.

## Decisions

DEC-30 to DEC-37, recorded in the [`README.md`](README.md#decisions) table.

| # | Decision |
| --- | --- |
| DEC-30 | A rule may seal. `seal: owner` or `seal: owner+agents` on a `send` or `hold` rule (never on `never`) puts every field the rule lists in the request's `sealed` part, except those named in `plain`, a subset of `fields` that may not hold `body` or `actor`. A rule without `seal` works as today, subject to DEC-32. |
| DEC-31 | The envelope is fixed in code. A sealed event carries outside `sealed` only: the subject, the type, the key, `by` on the person's own acts, `to`, `priority`, `due_at`, `closes`, `outcome`, `occurred_at`, `metadata.fields` with the `plain` fields, `metadata.native` with the source's opaque id only (a Gmail message id, a Slack channel id and `ts`, a Drive file id; never a Message-ID header or anything that names a domain or a person, Signal DEC-264), and boolean state fields (`unread`, `starred`). The event title, `open.title` (unless its field is plain), the body, `actor` (who wrote to the person), `references`, every other field and every other state field go in `sealed`, as Signal requires (Signal DEC-252). |
| DEC-32 | Personal bodies leave only sealed. A rule that lists `body` and can match an item under a personal stem (`gmail:`, `calendar:`, `drive:`, `slack:dm:`) needs `seal`: `beacon rules check` reports it as an error, and at run time such an item is held with the reason `body_needs_seal` rather than sent plain. Public Slack channel bodies may stay plain. |
| DEC-33 | The level can be raised on the machine, never lowered. `seal.owner` in the config lists words, senders (addresses and domains) and labels. After a rule decides `send` or `hold`, an item is sealed at `owner`, from plain or from `owner+agents`, when any of them matches, per source: Gmail's `subject`, `body`, `labels` and `thread_participants`; Calendar's `summary`, `body`, `organizer` and `attendee_addresses`; Drive's `title` and `body`, and for notes and transcripts their meeting's title, organiser and attendees; Slack's `body` and the thread root's text. A field the rule does not send is still matched, on the machine. The raise cannot unseal, lower a level, add a field, or revive `never`. Signal seals only a person's own subjects, so a matching item on a shared subject (a public Slack channel) is held instead, as `sensitive_shared`. The log counts raises and never says why (AGENTS.md). |
| DEC-34 | A subject never goes down. Beacon keeps, per subject, the highest level it has sent there (a level per subject hash in the state directory, no content), and seals every later item there at least that high. With DEC-33 matching the whole Gmail thread, a colleague's reply in a Remotely thread is `owner` even on a first run. |
| DEC-35 | Notes and transcripts take their meeting's level: raised to `owner` when the meeting matches `seal.owner`, or when the rules seal the meeting's calendar item at `owner`. |
| DEC-36 | Sealed rules may `send`; plaintext personal content stays `hold`. A `send` rule with plain fields from a personal source is a warning in `beacon rules check` ("plaintext personal content: start as hold") until its output has been checked. |
| DEC-37 | Fields the example needs, from what the sources already read. Gmail: `cc`, `participants` (the message's from, to and cc), `thread_participants` (every from, to and cc in the thread, from the thread metadata already read for `subject`), and `bulk` (`List-Unsubscribe`, `List-Id`, `Precedence: bulk`, `list` or `junk`, or a category label other than Personal). Calendar: `attendee_addresses`. Address lists are lowercased and joined with `, `, as `to` and `labels` are today, so a rule's regex matches `(.*, )?name(, .*)?`. Drive: `transcript_created` for a Meet transcript document attached to a meeting (English titles ending in `Transcript`, read as notes are), and `meeting_organizer` and `meeting_attendees` on notes and transcripts. |

## Scope

### Part 1 — rules and sources

- `internal/rules`: `seal`, `plain`, and the `rules check` error and warning (DEC-30, DEC-32, DEC-36).
- `internal/config`: `seal.owner` (`words`, `senders`, `labels`); words match whole words, case-insensitive, unless
  written `{exact: PIP}`.
- `internal/project`: the sealed request body (DEC-31), appended to the outbox and resent byte for byte as today
  (DEC-8). Signal compares a retry by decrypting (Signal DEC-260), so nothing changes there.
- `internal/runner`: the raise (DEC-33, DEC-35), the subject floor (DEC-34), `body_needs_seal`, `sensitive_shared`; log
  events `sealed` and `raised` with counts and levels only.
- `internal/google`, `internal/slack`: the fields in DEC-37, and the Slack thread root's text for the raise.
- `beacon explain`: the level, what raised it (locally only), and the request body (plaintext only with `--bodies`).
- `examples/rules-sealed.yaml`: the worked example below, checked clean against every source's fields.

No keys, no new hosts (`beacon egress` is unchanged), no new dependency, no new scope on Beacon's Signal key.

### Part 2 — live

1. A live dry run against Winton's Gmail, Slack and Drive confirms the sender of Gemini notes mail and how Meet
   transcripts arrive (by mail, as Drive documents attached to the meeting, or both). The worked example is final only
   after this: the `gemini-notes-mail` rule takes the confirmed sender, a transcript mail rule is added if they arrive by
   mail, and `examples/rules-sealed.yaml` follows.
2. Winton reviews that dry run's levels, not its content: which items would go `owner`, `owner+agents`, plain or
   `never`, and which were raised.
3. Beacon sends to stage with the final rules.

## Worked example: Winton's rules

Gmail and Slack bodies are sealed everywhere except public Slack channels. The Gemini notes sender below is provisional
until Part 2's dry run confirms it. Placeholders in angle brackets are filled in
before the live dry run; `crossterra.co` is the company domain.

`config.yaml`, in part:

```yaml
seal:
  owner:
    words: [salary, comp, compensation, offer, termination, "background check", incident, intrusion, legal,
            confidential, payroll, equity, vesting, {exact: PIP}]
    senders: [remotely.works, rippling.com, carta.com, <counsel-domain>, <accounting-domain>, <payroll-domain>]
    labels: [Confidential]
```

`rules.yaml`:

```yaml
rules:
  # Owner only. Before the bulk rule, so their automated mail is kept.
  - name: owner-senders
    match:
      source: gmail
      fields:
        thread_participants: {regex: '.*@(remotely\.works|rippling\.com|carta\.com|<counsel>|<accounting>|<payroll>)(, .*)?'}
    tier: send
    seal: owner
    fields: [subject, body, actor, from, to]
    ask: me
  - name: owner-label
    match: {source: gmail, fields: {labels: {regex: '(.*, )?Confidential(, .*)?'}}}
    tier: send
    seal: owner
    fields: [subject, body, actor, from, to]
    ask: me

  # Meeting notes and transcripts: owner+agents, raised to owner by their meeting (DEC-35).
  - name: gemini-notes-mail
    match: {source: gmail, fields: {from: gemini-notes@google.com}}
    tier: send
    seal: owner+agents
    fields: [subject, body]
  - name: notes-and-transcripts
    match: {source: drive, type: {regex: '(notes|transcript)_created'}}
    tier: send
    seal: owner+agents
    fields: [title, meeting, meeting_start, body]

  # Never: bulk mail.
  - name: bulk
    match: {source: gmail, fields: {bulk: "true"}}
    tier: never

  # Owner+agents: the CEO, colleagues, direct messages and private channels.
  - name: rahul-mail
    match: {source: gmail, fields: {participants: {regex: '(.*, )?<rahul-address>(, .*)?'}}}
    tier: send
    seal: owner+agents
    fields: [subject, body, actor, from, to]
    ask: me
  - name: employees
    match: {source: gmail, fields: {from: {regex: '.*@crossterra\.co'}}}
    tier: send
    seal: owner+agents
    fields: [subject, body, actor, from, to]
  - name: slack-private
    match: {source: slack, fields: {channel_type: {regex: 'im|mpim|private'}}}
    tier: send
    seal: owner+agents
    fields: [body, actor, permalink, references]
    ask: me

  # Other mail from real people: the body sealed, the subject plain, so held until checked (DEC-36).
  - name: other-mail
    match: {source: gmail, type: received}
    tier: hold
    seal: owner+agents
    plain: [subject]
    fields: [subject, body, actor, from]

  # Plain: public Slack channels.
  - name: public-channels
    match: {source: slack, fields: {channel_type: channel}}
    tier: send
    fields: [body, permalink, references]
```

The `Confidential` label is both a rule, ahead of `bulk`, so labelled mail with list headers is kept rather than
dropped (the raise cannot revive `never`), and in `seal.owner`, so it also raises anything a later rule sends. The
sensitive words are only in `seal.owner`. Rahul's DMs fall under `slack-private`, already `owner+agents`.

| Item | Rule | Level | Plain in Signal |
| --- | --- | --- | --- |
| Rippling pay stub (bulk headers) | `owner-senders` | `owner` | the subject id, the type, unread |
| A colleague's reply in that thread | `owner-senders` (thread participants) | `owner` | the same |
| Rahul's mail about the board deck | `rahul-mail` | `owner+agents` | the same, and the ask |
| Rahul's mail with "offer" in the subject | `rahul-mail`, raised | `owner` | the same |
| A mail labelled `Confidential`, with list headers | `owner-label` | `owner` | the same |
| Notes by Gemini for a 1:1 with a remotely.works attendee | `notes-and-transcripts`, raised | `owner` | the type |
| A vendor's personal reply | `other-mail` (held) | `owner+agents` | the mail's subject |
| A newsletter | `bulk` | `never` | nothing |
| "salary" in a public channel | `public-channels`, then held (`sensitive_shared`) | none | nothing until Winton decides |

## Out of scope

Key handling of any kind (Signal holds it, with KMS); sealing GitHub and Jira (shared subjects); calendar rules beyond
DEC-32 and the raise; the classifier (Phase 6), which sees plaintext on the machine as the rules do.

## Tests

- `internal/rules`: `seal`, `plain`, `body` or `actor` in `plain` refused, `seal` on `never` refused, a personal `body`
  without `seal` an error, a plain-personal `send` a warning.
- `internal/project`: golden sealed bodies; for every subset of fields, no listed field outside `sealed` unless plain;
  the envelope has exactly DEC-31's keys, and `metadata.native` never holds `@` or a domain for any source.
- `internal/runner`: each kind of raise, never lowering and never reviving `never`; the subject floor across passes and
  restarts; a first run that sees only a colleague's reply in a Remotely thread; a calendar event raised by its summary
  or an attendee's domain; notes taking their meeting's level; `body_needs_seal`; `sensitive_shared`; logs with counts
  only.
- `internal/google`, `internal/slack`: the new fields against the fakes.
- `cmd/beacon`: the worked example on fixtures lands in the levels of the table above, including a `Confidential`
  label that sorts after a category label, and a matched address that is not first in its list.
- **End to end against Signal** (`scripts/signal-e2e.sh`, with Signal's local stand-in for KMS): Beacon sends sealed
  fixture mail, DMs, notes and a transcript with canaries in every sealed field; a `pg_dump` of the Signal database
  holds none of them, while a plain public-channel canary is found; an agent key working for the person opens the
  `owner+agents` items and not the `owner` ones, and Beacon's own relay key opens none. Signal's tests cover the sign-in
  key opening `owner`.

## Exit gate

**Part 1.** The tests above pass and `scripts/gates.sh` is green. `docs/rules.md`, `docs/configuration.md`, `docs/sources.md`,
`docs/events.md`, `docs/commands.md` (`explain`) and `docs/trust-model.md` describe sealing as built: what crosses the
boundary (sealed parts, in plaintext over TLS, encrypted by Signal at rest), properties 3 and 9 (bodies only sealed),
and the known limits ("Signal cannot delete" becomes true for plaintext only; Signal can still read sealed items, and
who it opens them for is Signal DEC-261).

**Part 2.** The dry run has confirmed the Gemini notes sender and how transcripts arrive, and the worked example and
`examples/rules-sealed.yaml` say so; Winton has reviewed the levels; Beacon sends to stage, and Signal 12.08's stage
check passes with Beacon as the writer.

## Open questions

1. Whether `bulk` catches mail no rule names yet; Rippling and Carta come first anyway.
