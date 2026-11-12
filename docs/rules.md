# Rules

The rules file is YAML that the person owns and reviews like code (DEC-5). Rules are tried in order; the first
that matches decides the item. An item no rule matches is `never` (DEC-3). Unknown keys are errors, and a key
written with no value is an error rather than "no condition", so a typo cannot widen a rule.

```yaml
rules:
  - name: own-acts
    match: {own: true}
    tier: send
    fields: [title, url]

  - name: review-requests
    match:
      source: github
      type: review_requested
      addressed: true
      fields:
        repository: {glob: "acme/*"}
    tier: send
    fields: [title, url, repository, actor]
    ask: me
    priority: high

  - name: answers
    match: {type: commented, fields: {label: answered}}
    tier: send
    fields: [title]
    close_by_others: true

  - name: everything-else-from-acme
    match: {fields: {repository: {regex: "acme/.+"}}}
    tier: hold
    fields: [title]
```

## A rule

| Key | Required | Meaning |
| --- | --- | --- |
| `name` | yes | Unique. Shown by `run`, `explain` and `outbox`. Never sent. |
| `match` | yes | The conditions. `match: {}` matches every item. |
| `tier` | yes | `send`, `hold` or `never` (DEC-4). |
| `fields` | no | What the event carries (DEC-6). See [Fields](#fields). |
| `state` | no | Which of the source's state fields go in the event's state snapshot (DEC-28). See [State](#state). |
| `ask` | no | `me`: the event asks the person (DEC-21). Nobody else can be asked (DEC-12). |
| `priority` | no | `low`, `medium`, `high` or `urgent`, for the ask. Needs `ask`. |
| `close_by_others` | no | `true`: when this rule sends another person's act, it closes the person's open asks on the same subject (DEC-20), or only those the item names when it sets `close_only`. |
| `seal` | no | `owner` or `owner+agents`: the event is sealed (DEC-30). See [Sealed rules](#sealed-rules). |
| `plain` | no | Fields of a sealed rule that stay outside `sealed`: a subset of `fields`, never `body`, `actor` or `references`. Needs `seal`. |

A `never` rule takes none of `fields`, `state`, `ask`, `priority`, `close_by_others`, `seal` or `plain`.

The file holds one YAML document. A second document (after `---`) is an error, not ignored.

A seed file can drop a subject before these rules. See [`commands.md`](commands.md#beacon-seed). That drop is the built-in
rule `seed`; it is not in this file, and a rule here cannot override it.

## Conditions

Every condition in `match` must hold.

| Key | Tests |
| --- | --- |
| `source` | The source name. |
| `type` | The item type, without the source (`review_requested`). |
| `actor` | The identity of who caused the change. |
| `own` | A YAML boolean (`true`, `True`, `TRUE`, `false`, `False`, `FALSE`): whether the person caused it. |
| `addressed` | A YAML boolean: whether the person was asked or mentioned. |
| `fields` | A mapping of field name to matcher, on the source's fields. An item without the field does not match. |

`yes`, `no`, `on`, `off`, numbers and quoted strings are refused for `own` and `addressed` rather than guessed.

A matcher is a plain string (exact), or a mapping with exactly one of the forms below. A blank value (`source:`,
`source: ""`, `source: ~`) is an error. A value YAML reads as a number or a boolean must be quoted
(`label: "2024"`).

| Matcher | Matches |
| --- | --- |
| `{exact: "acme/app"}` | That value. |
| `{glob: "acme/*"}` | `*` is any run of characters (including `/`), `?` is one character. The whole value must match. |
| `{regex: "acme/(app\|api)"}` | An RE2 expression. The whole value must match: the expression is anchored at both ends. |

## Fields

`fields` names what leaves the machine. Anything not listed is not in the request body.

| Name | Goes to |
| --- | --- |
| `title` | The thread title (`open.title`): the object's title. Trimmed, and shortened to Signal's 300 characters. The service sources do not use this field as the event title. A source that sets no event title still uses it for both. For Slack it is the conversation's title, built from display names and the channel's name, never message text, and listing it also lets a send pass rename threads Beacon opened before ([`sources.md`](sources.md#slack)). |
| `summary` | `metadata.fields.summary`. When the rule lists it and did not list `title`, it is also the thread title. An issue's or a calendar event's summary is its title. |
| `subject` | `metadata.fields.subject`. When the rule lists it and lists neither `title` nor `summary`, it is also the thread title. A mail thread's subject is its title. |
| `body` | The event body. Full bodies leave only when a rule lists `body` (DEC-6), and from a personal subject only sealed (DEC-32). A body is never copied into a title. A Google source does not read a body unless a sealed `send` or `hold` rule that can match it lists `body`; a Slack source reads message text on the machine and sets `body` only when a rule lists it. |
| `actor` | `metadata.actor`: who caused the change, when it was not the person (DEC-27). On the person's own acts it is left out; their identity leaves only as `by`. |
| `references` | `metadata.references`: explicit references to other subjects (DEC-17). |
| any other source field | `metadata.fields.<name>`. |

GitHub, Jira, Gmail, Calendar and Drive events carry a title that describes the change (see [`sources.md`](sources.md)). It is not listed in `fields`, and it never includes comment, review or mail text. Without `title`, `summary` or `subject`, the thread has no object title, and Signal titles a new thread with the event title. A source that sets no event title still uses `title` for the event and the thread; without it, Signal titles both with the type.

## State

`state` names the source's state fields that go in the event's `state.fields` (DEC-28). `observed_at` and the
source's version marker sit beside `fields`, not inside it. Each source lists its state fields as it lists its
fields. A state field not listed is not sent, and an item without a snapshot carries no `state`. The shape is in
[`events.md`](events.md#the-request-body).

## Sealed rules

A rule with `seal` puts what it sends in the request's `sealed` part, which Signal encrypts at rest and opens only for
the callers the level allows (Signal DEC-250 to DEC-254). Beacon handles no keys: it sends over TLS as for any event.

```yaml
  - name: other-mail
    match: {source: gmail, type: received}
    tier: hold
    seal: owner+agents      # or owner
    plain: [subject]        # left outside sealed; never body, actor or references
    fields: [subject, body, actor, from]
```

- `owner` opens only for the person signed in to Signal's website; `owner+agents` also for agents that work for them
  and for a grant with `sealed: true`. Beacon's own key opens neither.
- Outside `sealed` a sealed event carries only the envelope, fixed in code (DEC-31): the subject, the type, the key,
  `by` on the person's own acts, `to`, `priority`, `due_at`, `closes`, `occurred_at`, `open.title` when its field is
  in `plain`, `metadata.fields` with the `plain` fields, `metadata.native` with the source's opaque id (no `@`), and the
  `unread` and `starred` state flags as booleans. The event title, the body, `actor`, `references`, every other field
  and every other state field go in `sealed`. The shape is in [`events.md`](events.md#sealed-events).
- Signal seals only a person's own subjects: those under `gmail:`, `calendar:`, `drive:` and `slack:dm:`. A sealed rule
  that can match only shared subjects is an error in `beacon rules check`; at run time such an item is held as
  `sensitive_shared`.
- **Personal bodies leave only sealed (DEC-32).** A rule that lists `body` and can match an item under a personal
  stem needs `seal`: `beacon rules check` reports it as an error, and at run time such an item (with a body) is held
  as `body_needs_seal` rather than sent plain. A Slack rule matching only `channel_type: channel` (public channels)
  may send `body` plain.
- **The raise (DEC-33).** After a rule decides `send` or `hold`, an item is sealed at `owner`, from plain or from
  `owner+agents`, when a word, sender or label in the config's [`seal.owner`](configuration.md#seal) matches what the
  source read on the machine, whether or not the rule sends it: Gmail's subject, body, labels and thread
  participants; Calendar's summary, description, organiser and attendees; Drive's title and body, and for notes and
  transcripts their meeting's title, organiser and attendees; Slack's text and its thread root's text. The raise
  cannot lower a level, unseal, add a field or revive `never`. A match on a shared subject (a public Slack channel) is
  held as `sensitive_shared`, since Signal could not seal it.
- **Notes take their meeting's level (DEC-35).** A note or transcript is `owner` when its meeting matches
  `seal.owner`, or when Beacon sent the meeting's calendar event at `owner`.
- **A subject never goes down (DEC-34).** Beacon keeps the highest level it sent per subject (as a hash, in
  `seal-floor.json` in the state directory) and seals every later item there at least that high, a rule that sends
  plain included.
- A body over Signal's limits is cut to fit the 64 KiB of sealed parts and marked `truncated` inside `sealed`.

`beacon explain` shows an item's level and what raised it. The log counts sealed and raised items and never says why.
[`examples/rules-sealed.yaml`](../examples/rules-sealed.yaml) is a worked example.

## `beacon rules check`

Validates the file and reports, for the enabled sources:

- rules that can never match: a `source` that matches no enabled source, a `fields` condition on a field none of
  its sources has, or an earlier rule that matches every item this one could (for example `{own: true}` before
  `{own: true, type: commented}`, or `{}` before anything);
- a name in `fields` or `state` that none of the rule's sources has;
- as a warning, a rule with `own: true` for a source with no entry in `person.identities`: those own acts are sent
  without `by`, so Signal records them as Beacon's acts rather than the person's (DEC-27);
- a rule that lists `body`, has no `seal`, and can match an item under a personal stem (DEC-32);
- a sealed rule that can match only shared subjects;
- as a warning, a `send` rule that sends plaintext personal content: an unsealed rule with fields or state, or a
  sealed rule with `plain` (DEC-36, "start as hold until its output has been checked").

A Slack rule can match personal items unless its `channel_type` condition matches none of `im`, `mpim` and `private`.
A fixture's subjects are not known to the check, so neither personal check applies to it; the run still holds.

The cover check is conservative: it reports only what it can prove. It exits 1 on an error or an unreachable rule,
and 0 when there are only warnings. `beacon run` refuses to start on a rules error, and starts with unreachable rules
and warnings.
