# Configuration

Beacon reads one YAML file. It holds no secrets: the Signal key is in the keychain. Unknown keys are errors.

The file is found at `--config PATH`, else `$BEACON_CONFIG`, else `config.yaml` in the user config directory
(`~/Library/Application Support/beacon/` on macOS). `beacon init` writes it.

```yaml
signal:
  url: https://signal.example.com    # https, or http to a loopback address
  keychain_account: signal           # optional; default signal:<host>:<port>
person:
  handle: "@winton"                  # your Signal actor: @handle or act_…
  identities:                        # your own identity in each source, keyed by source name
    github: github:user:winton
    gmail: email:winton@example.com  # email:, never mailto:
    calendar: email:winton@example.com
    drive: email:winton@example.com
    slack: slack:user:U0123ABCD
rules: rules.yaml                    # relative paths are relative to this file
state: state
mode: dry-run                        # dry-run or send
lease:
  period: 15m                        # optional; at least 1m
poll_interval: 1m                    # optional; at most half the lease period
seal:                                # optional: raise items to owner on this machine (DEC-33)
  owner:
    words: [salary, offer, "background check", {exact: PIP}]
    senders: [rippling.com, counsel@law.example]
    labels: [Confidential]
sources:
  - kind: fixture
    path: fixture.json
```

| Key | Meaning |
| --- | --- |
| `signal.url` | The Signal base URL, with no path. `http` is allowed only to `localhost` or a loopback address. |
| `signal.keychain_account` | The keychain account holding the Signal key, under the service `beacon`. |
| `person.handle` | The person Beacon serves. Asks name it in `to` (DEC-27). With a Google or Slack source it must be an `@handle`: personal subjects carry it without the `@` (DEC-10). |
| `person.identities` | The person's own identity per source. An own act carries `by` with this identity (DEC-27). Signal must have it attached to the person's actor, and Beacon's key must have a `relay` prefix that covers it. |
| `rules` | The rules file. Its grammar is in [`rules.md`](rules.md). |
| `state` | The state directory: outbox, hold queue, dropped record, instance id. |
| `mode` | `dry-run` until the person turns sending on. `beacon run --dry-run` forces a dry run in either mode. |
| `lease.period` | The lease period (DEC-25): how often the holder renews. It holds the lease for three periods in Signal's lease API (two with claim events), so another instance takes over within three periods of the holder stopping. Default `15m`. |
| `poll_interval` | How often `beacon run` makes a pass. Default `1m`. The launchd agent runs this loop. |
| `seal.owner` | Words, senders and labels that seal an item at `owner`. See [Seal](#seal). |
| `sources` | Enabled sources. Kinds: `fixture`, `github`, `jira`, `gmail`, `calendar`, `drive`, `slack`. See [Sources](#sources). |

## Seal

`seal.owner` raises an item to `owner` on the machine, after a rule decided `send` or `hold` (DEC-33,
[`rules.md`](rules.md#sealed-rules)). It never lowers a level, unseals, adds a field, or sends an item a rule dropped.

| Key | Matches |
| --- | --- |
| `words` | Whole words or phrases, case-insensitive (`salary` matches "Salary review", not "salaryman"; words in a phrase may be separated by any whitespace). `{exact: PIP}` is case-sensitive. Matched against mail subjects and bodies, calendar summaries and descriptions, Drive titles, bodies and meeting titles, and Slack text and thread roots. A body is matched only when the source read it (Google reads bodies only for a sealed rule that lists `body`). |
| `senders` | A lowercase address (`counsel@law.example`) or domain (`rippling.com`, which also covers `eu.rippling.com`). Matched against every from, to and cc in a Gmail thread, a calendar event's organiser and attendees, and a note's or transcript's meeting organiser and attendees. |
| `labels` | Gmail label names, compared without case, against every label on the thread. |

A blank word or label, or a sender that is not a lowercase address or domain, is an error. The lists never leave the
machine and are never logged; `beacon explain` shows which one raised an item.

## The Signal key

Signal's admin gives each person's Beacon its own integration actor and key (DEC-11): scopes `write` and `relay`,
with `--identity-prefix` patterns that cover the person's identities (`github:*`, `slack:*`, `email:*`). With a
Google or Slack source, the key also needs `read`: a send pass reads the restricted prefixes (`GET /api/restrictions`)
and sends nothing under `gmail:`, `calendar:`, `drive:` or `slack:dm:` until Signal's admin has restricted that prefix
([`sources.md`](sources.md#google)). Beacon reads nothing else with it. Without Google or Slack sources, no `read`
scope is needed.

`beacon init` reads the key from standard input, with echo off when it is a terminal, and stores it in the login
keychain. `beacon key set` replaces that key, in the same account, after `GET /api/me` accepts it and the actor is the
configured person or the agent the stored key already is (`--force` stores another actor). It reads from
standard input the same way and does not print the key. A refusal stores nothing. `beacon run` reads the key from
the keychain when it sends. A dry run does not read it.

## Sources

`beacon source add` writes the source into this file and stores the credential in the keychain. It rewrites the
file, so a comment in it is not kept. Details of what each source reads are in [`sources.md`](sources.md).

```yaml
sources:
  - kind: github
    repos: ["acme/app", "acme/*"]   # owner/name or owner/*
    projects: ["ENG"]               # optional; Jira keys in titles and branches
    host: api.github.com            # optional; an enterprise host uses /api/v3
  - kind: jira
    site: acme                      # acme.atlassian.net; a label, not a hostname
    projects: ["ENG", "OPS"]
  - kind: gmail                     # gmail, calendar and drive share the account's Google grant
    account: winton@example.com     # the Google account's primary address, lowercase
  - kind: calendar
    account: winton@example.com
  - kind: drive
    account: winton@example.com
  - kind: slack
    workspace: T0123ABCD            # the workspace id; beacon source add slack writes it
    user: U0123ABCD                 # the token's user id
    channels: ["C0123REVW"]         # optional; channel ids read for review requests and @here
```

`name` overrides the default, which is the kind. Two sources may not share a name. `person.identities` uses that
name: `github:user:<login>`, `jira:account:<account id>`, `email:<address>` for `gmail`, `calendar` and
`drive`, and `slack:user:<user id>` for `slack`.

A Google source takes `kind`, `name` and `account`, which is required. `beacon source add google --account` writes
them and stores the grant every Google source of that account shares: the person's OAuth client and the refresh
token, in the keychain as `google:<account>:client` and `google:<account>:refresh`. The source refuses a grant
whose signed-in account is another. The hosts and the API are fixed.

A Slack source takes `kind`, `name`, `workspace`, `user` and `channels`. `beacon source add slack` writes the
workspace and user ids `auth.test` reports for the token, and stores the token in the keychain as
`slack:<workspace>:<user>`. `channels` are channel ids (`C…`), not names; a direct message cannot be one. Their top
level is read for review requests, `@here`, `@channel` and `@everyone`, which count as mentions only there. A private
one is personal. The source refuses a token whose workspace or user is another. The host and the API are fixed.

A GitHub `projects` list, plus every Jira source's projects, limits which issue keys become `jira:<KEY>` references.

## Fixture source

A fixture is a JSON file of recorded items, used by tests and to try rules:

```json
{
  "source": "fixture",
  "fields": ["title", "body", "repository", "url", "label"],
  "state_fields": ["status", "head"],
  "items": [
    {
      "subject": "fixture:pr:acme/app#1",
      "key": "fixture:pr1:1",
      "type": "review_requested",
      "occurred_at": "2026-11-07T09:01:00Z",
      "actor": "fixture:user:dana",
      "own": false,
      "addressed": true,
      "fields": {"title": "Add retries", "repository": "acme/app"},
      "references": [],
      "due_at": null,
      "state": {"status": "open"},
      "observed_at": "2026-11-07T09:01:30Z",
      "version": "1"
    }
  ]
}
```

`fields` is the source's fixed field list and `state_fields` its state field list (DEC-28); an item with any other
field or state field is refused. An item may set `event_title`, which is not a source field. Every service source sets
it to describe the change; a fixture that omits it keeps using `fields.title` as the event title.
`observed_at` and `version` are reserved and cannot be state fields. An item with
`state` needs `observed_at` and `version`, and `version` must be the last part of its key. The file holds one JSON
value. The item shape is described in
[`events.md`](events.md#items).
