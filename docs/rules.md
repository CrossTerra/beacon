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
| `ask` | no | `me`: the event asks the person (DEC-21). Nobody else can be asked (DEC-12). |
| `priority` | no | `low`, `medium`, `high` or `urgent`, for the ask. Needs `ask`. |
| `close_by_others` | no | `true`: when this rule sends another person's act, it closes the person's open asks on the same subject (DEC-20). |

A `never` rule takes none of `fields`, `ask`, `priority` or `close_by_others`.

## Conditions

Every condition in `match` must hold.

| Key | Tests |
| --- | --- |
| `source` | The source name. |
| `type` | The item type, without the source (`review_requested`). |
| `actor` | The identity of who caused the change. |
| `own` | `true` or `false`: whether the person caused it. |
| `addressed` | `true` or `false`: whether the person was asked or mentioned. |
| `fields` | A mapping of field name to matcher, on the source's fields. An item without the field does not match. |

A matcher is a plain string (exact), or a mapping with exactly one of:

| Matcher | Matches |
| --- | --- |
| `{exact: "acme/app"}` | That value. |
| `{glob: "acme/*"}` | `*` is any run of characters (including `/`), `?` is one character. The whole value must match. |
| `{regex: "acme/(app\|api)"}` | An RE2 expression. The whole value must match: the expression is anchored at both ends. |

## Fields

`fields` names what leaves the machine. Anything not listed is not in the request body.

| Name | Goes to |
| --- | --- |
| `title` | The event title, and the thread title when the event opens one. Trimmed, and shortened to Signal's 300 characters. |
| `body` | The event body. Full bodies leave only when a rule lists `body` (DEC-6). |
| `actor` | `metadata.actor`: who caused the change, when it was not the person (DEC-27). |
| `references` | `metadata.references`: explicit references to other subjects (DEC-17). |
| any other source field | `metadata.fields.<name>`. |

Without `title`, Signal titles the event (and a new thread) with its type.

## `beacon rules check`

Validates the file and reports, for the enabled sources:

- rules that can never match: a `source` that matches no enabled source, a `fields` condition on a field none of
  its sources has, or an earlier rule that matches every item this one could (for example `{own: true}` before
  `{own: true, type: commented}`, or `{}` before anything);
- a name in `fields` that none of the rule's sources has.

The cover check is conservative: it reports only what it can prove. It exits 1 when it reports anything. `beacon
run` refuses to start on a rules error, and starts with unreachable rules.
