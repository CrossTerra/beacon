# Phase 4 — Slack

Status: built against fixtures; the live dry run against a real workspace is pending (the person installs their app
and stores the token after this pull request). What was built is described in [`docs/sources.md`](../docs/sources.md),
[`docs/commands.md`](../docs/commands.md) and [`docs/configuration.md`](../docs/configuration.md).

Revised after an independent review of #12: a bounded `close_siblings_before`, private channels and group direct
messages as personal, group direct message history, watermarks that never skip (a new thread, a history read cut at
its limit), user group and `@here` mentions, threads followed on a mention, edits that add a mention, and deletions.

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
  | `channels:history`, `groups:history` | `conversations.replies` and `conversations.history` in public and private channels |
  | `im:history`, `mpim:history` | the same in direct and group direct messages |
  | `channels:read`, `groups:read` | `conversations.info`: whether a configured channel is private |
  | `mpim:read` | `users.conversations`: the group direct messages the person is in |
  | `usergroups:read` | `usergroups.list`: the user groups the person is in |

  `auth.test` returns the token's scopes (`X-OAuth-Scopes`). A token missing one is refused by `source add` (nothing is
  stored; the command names the missing scopes) and fails a pass with `source_unauthorized`. A token that also holds
  other scopes (a write scope from an app the person reuses) is accepted; `source add` names them, and Beacon never
  calls a method outside its list.
- **Only the keychain package touches the token.** `keychain.SlackUserToken` checks the `xoxp-` prefix; the source sets
  it only as the bearer header.

### Host, methods, query keys

- **Host.** `slack.com` only (the Web API at `https://slack.com/api/`). Listed by `beacon egress` from the source's
  `Hosts`, and `source add slack` uses the same list.
- **Methods.** `auth.test`, `search.messages`, `conversations.replies`, `conversations.history`,
  `conversations.info`, `users.conversations`, `usergroups.list`, all `GET`. Any other path is refused before it is
  sent (`source_scope`).
- **Query keys per method.** `search.messages`: `query`, `count`, `page`, `sort`, `sort_dir`; the query must be one of
  the three Beacon builds. `conversations.replies`: `channel`, `ts`, `oldest`, `inclusive`, `limit`, `cursor`.
  `conversations.history`: `channel`, `oldest`, `latest`, `limit`, `cursor`, for a configured channel or a group
  direct message the person is in. `conversations.info`: `channel`, configured only. `users.conversations`: `types`
  (`mpim`), `exclude_archived`, `limit`, `cursor`. `usergroups.list`: `include_users`. `auth.test`: none.
- **Rate limits.** Slack limits per method per workspace by tier. Beacon spaces its own calls to stay under them:
  `search.messages` and `usergroups.list` (Tier 2, 20 a minute) at most one every 3 seconds, the Tier 3 methods (50 a
  minute for an internal app) at most one every 1.2 seconds. A `429` honours
  `Retry-After` like Phase 2 (a wait over two minutes ends the pass and the cursor stays); `"error":"ratelimited"`
  ends the pass the same way. A thread with a message in the last hour is read every pass, a quieter one every 15
  minutes, and a pass reads at most 40; the rest are read on later passes, longest unread first, each from its own
  watermark, so none is skipped.
- **Errors.** `invalid_auth`, `not_authed`, `token_revoked`, `token_expired`, `account_inactive`, `missing_scope`,
  `not_allowed_token_type`, `no_permission` are `source_unauthorized`. `channel_not_found`, `thread_not_found` and
  `not_in_channel` on one thread or channel skip it (a followed thread is dropped from the cursor). Any other error
  fails the pass and the cursor stays.

### Collection

- **Search** (`search.messages`, newest first, 100 a page, at most 20 pages a query; past that the pass is marked cut):
  `is:dm` and `from:<@USER>` over the window; `<@USER>` over the last 7 days every pass, keeping one older than the
  window only when the cursor has not seen it (an edit added the mention, or search indexed it late). `after:` is a
  date; Beacon asks from two days earlier and keeps only messages in range.
- **Configured channels** (`conversations.info`, then `conversations.history` from the channel's watermark): private
  or public, then top-level messages.
- **Group direct messages** (`users.conversations` with `types=mpim`, then `conversations.history` from each one's
  watermark, at most 20 a pass).
- **Followed threads** (`conversations.replies`): a thread the person posted in or was mentioned in during the last 7
  days, outside one-to-one direct messages. A thread followed for the first time starts at that pass's window, so a
  reply is not lost while it waits for a later pass.
- **Deletions** (`conversations.replies` with `inclusive` on the message's own `ts`): each message that addressed the
  person, at most every 15 minutes, 20 a pass, for 7 days.
- **No skips.** Threads and group direct messages with a message in the last hour are read every pass, quieter ones
  every 15 minutes, longest unread first. History is newest first: a read that stops at its page limit keeps its
  watermark and records the gap below what it read; the next pass reads the gap before anything newer.
- **Window.** A pass reads from 15 minutes before the cursor. The first run reads the last 7 days (DEC-23). The cursor
  moves only after the runner decides every item.

### Items (DEC-18)

One item per message. When a message qualifies twice, the first type in this list wins:

| Item | When | Addressed | Own | Event title |
| --- | --- | --- | --- | --- |
| `own_message` | The person's message (the API's `user` is the token's user, and no `bot_id`) in a direct or group direct message, a private channel, a thread, or a configured channel | no | yes | Sent, or Replied in a thread |
| `mentioned` | `<@USER>`, a user group the person is in, or `@here`/`@channel`/`@everyone` in a configured channel, in someone else's message | yes | no | Mentioned |
| `dm_received` | Someone else's message in a one-to-one or group direct message | yes | no | Direct message |
| `thread_reply` | Someone else's reply in a followed thread, after the person's first post or mention there | no | no | Thread reply |
| `review_request` | Someone else's top-level message in a configured channel with a pull request link and review wording | no | no | Review requested |
| `message_deleted` | A message that was `mentioned` or `dm_received` is gone; on its subject, so a `close_by_others` rule closes the ask | no | no | Deleted |

- **Subject (DEC-10).** A public channel message: `slack:<channel>:<thread ts>`. A one-to-one or group direct
  message, or a private channel: `slack:dm:<person>:<channel>:<thread ts>`. Private channels share the direct-message
  stem, so one restriction keeps them private. A channel is public only when a read says `is_private: false`.
- **Key.** `slack:<channel>:<ts>`; the version is the message `ts`. A deletion is `slack:<channel>:<ts>:deleted`.
- **Fields.** `channel`, `channel_type` (`im`, `mpim`, `private`, `channel`), `ts`, `thread_ts`, `author`,
  `permalink`, `body` (only when a rule lists it). No state fields.
- **Actor and identity.** `slack:user:<id>` or `slack:bot:<id>`; `person.identities.<name>` is `slack:user:<id>`.
- **References (DEC-17)** from GitHub pull request and Jira links in the text.
- **Text is read, not sent**, except as `body` when a rule lists it.

### Answered (DEC-19, DEC-20)

An own message in a thread closes the open asks on the thread's subject. An own top-level message in a one-to-one
direct message sets `close_siblings_before` to its `ts`: it also closes an open ask on a sibling subject (same
conversation) that opened that subject (top-level, not a question inside a thread there) with a `ts` before it. The
bound is kept in the stored decision, so a retry closes the same asks. Top-level messages in channels and group direct
messages close nothing else. Someone else's answer closes an ask only through a `close_by_others` rule.

### Personal subjects

`slack:dm:` is already a fixed personal stem; the source reports it, so a send pass holds direct messages, group
direct messages and private channels until `slack:dm:*` is restricted. Public channel subjects are not held. A config
with a Slack source needs `person.handle` as an `@handle`.

### Starter rules

Own messages `send` with `permalink` only; bots `never`; deletions `send` with `close_by_others`; one-to-one direct
messages `send`, `ask: me`, high; group direct messages `send`, `ask: me`, medium; mentions `send`, `ask: me`, medium,
without `references`; thread replies and review requests fall to `never`. No rule lists `body`.

## Out of scope

Slack Connect specifics, user display names (`users:read`), files and attachments, reactions, unreads, posting or
marking read, edits seen only through history or replies, Enterprise Grid org-wide search, Socket Mode or the Events API, a live run in this pull request.

## Tests

- The source against a fake Slack served by `httptest`, covering every item type, own acts, addressed, the bounded
  sibling close (an ask posted after the reply, a thread question), private channels and group direct messages,
  user group and `@here` mentions, an edit that adds a mention, a deletion, a deferred new thread, a history gap,
  pagination, `429` backoff, `ratelimited`, a refused token, a missing scope, an identity mismatch, a skipped thread,
  and a cursor that only moves after commit.
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
