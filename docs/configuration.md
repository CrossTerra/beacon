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
rules: rules.yaml                    # relative paths are relative to this file
state: state
mode: dry-run                        # dry-run or send
lease:
  period: 5m                         # optional; at least 1m
poll_interval: 1m                    # optional; at most half the lease period
sources:
  - kind: fixture
    path: fixture.json
```

| Key | Meaning |
| --- | --- |
| `signal.url` | The Signal base URL, with no path. `http` is allowed only to `localhost` or a loopback address. |
| `signal.keychain_account` | The keychain account holding the Signal key, under the service `beacon`. |
| `person.handle` | The person Beacon serves. Asks name it in `to` (DEC-27). With a Google source it must be an `@handle`: personal subjects carry it without the `@` (DEC-10). |
| `person.identities` | The person's own identity per source. An own act carries `by` with this identity (DEC-27). Signal must have it attached to the person's actor, and Beacon's key must have a `relay` prefix that covers it. |
| `rules` | The rules file. Its grammar is in [`rules.md`](rules.md). |
| `state` | The state directory: outbox, hold queue, dropped record, instance id. |
| `mode` | `dry-run` until the person turns sending on. `beacon run --dry-run` forces a dry run in either mode. |
| `lease.period` | The lease period (DEC-25). Default `5m`. |
| `poll_interval` | How often `beacon run` makes a pass. Default `1m`. |
| `sources` | Enabled sources. Kinds: `fixture`, `github`, `jira`, `gmail`, `calendar`, `drive`. See [Sources](#sources). |

## The Signal key

Signal's admin gives each person's Beacon its own integration actor and key (DEC-11): scopes `write` and `relay`,
with `--identity-prefix` patterns that cover the person's identities (`github:*`, `slack:*`, `email:*`). Beacon
needs no `read` scope in Phase 1.

`beacon init` reads the key from standard input, with echo off when it is a terminal, and stores it in the login
keychain. `beacon run` reads it from there when it sends. A dry run does not read it.

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
  - kind: gmail                     # gmail, calendar and drive share the Google grant
  - kind: calendar
  - kind: drive
```

`name` overrides the default, which is the kind. Two sources may not share a name. `person.identities` uses that
name: `github:user:<login>`, `jira:account:<account id>`, and `email:<address>` for `gmail`, `calendar` and
`drive`.

A Google source takes only `kind` and `name`. `beacon source add google` writes them and stores the grant, which
every Google source in the file shares: the person's OAuth client and the refresh token, in the keychain as
`google:client` and `google:refresh`. The hosts and the API are fixed.

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
