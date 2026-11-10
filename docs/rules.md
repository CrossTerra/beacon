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

A `never` rule takes none of `fields`, `state`, `ask`, `priority` or `close_by_others`.

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
| `title` | The thread title (`open.title`): the object's title. Trimmed, and shortened to Signal's 300 characters. The service sources do not use this field as the event title. A source that sets no event title still uses it for both. |
| `summary` | `metadata.fields.summary`. When the rule lists it and did not list `title`, it is also the thread title. An issue's or a calendar event's summary is its title. |
| `subject` | `metadata.fields.subject`. When the rule lists it and lists neither `title` nor `summary`, it is also the thread title. A mail thread's subject is its title. |
| `body` | The event body. Full bodies leave only when a rule lists `body` (DEC-6). A body is never copied into a title. A Google source does not read a body unless a `send` or `hold` rule that can match it lists `body`; a Slack source reads message text on the machine and sets `body` only then. |
| `actor` | `metadata.actor`: who caused the change, when it was not the person (DEC-27). On the person's own acts it is left out; their identity leaves only as `by`. |
| `references` | `metadata.references`: explicit references to other subjects (DEC-17). |
| any other source field | `metadata.fields.<name>`. |

GitHub, Jira, Gmail, Calendar and Drive events carry a title that describes the change (see [`sources.md`](sources.md)). It is not listed in `fields`, and it never includes comment, review or mail text. Without `title`, `summary` or `subject`, the thread has no object title, and Signal titles a new thread with the event title. A source that sets no event title still uses `title` for the event and the thread; without it, Signal titles both with the type.

## State

`state` names the source's state fields that go in the event's `state.fields` (DEC-28). `observed_at` and the
source's version marker sit beside `fields`, not inside it. Each source lists its state fields as it lists its
fields. A state field not listed is not sent, and an item without a snapshot carries no `state`. The shape is in
[`events.md`](events.md#the-request-body).

## `beacon rules check`

Validates the file and reports, for the enabled sources:

- rules that can never match: a `source` that matches no enabled source, a `fields` condition on a field none of
  its sources has, or an earlier rule that matches every item this one could (for example `{own: true}` before
  `{own: true, type: commented}`, or `{}` before anything);
- a name in `fields` or `state` that none of the rule's sources has;
- as a warning, a rule with `own: true` for a source with no entry in `person.identities`: those own acts are sent
  without `by`, so Signal records them as Beacon's acts rather than the person's (DEC-27).

The cover check is conservative: it reports only what it can prove. It exits 1 on an error or an unreachable rule,
and 0 when there are only warnings. `beacon run` refuses to start on a rules error, and starts with unreachable rules
and warnings.
