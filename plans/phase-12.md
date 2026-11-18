# Phase 12 — Slack Socket Mode beside polling

Status: planned. No code yet. Depends on phase 4 (the Slack source) and phase 11 (source loops, coverage).

## Goal

A Slack message that concerns the person reaches Signal within seconds, and a message in a quiet channel no longer
waits up to 15 minutes, without widening what Beacon may read or where it may connect. Polling stays: it is how Beacon
catches up after a disconnect, a sleep, or an event Slack did not deliver.

## Why

After phase 11, Slack is read on its own 15-second loop, but a pass is bound by the per-method spacing (about 40
seconds with many channels) and quiet channels are read every 15 minutes, at most 40 a pass. Polling cannot get much
faster without more calls than the tiers allow. Slack pushes events over a websocket the app opens (Socket Mode), so
the latency becomes Slack's delivery time, and polling can slow down.

## Scope

1. **An app-level token in the keychain.** `beacon source add slack --socket` reads an app-level token (`xapp-…`,
   scope `connections:write`) with echo off, refuses any other kind, checks it with `apps.connections.open`, and stores
   it as `slack:<workspace id>:<user id>:app` beside the user token. Only the keychain package reads it; it is set only
   as the Authorization header of that one call. The config gets `socket: true` on the Slack source, never the token.
2. **An outbound websocket.** `apps.connections.open` (POST, `https://slack.com/api/`) returns a one-time `wss://`
   URL on `wss-primary.slack.com` (and Slack's other `wss-*.slack.com` hosts). Beacon dials it outbound; nothing listens
   on the machine. The hosts are declared in the Slack source's `Hosts`, so `beacon egress` lists them and the guard
   allows them, and the URL's host must be one of them or the dial is refused. A websocket client needs a dependency
   or a small implementation over `net/http` (RFC 6455 framing for text, ping, pong and close): the pull request that
   adds it gives the reason, and the standard library is preferred.
3. **Events the person's token can see.** Socket Mode delivers the Events API's events. With user-scope event
   subscriptions (events on behalf of the user who installed the app, under the user token's read scopes):
   `message.im` and `message.mpim` (direct and group direct messages), `message.channels` and `message.groups`
   (public and private channel messages, which include thread replies with `thread_ts`). `app_mention` is a bot event
   and is not used: a mention of the person is a `message.*` whose text names them, found the same way polling finds
   it. Each event is
   acknowledged on the socket at once (`envelope_id`), before anything else, so Slack does not resend it.
4. **One path for items.** An event is turned into the same `message` the poll builds, and the same item code decides
   its type, subject, key and title, so a message delivered by both the socket and a poll has one key and is one event
   (DEC-9). A channel the source does not read (not named, and not a member channel with `member`) is dropped on
   arrival, as the scope check drops it today. Message text is read on the machine as it is today, and leaves only as
   `body` when a rule lists it.
5. **Into the pass.** Delivered messages wait in memory for the Slack loop. An event wakes the loop at once (phase
   11's wake channel) instead of waiting its interval, so a pass runs with the delivered messages and its normal
   reads; at most one such pass every few seconds. The cursor and watermarks move as today, after the items are
   decided, so a crash between delivery and the pass loses nothing the next poll does not read.
6. **Polling as catch-up.** While the socket is connected and healthy, the poll interval stretches (2 minutes,
   configurable) and quiet channels keep their 15 minutes: the poll finds what the socket missed, deletions, and edits
   that add a mention. When the socket is down, polling returns to its phase 11 interval.
7. **Reconnects.** Slack sends `disconnect` before it rotates a connection (`refresh_requested`, `warning`) and closes
   a connection after some hours: Beacon opens a new one before closing the old, as Slack recommends, and keeps no more
   than two. A dropped connection reconnects with backoff (1 s doubling to 5 minutes, with jitter); a refused app token
   (`invalid_auth`) stops trying and reports `source_unauthorized` until the token is replaced. A wake from sleep
   (phase 11) reconnects at once and runs a catch-up pass. Pings keep the connection; a missing pong reconnects.
8. **Coverage.** The Slack snapshot gains `socket` (`connected`, `reconnecting`, `refused`, `off`), when the socket
   last delivered, and how many reconnects in the last hour; the heartbeat stays tied to a pass that collected.

## What the Slack app needs (a person with rights to the app does this)

The person's Slack app, built for their own workspace (phase 4), gains:

- **Socket Mode** turned on (*Settings → Socket Mode*).
- An **app-level token** with `connections:write` (*Basic Information → App-Level Tokens*), copied once into
  `beacon source add slack --socket`.
- **Event Subscriptions** on, with these **user** events (*Subscribe to events on behalf of users*): `message.im`,
  `message.mpim`, `message.channels`, `message.groups`. They need the user scopes the app already has
  (`im:history`, `mpim:history`, `channels:history`, `groups:history`). No bot events, no bot token, no new user scope.
- **Reinstall** to the workspace so the subscriptions take effect. In a workspace where installs need an admin's
  approval, that approval is the admin's step; Beacon cannot do it.

Without these, the source keeps polling as in phase 11 and says once a pass that the socket is off and why.

## Trust model

- Credentials stay on the machine (DEC-2). The app-level token is a second Slack credential in the keychain, read only
  by the keychain package, never in the config, the log, the outbox or a coverage record. It can open a socket for the
  app; it cannot read or post anything. `beacon source add slack --socket` refuses a token that is not `xapp-`.
- Egress stays declared: `slack.com` for `apps.connections.open`, and the `wss-*.slack.com` hosts, in the source's
  `Hosts` and so in `beacon egress`. The guard refuses a socket URL on any other host. Nothing listens; the
  connection is outbound only.
- Nothing new leaves for Signal. A delivered message is an item like a polled one, through the rules and the
  projection. The socket delivers only what the user token could already read; Beacon drops messages from
  conversations its configuration does not read.
- Logs carry counts and fixed codes: `socket_connected`, `socket_closed` with Slack's reason code from a fixed list,
  `socket_events` with a count. Never an envelope, a URL (the URL carries a ticket), message text or an id.
- `docs/trust-model.md` gains the app token, the socket hosts, and a known limit: Slack decides what it delivers and
  when; the catch-up poll is what guarantees nothing is missed.

## Out of scope

Bot tokens, slash commands, interactivity, posting, a distributed app, and events other than messages (reactions,
channel joins, file events).

## Tests (planned)

- A fake Socket Mode server in `slacktest`: `apps.connections.open` returning a `ws://` URL on the fake, hello,
  events, `disconnect` with `refresh_requested`, a dropped connection, a refused token.
- Every event kind becomes the same item, with the same key, as the poll's message; a message delivered by both is one
  event; one in a conversation not read is dropped.
- Acknowledgement before processing; a duplicate delivery with the same `envelope_id` is one item.
- Reconnect with backoff, a new connection before the old one closes, a wake reconnecting at once, no more than two
  connections, a refused token stopping retries and reporting `source_unauthorized`.
- The egress guard refusing a socket URL on another host; the app token never in any output.
- The poll interval stretching while connected and returning when the socket is down.

## Exit gate

`scripts/gates.sh` passes. Against the fake, a delivered direct message is in the outbox within a second of delivery.
On the person's workspace, after the app setup above, Slack messages reach Signal at a median under 10 seconds over a
few days of the running agent, with the catch-up poll finding nothing the socket missed beyond deletions and edits.
