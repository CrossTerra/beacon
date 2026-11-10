# Phase 4 — Slack

Status: built against fixtures; the live dry run against a real workspace is pending (the person installs their app
and stores the token after this pull request). What was built is described in [`docs/sources.md`](../docs/sources.md),
[`docs/commands.md`](../docs/commands.md) and [`docs/configuration.md`](../docs/configuration.md).

## Goal

The person's Slack as a source: direct messages, mentions, replies in threads they posted in, their own messages, and
pull request review requests in channels they name, shaped by DEC-9, DEC-10, DEC-17 to DEC-21 and DEC-23 like the
other sources. Read-only access with a user token the person provides, and a starter rules file that asks for direct
messages and mentions, records the person's own messages as links, and sends nothing else. At the end, Beacon can run
in dry-run against a real workspace and its report reads like that person's Slack unreads that need them.

What a follow-through agent learned reading Slack (bosun `knowledge/sources/slack.md`) shapes the collection:
`conversations.unreads` is slow with a user token and is not used; search and history are; an ask is answered by the
person's own later message in the same thread, or in the same direct message; one global cursor skips a quiet
conversation, so threads and channels keep their own watermark.

## Scope

### Auth

- **A user token the person provides (DEC-2).** The person creates a Slack app in their workspace (internal, not
  distributed), adds the user scopes below, installs it, and copies the user OAuth token (`xoxp-…`).
  `beacon source add slack` reads it with echo off, refuses anything that is not a user token, calls `auth.test`
  through the egress guard to learn the workspace and user ids, and stores it in the keychain as
  `slack:<workspace id>:<user id>`. The config gets `workspace` and `user`, never the token.
- **Read-only scopes, all user scopes:**

  | Scope | For |
  | --- | --- |
  | `search:read` | `search.messages`: direct messages, mentions, the person's own messages |
  | `channels:history` | `conversations.replies` and `conversations.history` in public channels |
  | `groups:history` | the same in private channels |
  | `im:history` | `conversations.replies` in direct messages |
  | `mpim:history` | `conversations.replies` in group direct messages |

  `auth.test` returns the token's scopes (`X-OAuth-Scopes`). A token missing one is refused by `source add` (nothing is
  stored; the command names the missing scopes) and fails a pass with `source_unauthorized`. A token that also holds
  other scopes (a write scope from an app the person reuses) is accepted; `source add` names them, and Beacon never
  calls a method outside its list.
- **Only the keychain package touches the token.** `keychain.SlackUserToken` checks the `xoxp-` prefix; the source sets
  it only as the bearer header.

### Host, methods, query keys

- **Host.** `slack.com` only (the Web API at `https://slack.com/api/`). Listed by `beacon egress` from the source's
  `Hosts`, and `source add slack` uses the same list.
- **Methods.** `auth.test`, `search.messages`, `conversations.replies`, `conversations.history`, all `GET`. Any other
  path is refused before it is sent (`source_scope`).
- **Query keys per method.** `search.messages`: `query`, `count`, `page`, `sort`, `sort_dir`; the query must be one of
  the three Beacon builds. `conversations.replies`: `channel`, `ts`, `oldest`, `limit`, `cursor`.
  `conversations.history`: `channel`, `oldest`, `limit`, `cursor`, and the channel must be one the config names.
  `auth.test`: none.
- **Rate limits.** Slack limits per method per workspace by tier. Beacon spaces its own calls to stay under them:
  `search.messages` (Tier 2, 20 a minute) at most one every 3 seconds, `conversations.replies` and
  `conversations.history` (Tier 3, 50 a minute for an internal app) at most one every 1.2 seconds. A `429` honours
  `Retry-After` like Phase 2 (a wait over two minutes ends the pass and the cursor stays); `"error":"ratelimited"`
  ends the pass the same way. A thread with a message in the last hour is read every pass, a quieter one every 15
  minutes, and a pass reads at most 40; the rest are read on later passes, longest unread first, each from its own
  watermark, so none is skipped.
- **Errors.** `invalid_auth`, `not_authed`, `token_revoked`, `token_expired`, `account_inactive`, `missing_scope`,
  `not_allowed_token_type`, `no_permission` are `source_unauthorized`. `channel_not_found`, `thread_not_found` and
  `not_in_channel` on one thread or channel skip it (a followed thread is dropped from the cursor). Any other error
  fails the pass and the cursor stays.

### Collection

- **Search** (`search.messages`, sorted newest first, 100 a page, at most 20 pages a query; past that the pass is
  marked cut):
  - `is:dm after:<date>`: one-to-one direct messages, including the person's own.
  - `<@USER> after:<date>`: mentions, in channels and group direct messages.
  - `from:<@USER> after:<date>`: the person's own messages. They are own acts, and they say which threads the person
    posted in.

  `after:` is a date. Beacon asks from two days before the window and keeps only messages whose `ts` is in it.
- **Followed threads** (`conversations.replies`): every thread the person posted in during the last 7 days, outside
  one-to-one direct messages (search already reads those). Each keeps its own watermark in the cursor: the newest
  reply read, the person's first and latest post, and when it was read. A thread is dropped 7 days after the person's
  latest post in it.
- **Configured channels** (`conversations.history`, top-level messages): `channels` in the config, each with its own
  watermark. Read for review requests and the person's own messages; mentions there are already in search.
- **Window.** A pass reads from 15 minutes before the cursor (search; history and replies use their watermarks).
  The first run reads the last 7 days (DEC-23). The cursor moves only after the runner decides every item, as every
  source.

### Items (DEC-18)

One item per message. When a message qualifies twice, the first type in this list wins:

| Item | When | Addressed | Own | Event title |
| --- | --- | --- | --- | --- |
| `own_message` | The person's message (the API's `user` is the token's user, and no `bot_id`) in a direct or group direct message, in a thread, or in a configured channel | no | yes | Sent, or Replied in a thread |
| `mentioned` | `<@USER>` in someone else's message | yes | no | Mentioned |
| `dm_received` | Someone else's message in a one-to-one direct message | yes | no | Direct message |
| `thread_reply` | Someone else's reply, after the person's first post, in a thread the person posted in | no | no | Thread reply |
| `review_request` | Someone else's top-level message in a configured channel with a pull request link and review wording (`review`, `look`, `approve`, `PTAL`, …) | no | no | Review requested |

Edits, reactions, joins and other message subtypes are not events. An own top-level message in a channel that is not
configured is not an item (the thread it starts is still followed).

- **Subject (DEC-10).** A channel message: `slack:<channel>:<thread ts>`, the thread root's `ts` (its own `ts` when
  it is not in a thread). A one-to-one or group direct message: `slack:dm:<person>:<channel>:<thread ts>`, the person
  being `person.handle` without `@`. Only a `C…` channel that search does not mark as a direct message is a channel;
  anything else is treated as personal, which can only hold more back.
- **Key.** `slack:<channel>:<ts>`; the version is the message `ts`.
- **Fields.** `channel` (id), `ts`, `thread_ts` (the root, when in a thread), `author` (user or bot id), `permalink`,
  `body` (the message text, set only when a rule lists `body`). No state fields: Slack has no object state Beacon reads.
- **Actor.** `slack:user:<id>`, or `slack:bot:<id>` for an app. **Identity.** `person.identities.<name>` is
  `slack:user:<user id>`; a mismatch with `auth.test`, as with the config's `workspace` and `user`, is
  `identity_mismatch`.
- **References (DEC-17).** A GitHub pull request URL in the text becomes `github:pr:<owner>/<repo>#<n>`; a Jira
  `https://<site>.atlassian.net/browse/KEY-1` becomes `jira:KEY-1`.
- **Text is read, not sent.** Slack returns a message's text with the message, whatever the scope, so Beacon reads it
  on the machine to find mentions, review wording and links. It leaves only as `body`, when a rule lists it.

### Answered (DEC-19, DEC-20)

An own message in a thread is on the thread's subject, so it closes the open asks there. A one-to-one direct message
has no thread for top-level messages: each is its own subject under DEC-10. An own top-level message in a one-to-one
direct message closes the open asks on its sibling subjects, those that differ from its own only in the last part
(the earlier top-level messages in that conversation). That is a new item flag, `close_siblings`, honoured only on
own acts and kept in the stored decision so a retry closes the same asks. A top-level message in a channel or a group
direct message closes nothing else: it may be about something else (bosun's "answered: likely").

### Personal subjects

`slack:dm:` is already a fixed personal stem. The Slack source reports it, so a send pass reads Signal's restrictions
and holds `slack:dm:` send items until `slack:dm:*` is restricted, exactly as Gmail. Channel subjects are not personal
and are not held. A config with a Slack source needs `person.handle` as an `@handle`.

### Starter rules

`examples/rules.yaml` gains: own messages `send` with `permalink` only (they close asks); bot messages `never`; direct
messages and mentions `send` with `ask: me` (priority high for direct messages, medium for mentions) and
`channel`, `permalink`, `author`, `actor`, `references`; thread replies and review requests fall to `never`. No rule
lists `body`.

## Out of scope

Slack Connect specifics, user display names (`users:read`), files and attachments, reactions, unreads, posting or
marking read, Enterprise Grid org-wide search, Socket Mode or the Events API, a live run in this pull request.

## Tests

- The source against a fake Slack served by `httptest`, covering every item type, own acts, addressed, the
  answered-by-sibling flag, pagination, `429` backoff, `ratelimited`, a refused token, a missing scope, an identity
  mismatch, a skipped thread, and a cursor that only moves after commit.
- Keys are stable across passes; a message seen by two reads yields one item.
- No request leaves `slack.com` and the four methods, and each method takes only its query keys.
- Pacing: calls to one method are spaced by its tier.
- `source add slack` stores the token under the workspace and user, prints no token, and refuses a bot token and a
  token missing a scope.
- The starter rules against the fixtures: direct messages and mentions are asks, own messages leave as links only,
  everything else is dropped, no body leaves, and `slack:dm:` items are held until `slack:dm:*` is restricted.

## Exit gate

All gates green. Against fixtures, the source produces the documented items, subjects, keys and fields. After this
pull request, the person stores a token and a live dry run (`beacon run --first`) completes, its report is checked by
the person, and nothing is sent. Sending direct messages waits for the Signal admin to restrict `slack:dm:*`.
