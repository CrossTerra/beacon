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
| DEC-10 | Subjects are the source's stable id (`github:pr:<owner>/<repo>#<n>`, `jira:<KEY>`, `gmail:thread:<id>`, `slack:<channel>:<thread ts>`), so other writers about the same thing land on the same thread. |
| DEC-11 | Beacon writes as one Signal integration actor per person, with `write` and `relay` scoped to the identity prefixes of its sources (`github:*`, `slack:*`, `mailto:*`). The person who caused a change is recorded with `by`. |
| DEC-12 | Beacon asks for attention only for its own person, and only when a rule says so (`ask: me`). Routing work to other people belongs to agents that read Signal. |
| DEC-13 | Rule proposals, not silent learning. When the person dismisses items, Beacon can propose a rule. A proposal changes nothing until it is accepted, and an accepted proposal is a diff to the rules file. |
| DEC-14 | No telemetry, update check or crash reporting. Egress is the source APIs, the Signal host, and a hosted classifier only if configured. |
| DEC-15 | Nothing is specific to one organization. The Signal URL, sources, identities and rules are configuration. |
| DEC-16 | Releases are reproducible from a tag, signed, notarized on macOS, and ship an SBOM. |

## Phases

| Phase | Plan | Status | Depends on |
| --- | --- | --- | --- |
| 1 | Trust core: config, keychain, rules engine, projection, outbox, Signal client, `explain`, `egress`, dry run | pending | — |
| 2 | First sources: GitHub and Jira | pending | 1 |
| 3 | Google: Gmail, Calendar, Drive (comments and mentions) | pending | 1 |
| 4 | Slack: direct messages, mentions, threads the person is in | pending | 1 |
| 5 | Review: the hold queue and rule proposals, in the terminal first | pending | 2 |
| 6 | Local classifier | pending | 5 |
| 7 | Release engineering: reproducible builds, signing, notarization, SBOM, install docs | pending | 2 |
| 8 | macOS menu-bar shell | pending | 5, 7 |

Each phase gets its own plan file with goal, scope, out of scope, design notes, tests and an exit gate before work
starts.

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

## Open questions

1. Threads: should Beacon open threads (`open`) for items it sends, or leave them unthreaded for a filer agent?
2. Versions per source: which marker defines "a change of state" for each source, so that edits and reactions do
   not each become an event?
3. Which local model and runtime for the classifier, and how its prompt and version are recorded with each
   decision.
4. Backfill: how far back the first run reads, and whether backfilled items default to `hold`.
5. Several machines: one Beacon per person, or one per machine sharing cursors through Signal.
