# Phase 10 — Sealed events

Status: pending (proposed). Signal's side is Signal plan 12.08, "Sealed events"
([`plans/phase-12/12.08-sealed.md`](https://github.com/CrossTerra/signal/blob/main/plans/phase-12/12.08-sealed.md),
Signal DEC-250 to DEC-270). Read [`README.md`](README.md), [`../docs/trust-model.md`](../docs/trust-model.md),
[`../docs/rules.md`](../docs/rules.md) and that plan first.

## Goal

A rule can send an item **sealed**: its content encrypted on the person's Mac, before it is appended to the outbox, to
the person's own key alone (`owner`) or also to the keys of the agents that work for them (`owner+agents`). Signal,
its database, its admins and its developers then hold ciphertext and a fixed envelope, and only the person (and, at
`owner+agents`, their agents) can read what Beacon sent.

Bodies from personal sources (Gmail, Slack direct messages, group direct messages and private channels, meeting notes
and transcripts, Drive comments, calendar descriptions) leave the machine only sealed. Sealing can be raised on the
machine by sensitive words, senders and labels, never lowered. With that in place, a sealed rule may `send` without a
per-item hold, and Winton's mail and Slack bodies can reach Signal.

## Why

Today a rule that lists `body` on a personal source sends the text in plaintext to a store whose operators can read it
(trust model, "Beacon trusts the Signal host it is pointed at"), so the safe advice is to hold those rules
(known limits, "Signal cannot delete"). Signal 12.08 makes the store unable to read sealed content, and gives it a
delete for sealed items (crypto-shredding). Beacon is the writer that has to seal correctly: it decides the level, fixes
the envelope, finds and checks the recipients' keys, and fails closed.

## Decisions

Proposed as DEC-30 to DEC-41, recorded in the [`README.md`](README.md#decisions) table when the phase is adopted.

| # | Decision |
| --- | --- |
| DEC-30 | A rule may seal. `seal: owner` or `seal: owner+agents` on a `send` or `hold` rule (never on `never`) seals every field the rule lists, except those named in `plain`, a subset of `fields` that may never hold `body` or `actor`. The envelope is fixed in code (DEC-31), not by the rule. A rule without `seal` works as today, subject to DEC-32. |
| DEC-31 | The envelope is fixed. A sealed event carries in plaintext only: the subject, the type, the event key, the event title (the change description from the source's fixed vocabulary, which already never holds content), `by` on the person's own acts, `to`, `priority`, `due_at`, `closes`, `outcome`, `occurred_at`, the `sealed` header, `metadata.fields` with the `plain` fields, `metadata.native` with an opaque id only, and boolean state fields (`unread`, `starred`). Everything else a rule lists goes in the ciphertext: `body`, `actor` (who wrote to the person, Signal DEC-253), `references`, every other field, every other state field. The thread title is a plain field or nothing. |
| DEC-32 | Personal bodies leave only sealed. A rule that lists `body` and can match an item under a personal stem (`gmail:`, `calendar:`, `drive:`, `slack:dm:`) needs `seal`: `beacon rules check` reports it as an error, and at run time such an item is held with the reason `body_needs_seal` rather than sent plain. Public Slack channel bodies (`channel_type: channel`) may stay plain. |
| DEC-33 | Sealing can be raised on the machine, never lowered. `seal.owner` in the config lists words, senders (addresses and domains) and labels. After a rule decides `send` or `hold`, an item is sealed at `owner` when any of them matches its text or its people, field by field per source: Gmail's `subject`, `body`, `labels`, and the addresses of `thread_participants` (every From, To and Cc in the thread, DEC-41); Calendar's `summary`, `body`, `organizer` and `attendee_addresses`; Drive's `title` and `body`, and for notes and transcripts their meeting's title, organiser and attendees; Slack's `body` and the text of the thread's root message, which Beacon reads with the thread anyway. A field the rule does not send is still matched, on the machine, from plaintext or from `owner+agents`. It cannot unseal, lower a level, or turn `never` into anything. Signal seals only a person's own subjects (Signal DEC-250), so a matching item on a shared subject (a public Slack channel) cannot be sealed: it is held locally instead, with the reason `sensitive_shared`, and the person decides. Raising is counted in the log, never explained there (AGENTS.md: no rule matches on content). |
| DEC-34 | A subject never goes down. Beacon keeps, per subject, the highest level it has sent there, and seals every later item on that subject at least at that level, so a reply in a Remotely thread from a colleague is still `owner`. Kept in the state directory beside the cursors (a level per subject hash, no content). The floor covers what Beacon has sent; what it never saw is covered by the raise matching the whole thread, not the one message (DEC-33), so a first run that reads only a thread's latest message, a colleague's reply, still finds Remotely among the thread's participants. |
| DEC-35 | Notes and transcripts take their meeting's level. A `notes_created` or `transcript_created` item is raised to `owner` when its meeting matches `seal.owner` (DEC-33), and to the level of the meeting's calendar item when the rules seal that item higher. |
| DEC-36 | Recipients come from Signal and are verified on the machine. Beacon reads the person's keys with `GET /api/seal/keys`, checks every signature against the identity key it pinned (trust on first use; `beacon keys show` prints the fingerprint the website shows), drops agent keys Signal says have lapsed, and wraps for the current person key and, at `owner+agents`, each current agent key. A failure is not a decision, as when Signal's restrictions cannot be read (property 9): the source's sealed items are not recorded and its cursor stays, so a later pass decides them again, and nothing waits in the hold queue for a review that only Phase 5 could give. Two kinds are told apart in the log and in `beacon run`: keys that cannot be read (Signal unreachable, `401`, `5xx`; `withheld` with `seal_keys_unknown`), which pass on their own, and keys Signal returned that cannot be used (a changed identity key, a bad signature, no current person key; `withheld` with `seal_keys_unusable`), for which `beacon run` and `beacon service status` name the command to run (`beacon keys trust`, `beacon keys init`). Never plaintext, never fewer recipients than the level needs. |
| DEC-37 | The cryptography is the standard library. Go 1.26 (from 1.24) for `crypto/hpke`; `crypto/aes` and `crypto/cipher` for AES-256-GCM, `crypto/ecdh` for X25519, `crypto/ed25519`, `crypto/sha256`, `crypto/rand`. No new module. The construction is Signal DEC-254's, and the test vectors are shared with Signal's `examples/sealed/`. |
| DEC-38 | Beacon is the person's device for keys. The identity key, every generation of the person key, and nothing of any agent's, are generic passwords in the login keychain under the service `beacon`, accounts `seal:<handle>:identity` and `seal:<handle>:person:<kid>`, readable only by Beacon's signed binary and on this device only (not synced). Only the keychain package reads them. Beacon never decrypts an item in a pass; it holds the private keys to sign key events, to enrol agents, to recover, to rotate and to open an item on request (`beacon keys open`). Signal takes an identity key and re-wraps only from the person's own key (Signal DEC-256, DEC-259); for those two acts Beacon reads a short-lived key of the person's from standard input, uses it for that command, and never stores or logs it. |
| DEC-39 | Encrypt once; the outbox keeps the sealed bytes. The request body appended and synced to the outbox (DEC-8) is the sealed one, so a retry resends the same ciphertext Signal compares by key. The stored decision keeps listed fields locally as today, so `beacon outbox retry --rejected` re-seals from it with a new content key. |
| DEC-40 | Sealed rules may `send`; plaintext personal content stays `hold`. A `send` rule with plain fields from a personal source is a warning in `beacon rules check` ("plaintext personal content: start as hold") until its output has been checked. |
| DEC-41 | Fields the worked example needs. Gmail: `cc`, `participants` (from, to and cc of the message), `thread_participants` (every from, to and cc in the thread, from the thread metadata Beacon already reads for `subject` and the thread state), and `bulk` (`true` with `List-Unsubscribe`, `List-Id`, `Precedence: bulk`, `list` or `junk`, or a Gmail category label other than Personal), all from headers `gmail.metadata` already reads. Drive: `transcript_created` for a meeting's Google Meet transcript document, found as notes are, and `meeting_organizer` and `meeting_attendees` on notes and transcripts. Calendar: `attendee_addresses`, the listed attendees' addresses (today `attendees` is only a count). Fields may be used in `match` without being listed in `fields`, as today. |

## Scope

### 10.1 Keys

- Go 1.26 in `go.mod`, CI and `docs/development.md`; the reproducibility check on the new toolchain.
- `internal/seal`: sealing an item (content key, AES-256-GCM, PADMÉ, the binding, HPKE wraps), opening one, key ids,
  canonical JSON, signing and verifying key events, the key fold (as Signal's), and the pin file. Pure, with randomness
  passed in; private keys reach it from the keychain package as opaque handles, never as bytes outside it.
- `internal/keychain`: the seal accounts (DEC-38), with `kSecAttrAccessibleWhenUnlockedThisDeviceOnly`.
- `internal/signal`: `GET /api/seal/keys`, `GET`/`PUT /api/seal/secrets`, `POST /api/events` for `seal.` key events.
- Commands ([`docs/commands.md`](../docs/commands.md)):
  - `beacon keys init`: generate the identity and person keys, publish them, print the recovery key once (and refuse to
    print it again), store the recovery blob. Signal takes an identity key only from the person's own key (Signal
    DEC-256), which Beacon does not hold, so `init` reads a short-lived key of the person's (from the website's _Keys_
    page or `signal admin create-key --expires-in 10m`) from standard input with echo off, uses it for the identity
    event alone, and never stores or logs it; the person key and later events go with Beacon's own key. `--rewrap` in
    `rotate` reads one the same way, since only the person's own key writes re-wraps. Refused when Signal already lists an identity key for the person: then
    `beacon keys recover`, or `beacon keys trust` for one made in the browser.
  - `beacon keys show`: the person's fingerprint, current and retired person keys, agent keys and whether each is
    current; pinned fingerprints of others.
  - `beacon keys trust <fingerprint>`: pin a person's identity key after comparing it, including after a reset.
  - `beacon keys recover`: read the recovery key from standard input with echo off, open the blob, store the keys.
  - `beacon keys agent add <@handle> --public-key <base64>` and `beacon keys agent revoke <@handle> [--purge]`: sign
    and publish, or revoke, an agent's key (Signal DEC-260).
  - `beacon keys rotate [--rewrap]`: a new person key; with `--rewrap`, wraps every past item of the person's for it
    (Signal DEC-259).
  - `beacon keys open <evt_…>`: decrypt one item to the terminal, for the person, not in a pass.
- `beacon egress` is unchanged: every call goes to the configured Signal host.

### 10.2 Rules, projection and send

- `internal/rules`: `seal`, `plain`, and the `rules check` errors and warnings (DEC-30, DEC-32, DEC-40).
- `internal/config`: `seal.owner` (`words`, `senders`, `labels`), each validated; words match whole words, case
  insensitive, unless written `{exact: "PIP"}`.
- `internal/project`: the sealed body (DEC-31): the envelope from the item and the rule, the plaintext from the listed
  fields that are not plain, sealed once, before the outbox append.
- `internal/runner`: the raise (DEC-33, DEC-35), the subject floor (DEC-34), recipients (DEC-36), `body_needs_seal`,
  and the two kinds of key failure, neither recorded, both keeping the cursor (DEC-36). Log events: `sealed` and
  `raised` with counts and levels only, `seal_keys_unread`, `seal_identity_changed`, `withheld` with
  `seal_keys_unknown` or `seal_keys_unusable`.
- `beacon explain` shows the level, which part of the config raised it (locally only), the envelope, and the
  plaintext only with `--bodies`.

### 10.3 Source fields

- Gmail: `cc`, `participants`, `thread_participants`, `bulk` (DEC-41).
- Calendar: `attendee_addresses` (DEC-41).
- Drive: `transcript_created`, `meeting_organizer`, `meeting_attendees`. Meet saves a transcript as a Google Doc
  attached to the event; English titles ending in `Transcript`, the same 32 KiB text export as notes, read only when a
  rule lists `body`.

### 10.4 Winton's rules, live

- `examples/rules-sealed.yaml`: the worked example below, checked clean against every source's fields.
- A dry run against Winton's real Gmail, Slack and Drive, reviewed by Winton for levels, not content: which items
  would go `owner`, `owner+agents`, plain, `never`, and which were raised.
- Then send to stage.

## Worked example: Winton's rules

Gmail and Slack bodies are sealed everywhere except public Slack channels. The owner list is one config block applied
to every item (DEC-33), and the senders are also rules so their automated mail is sealed rather than dropped as bulk.
Addresses in angle brackets are placeholders Winton fills in before the live dry run. `crossterra.co` is the company
domain.

`config.yaml`, in part:

```yaml
seal:
  owner:
    words: [salary, comp, compensation, offer, termination, "background check", incident, intrusion, legal,
            confidential, payroll, equity, vesting, {exact: PIP}]
    senders: [remotely.works, rippling.com, carta.com, <legal counsel domain>, <accounting firm domain>,
              <payroll provider domain>]
    labels: [Confidential]
```

`rules.yaml`:

```yaml
rules:
  # Owner only: people and services whose mail is the person's alone.
  - name: owner-senders
    match:
      source: gmail
      fields:
        thread_participants: {regex: '.*@(remotely\.works|rippling\.com|carta\.com|<counsel>|<accounting>|<payroll>)(,.*)?'}
    tier: send
    seal: owner
    fields: [subject, body, actor, from, to]
    ask: me
  - name: owner-label
    match: {source: gmail, fields: {labels: {regex: '(.*,)?Confidential(,.*)?'}}}
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

  # Never: bulk mail, after the owner senders and the notes so their automated mail is kept.
  - name: bulk
    match: {source: gmail, fields: {bulk: "true"}}
    tier: never

  # Owner+agents: the CEO, colleagues, direct messages and private channels.
  - name: rahul-mail
    match: {source: gmail, fields: {participants: {regex: '(.*,)?<rahul-address>(,.*)?'}}}
    tier: send
    seal: owner+agents
    fields: [subject, body, actor, from, to]
    ask: me
  - name: rahul-dm
    match: {source: slack, actor: "slack:user:<rahul-user-id>", fields: {channel_type: {regex: 'im|mpim|private'}}}
    tier: send
    seal: owner+agents
    fields: [body, actor, permalink, references]
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

  # Other mail from real people: the body sealed, the subject plain, so held until checked (DEC-40).
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

What it does with a few items:

| Item | Rule | Level | Plain in Signal |
| --- | --- | --- | --- |
| Rippling pay stub (bulk headers) | `owner-senders` | `owner` | subject id, "New message", unread |
| A colleague's reply in that thread | `employees`, then the subject floor | `owner` | the same |
| Rahul's mail about the board deck | `rahul-mail` | `owner+agents` | the same, and the ask |
| Rahul's mail with "offer" in the subject | `rahul-mail`, raised | `owner` | the same |
| Notes by Gemini for a 1:1 with a remotely.works attendee | `notes-and-transcripts`, raised | `owner` | "Meeting notes created" |
| A vendor's personal reply | `other-mail` (held) | `owner+agents` | the subject |
| A newsletter | `bulk` | `never` | nothing |
| "salary" in a public channel | `public-channels`, then held (`sensitive_shared`) | none: held on the Mac | nothing until Winton decides |

## Out of scope

- Bosun's key and decryption (its own repository), and the website's decrypt page (Signal 12.08).
- Sealing for GitHub and Jira, which are shared subjects.
- Sealing a source's state beyond its boolean flags: Gmail's `labels` and `last_from` go in the ciphertext when listed.
- Calendar rules: unchanged here, except that a description listed as `body` needs `seal` (DEC-32).
- Local search over sealed content; the classifier (Phase 6), which will see the plaintext on the machine like the
  rules do.

## Design notes

- **Fail closed, everywhere.** Every failure on the sealing path (keys unread, a bad signature, a changed identity, a
  wrap that will not build) holds the item. No path turns a sealed rule into a plain send, and no path sends to fewer
  recipients than the level needs.
- **The pin is not a secret.** The pinned fingerprints live in the state directory (`0600`), not the keychain: a
  local attacker who can rewrite them can also read the keychain, which the trust model already says Beacon cannot
  stop.
- **Order of the raise.** The rule decides tier, fields and level; then the raise, then the subject floor. The raise
  never adds fields, so it cannot send more than the rule lists, only seal it harder.
- **`owner` hides content from agents, not activity.** An `owner` ask still appears in the person's queue and in
  `for=me` with its priority and due time. The agents see that something sealed is waiting, not what.
- **Why Beacon holds the person's keys.** It runs as the person on the person's Mac, signs nothing but key events, and
  never decrypts during a pass. An agent like Bosun gets its own key and only `owner+agents` items.

## Tests

- `internal/seal`: sealing and opening; every binding field flipped fails; PADMÉ sizes; the shared vectors open with
  Signal's `examples/sealed/open.mjs` output and Signal's open Beacon's; the key fold with rotation, revocation,
  lapse and reset; a bad signature refused; a changed identity refused until trusted.
- `internal/rules`: `seal`, `plain`, `body` or `actor` in `plain` refused, `seal` on `never` refused, a personal
  `body` without `seal` an error, a plain-personal `send` a warning.
- `internal/project`: golden sealed bodies with a fixed random source; no listed, non-plain field in the plaintext
  part of the bytes, for every subset of fields; the envelope has exactly DEC-31's keys.
- `internal/runner`: the raise (each kind, never lowering, never reviving `never`), the subject floor across passes and
  restarts, a first run that sees only a colleague's latest reply in a Remotely thread raised by the thread's
  participants, a calendar event raised by its summary or an attendee's domain, notes taking their meeting's level, keys
  that cannot be read and keys that cannot be used each leaving the items unrecorded and the cursor in place, then
  sending on the first pass after `beacon keys trust`, `body_needs_seal`, and a public-channel
  item that matches `seal.owner` held as `sensitive_shared`.
- `internal/google`: `cc`, `participants`, `thread_participants`, `bulk` from headers; `attendee_addresses`; `transcript_created` and meeting fields against
  `googletest`.
- `cmd/beacon`: `beacon keys` commands against a fake Signal and a fake keychain, the recovery key printed once and
  never logged; a retry resends the same sealed bytes; the worked example's rules on fixtures land in the levels of the
  table above; logs carry no content and no raise reason.
- **End to end against Signal** (`scripts/signal-e2e.sh`): Beacon sends sealed fixture mail, DMs, notes and a
  transcript with canaries in every sealed field; then `pg_dump` of the Signal database holds none of the canaries and
  no 12-byte run of any sealed plaintext, while a plain public-channel canary is found; Signal's `open.mjs` opens an
  `owner+agents` item with an agent key and fails on an `owner` one.

## Exit gate

- **10.1** `scripts/gates.sh` passes on Go 1.26; the shared vectors pass in both repositories; `beacon keys init`,
  `show`, `recover` and `agent add` work against Signal's e2e stack.
- **10.2 and 10.3** the tests above pass, the dump test included; `docs/rules.md`, `docs/configuration.md`,
  `docs/commands.md`, `docs/sources.md`, `docs/events.md` and `docs/trust-model.md` describe sealing as built.
- **10.4** Winton reviews the live dry run's levels, then Beacon sends to stage; Signal 12.08's stage check (a dump of
  stage with no canary, the item opened in the browser, an agent opening only `owner+agents`) passes with Beacon as the
  writer.

## Docs to change

- `docs/trust-model.md`: "What crosses the boundary" (sealed bodies, the keys and blobs Beacon reads and writes);
  property 3 (sealed fields), property 7 (seal keys in the keychain), property 8 (no raise reasons in logs), property 9
  (personal bodies only sealed); a new property for sealing and failing closed; known limits: "Signal cannot delete"
  becomes true for plaintext only, "Signal has no per-event visibility yet" is replaced by what Signal can and cannot
  see of a sealed item (the envelope, sizes, who and when), and a limit for trust on first use.
- `docs/rules.md`: `seal`, `plain`, the envelope, the `rules check` additions.
- `docs/configuration.md`: `seal.owner`, the key's scopes (`read` for keys), Go version.
- `docs/commands.md`: `beacon keys`, `explain` with levels.
- `docs/sources.md`: new Gmail and Drive fields and items.
- `docs/events.md`: the sealed request body.
- `docs/development.md`: Go 1.26, the shared vectors.
- `plans/README.md`: the decisions, this phase's row, and "Dependencies on Signal" (sealed events, 12.08).

## Open questions

1. The sender of Gemini notes mail and of Meet transcript mail, and whether Winton's Workspace emails transcripts at
   all; the live dry run confirms both.
2. Whether `bulk` catches Rippling and Carta mail; it does not matter for them (their rules come first), but it does
   for anything not yet listed.
3. Whether the stored decision (DEC-39) should keep only the sealed bytes for sealed items, at the cost of
   `outbox retry --rejected` for them.
