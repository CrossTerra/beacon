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
| `person.handle` | The person Beacon serves. Asks name it in `to` (DEC-27). |
| `person.identities` | The person's own identity per source. An own act carries `by` with this identity (DEC-27). Signal must have it attached to the person's actor, and Beacon's key must have a `relay` prefix that covers it. |
| `rules` | The rules file. Its grammar is in [`rules.md`](rules.md). |
| `state` | The state directory: outbox, hold queue, dropped record, instance id. |
| `mode` | `dry-run` until the person turns sending on. `beacon run --dry-run` forces a dry run in either mode. |
| `lease.period` | The lease period (DEC-25). Default `5m`. |
| `poll_interval` | How often `beacon run` makes a pass. Default `1m`. |
| `sources` | Enabled sources. Phase 1 has one kind, `fixture`. |

## The Signal key

Signal's admin gives each person's Beacon its own integration actor and key (DEC-11): scopes `write` and `relay`,
with `--identity-prefix` patterns that cover the person's identities (`github:*`, `slack:*`, `email:*`). Beacon
needs no `read` scope in Phase 1.

`beacon init` reads the key from standard input, with echo off when it is a terminal, and stores it in the login
keychain. `beacon run` reads it from there when it sends. A dry run does not read it.

## Fixture source

A fixture is a JSON file of recorded items, used by tests and to try rules:

```json
{
  "source": "fixture",
  "fields": ["title", "body", "repository", "url", "label"],
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
      "due_at": null
    }
  ]
}
```

`fields` is the source's fixed field list; an item with any other field is refused. The item shape is described in
[`events.md`](events.md#items).
