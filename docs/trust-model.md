# Trust model

Beacon sits between accounts that belong to one person and a store that many people and agents read. This file
says what Beacon protects, from whom, and how. Code that changes any of these properties changes this file in the
same pull request.

`beacon serve` owns the approval journal, the same-user proposal socket, and the review pages on loopback
([approvals](approvals.md)). Presence and verification on your passkey bind the exact approval and release the
write-vault key. Configured providers get only their own requests, and Signal gets ordered journal records with
the approval app's key. `beacon egress` lists the hosts. The [supervisor](supervision.md) starts the collector as
the same user; its pass records carry names, times and fixed codes.

An approved `agent.run` uses the configured checkout, executable and model. A run inherits no service credentials,
but can use the person's files, OpenCode's own sign-in and its network destinations. `beacon egress` lists only
Beacon's hosts. OpenCode's `--auto` uses permissions configured for the checkout; the run is not a sandbox.

## What crosses the boundary

| Leaves the machine | Never leaves the machine |
| --- | --- |
| Events a rule sent: the projected fields and state fields, plus an event title that describes the change | Source credentials (OAuth clients, refresh and access tokens, API tokens) |
| A sealed event's `sealed` part (DEC-30): in plaintext over TLS, like any write; Signal encrypts it at rest with KMS and opens it only for the callers its level allows. Beacon holds no keys. Its envelope (subject, type, key, the ask, `unread` and `starred`, the fields a rule leaves `plain`, the source's opaque `native` id) is plaintext in Signal | `seal.owner`'s words, senders and labels, what matched them, and the subject floor (`seal-floor.json`: a level per subject hash) |
| The Signal key's bearer header, to the configured Signal host | Items the rules dropped or that are waiting for review |
| `beacon key set`: `GET /api/me`, which sends the new key and nothing else, and only stores it if Signal accepts it | |
| Requests to the source APIs, made with your credentials, including on a dry run, and to Google's token endpoint | The rules file, the review queue, the outbox (the stored decision keeps listed fields and the event title, not an unlisted body), the cursors, the seed file, classifier input and output |
| Slack approvals: the Slack user token's bearer header and the conversation and thread ids go only to Slack. Only after approval does the frozen draft go to Slack, with unfurls off and a random operation UUID as message metadata and `client_msg_id`; apps that read the message's metadata can see the UUID. Signal gets the journal's records: the native message id is plain; the draft, reason and approval details are sealed by Signal | The thread root's preview and the app's operation UUID are never sent to Signal |
| The lease: your Signal handle, a random instance id and the lease period, as a lease in Signal's lease API (not an event) or, where Signal does not offer one, as a claim event | Rule names, and why an item matched |
| With a Google or Slack source, a read of Signal's restricted prefixes (`GET /api/restrictions`), which sends nothing but the key | The Slack name cache (display names and channel names by id) and the record of threads checked for a retitle |
| With a rule that lists Slack's `title`, or a Google thread Beacon opened with a title that is now sealed: reads of Beacon's own events and their threads (`GET /api/events/{id}`, `GET /api/threads/{id}`), and a new title for a thread Beacon opened (`thread.titled`) | |
| Your own identity in a source, as `by`, on your own acts | Who else caused a change, unless a rule lists `actor` |
| Each source's coverage, from the running agent while it holds the lease (DEC-42): a heartbeat lease per source (your handle, the source's name, the instance id, a time to live) and, when it changes, a `beacon.coverage` snapshot on `beacon:coverage:<handle>:<source>`: status, fixed reason codes, times, counts and flags, never an item, a key, an id, a name or an error's text | `coverage.json` beyond what a snapshot carries: how long each collect took, and when the machine slept |

## Properties

1. **Default deny.** An item with no matching `send` or `hold` rule is dropped. Adding a source sends nothing
   until a rule names it. A rule condition with no value is an error, not "match anything".
2. **Rules decide; the classifier only narrows.** The classifier sees items the rules already allowed. It can
   hold or drop them. It cannot send an item the rules denied, and it cannot add fields. (No classifier exists
   yet; it arrives in Phase 6.)
3. **Projection is explicit.** Each source has a fixed list of fields, and an item with any other field is refused.
   A rule lists the fields it sends. Message bodies, attachments and file contents leave only when a rule lists
   them. Attachments and file contents are out of scope for the first release. A state snapshot carries only the
   state fields a rule lists (DEC-28). Who caused a change leaves only when a rule lists `actor`; your own identity
   leaves as `by` on your own acts (DEC-27). An event's title is a description of that change, not a source field
   and not the object's title. It leaves with the event so a thread's events do not all read as the pull request,
   issue, mail or file title. It is built only from the change the source already recorded (who was requested when
   it is not you, the review state, a short commit sha, the check conclusion, a pull request's state (merged or closed), the assignee's display name when it is not you, the status names, the due
   date; for Google, the item type, the person's calendar response and whether the event is cancelled; for Slack, the item type) and it never includes
   comment, review, mail or message text, a mail subject, an event summary, a file name or a channel name. The object's title leaves only when a rule lists
   `title`, `summary` or `subject`, as the thread title. Slack's `title` is the conversation's: who and where, built
   only from display names, the channel's name and, for a direct message's day, that day ("DM with Dana Scully · Thu
   12 Nov", "#eng: Bob Ross mentioned you"), never
   message text. Display names (`channel_name`, `author_name`) are fields like any other and leave only when a rule
   lists them. Slack names are read only when a rule lists one of those three fields. A thread Beacon opened before
   its rule listed `title` is renamed (`thread.titled`) only when the current rule for the stored item still sends it
   and lists `title`, Signal shows that Beacon's own event opened that thread, and nobody renamed or merged it since.
   A thread title is plain, so a stored event that was sealed, a sealed rule that does not leave `title` plain, or a
   subject Beacon has sent sealed is never retitled from names. The reverse holds for a Google thread Beacon opened with
   a plain object title that is now sealed (its current rule seals it, or Beacon has since sent the subject with its
   title sealed): it is renamed, under the same checks, to a fixed neutral title that names only the kind of thing
   ("Mail from a colleague", "Meeting", "Document"), never its subject, summary or file name.
   A thread someone else opened or renamed is not touched. A Google source does not even read a mail body, an event
   description, comment text, meeting notes or a transcript unless a sealed rule that can match it lists `body`.
   Slack returns a message's text with the message, so the Slack source reads it on the machine to find mentions,
   review wording and links, and to match `seal.owner`; it leaves only as `body`, when a rule lists it. **A body from a
   personal subject leaves only sealed (DEC-32)**: `beacon rules check` refuses a rule that would send one plain, and
   at run time such an item is held as `body_needs_seal`. On a sealed event the envelope is fixed in code (DEC-31):
   the event title, the thread title when its field is not plain (`sealed.open.title`, Signal DEC-281), the body, who
   caused it (`actor`), and every field, state field and `references` the rule does
   not leave `plain` go in `sealed` (references are subjects; left plain, Signal relates threads through them, Signal
   DEC-284); `metadata.native` is the source's opaque id, never a Message-ID, message text, or anything
   with `@`. An observation of an act carries that id namespaced; sealing does not cover it (DEC-29). The local check (`seal.owner`, DEC-33), a meeting's level (DEC-35) and the subject floor (DEC-34) can
   only raise a level: they never lower or unseal one, add a field, or send an item a rule dropped. The log counts
   raises and never says why.
4. **Local classifier by default.** The classifier runs on the machine. Using a hosted model is an explicit
   setting, shown by `beacon egress`, because it sends item content to that model's vendor before the filter
   decides.
5. **The outbox is the record.** Every request body, lease claims included, is appended to the outbox and synced
   to disk before it is sent. A pending retry resends those exact bytes, which is what Signal requires of a key it
   already holds. A body Signal accepted (`sent`, `replayed`) is never resent or rewritten. The only retryable
   `409` is `conflict` with `details.field` equal to `/by/identity`: the identity moved between Signal resolving
   `by` and writing the event, so Signal asks for the same request again. Beacon retries the stored bytes with
   backoff and leaves the body pending for the next pass if attempts run out. A recorded `409` is final and never
   decided again. A `400` with no stored item is not a decision: `beacon run` may decide that key
   again when a source yields it, append a new attempt, and send it. The refused lines stay in the file. Any other
   refusal is not sent again by `beacon run`. `beacon outbox retry --rejected` runs the current rules on the stored
   item. It sends only when they still send, and only fields both the stored decision and the current rule list. If
   they hold or drop it, the entry is recorded superseded and not sent. A rule above that match that could still
   apply, and that needs a field or actor the stored item does not have, cannot be proved, so the entry is
   superseded as never and not sent. A condition the item does have that already fails that rule does not block
   the send. The command logs counts only. The stored
   decision keeps listed fields and the event title. An unlisted field, including a body, is not written and does not leave.
6. **Fixed egress.** Beacon connects only to the source APIs (and Google's token endpoint), the configured Signal
   host and, if configured, a hosted classifier. `beacon egress` prints the list, and every HTTP request goes through
   a transport that refuses any other host before connecting. Proxy settings from the environment are ignored and
   redirects are not followed. GitHub checks every request, including pagination, against its configured scheme,
   hostname and port before attaching the token. Each Google source also refuses any URL outside the API paths it uses, because
   `www.googleapis.com` serves other APIs too. The Slack source refuses any method but `auth.test`,
   `search.messages`, `conversations.replies`, `conversations.history`, `conversations.info`, `conversations.members`,
   `users.info`, `users.conversations` (group direct messages, and with `channels: member` the public and private
   channels the person is in) and `usergroups.list`, any query key those calls do not use, a search that is not one of
   its three, the history of a channel the config does not name (with `member`, one the person is not a member of) or
   a group direct message the person is not in, and `conversations.info` or `conversations.members` on a conversation
   the pass neither reads nor made an item from, so its token cannot post, mark read or list the workspace's people or
   the channels the person is not in through Beacon. `beacon source add google` listens on `127.0.0.1` for one OAuth
   redirect; the person's browser, not Beacon, opens Google's consent page. There is no telemetry, no update check
   and no crash reporting.
7. **Unattended credentials are bound in KDBX.** Read tokens, Google OAuth clients and refresh tokens, and the
   collector and app Signal keys are in `unattended.kdbx`, not plaintext configuration. The key is a private
   file on macOS, and on Linux is in Secret Service when setup finds it, otherwise a private file. An existing
   vault never receives a replacement key. Same-user processes can read this vault. KeePassXC edits are picked
   up without a restart; writes sync a candidate first, then check the prior file hash immediately before rename.
   That does not close the last check-to-rename race against KeePassXC. Partial setup fails rather than replacing
   the key or silently using an incomplete vault.
   Credentials redact in output and are not stored in the journal, passed to an agent, or logged. The only
   OS Keychain access is the person's explicit one-time collector import; it copies only configured collector
   items, without updating or deleting them. GitHub and Slack imports refuse grants not reported to be read-only.
   GitHub fine-grained tokens without a reported grant must be newly issued read-only tokens entered directly by
   the operator, who checks those permissions at issuance. Google tokens are exchanged using PKCE and checked `state`; access
   tokens are kept only in memory. Signal must use HTTPS except for a loopback test endpoint. No daemon runs as root.
8. **Logs carry no content.** Log lines have fixed event names and carry counts, source names, outcomes, statuses,
   fixed Signal error codes and fixed reason codes. An unrecognized Signal error code becomes `other`. The
   supervised collector writes pass summaries to `collector.log` in the service state directory (`~/Library/Application Support/beacon/collector.log` on macOS), rotated by size with
   one previous file kept. The retired LaunchAgent's logs are readable for migration. Logs exclude item content, rule matches, keys,
   bodies, tokens and error text. `beacon run` prints a fixed message for each reason.
9. **Personal subjects stay personal.** Subjects under `gmail:`, `calendar:`, `drive:` and `slack:dm:` carry the
   person's handle (`gmail:<handle>:…`, DEC-10). `slack:dm:` holds Slack direct messages, group direct messages and
   private channels; a Slack channel is public only when Slack says so (`is_private: false`), and any other is
   treated as private. Bodies from these subjects leave only sealed (property 3). Signal seals only a person's own
   subjects, so an item on a shared subject that would be sealed (a public channel message matching `seal.owner`) is
   held as `sensitive_shared`, not sent plain. Those stems are fixed, not taken from the enabled sources, so a body
   already in the outbox stays personal after its source is removed. A send pass reads Signal's restricted prefixes
   when a personal source is enabled or the outbox holds a pending or rejected event under one of those stems, and
   again immediately before pending bodies are sent, so a restriction lifted during the pass is honoured. It sends
   such a subject only when the longest restricted stem it is under is its own (`gmail:*`), which is what makes Signal
   keep it to its owner. Otherwise the source's send items are held locally; if the restrictions cannot be read, they
   are not recorded and the cursor stays. A pending body, or a refusal `beacon outbox retry --rejected` would resend,
   waits the same way, including after the source is gone. Your own acts are recorded from the source's own record only: a Gmail message is
   yours only with the `SENT` label, never because its From header names you, and a calendar change is yours only when
   it is your response or an event your account created, and a Slack message is yours only when Slack's `user` is the
   token's user and it has no `bot_id`. A Slack direct or group direct message has one subject per conversation per
   day (DEC-38), so your own message there closes only the asks it answers on that day's subject: a top-level message,
   the top-level messages before it; a reply in a thread, that thread. Never a question inside another thread, a
   message after it, or an ask in another day or conversation.
10. **One sender per person.** Only the instance holding the lease sends (DEC-25), and no write outlasts the lease.
    Another instance collects and keeps its bodies pending.
11. **Reproducible, not signed.** The [installer](install.md) builds from the audited `git archive`, refuses
    tracked edits and checks the commit in the executable. User service managers start it at login. The exact
    commit and the person's supply chain are trusted, not a code signature. Cutover archives a fully drained
    Swift SQLite journal, never replays it, and starts a strictly newer journal with Signal sequence 1. A retry
    rechecks both the archive checksum and the closed source, including committed WAL, before handing off.
12. **Services end with the service.** The [Go reaper](supervision.md) cancels restarts and stops each process
    group and tracked descendant even after a service crash. It retains the instance lock through cleanup;
    an unconfirmed sweep must be recovered before accepting proposals. A copied session cannot sweep the
    original jobs. Linux's user unit adds a cgroup boundary. Supervision is not isolation.
13. **The socket grants no approval authority.** Kernel user IDs and private paths admit only the same user;
    process labels are unverified except for a supervised run's attribution. No socket operation approves,
    lists history, or quits. Approval requires a fresh PRF-capable passkey assertion over the frozen request.
    Browser sessions can read and decline, not approve. Loopback, Host, Tailscale login and form checks do not
    make a loopback header evidence of approval. The journal is private plaintext. A restart expires approvals
    without a start and never redispatches a start without a result.
14. **Provider writes use a different vault.** The encrypted write credentials are bound to HTTPS origin,
    account and, for Slack, workspace. The data key comes from a verified passkey's PRF and is not passed to
    children. A token shared with the unattended vault is refused. Clients disallow redirects and proxy
    overrides; fresh provider checks run after the durable start, and lost replies permit only read-back.
15. **Slack approval records use trusted visibility.** Beacon verifies the Slack user, workspace and visibility before
     writing `proposed`. Only a conversation Slack confirms as public gets a conversation subject; private channels,
     DMs and group DMs use the person's own Signal subject. Agent-supplied context cannot route records. The subject
     stays fixed because Signal requires exact equality throughout the proposal. Beacon checks visibility, membership
     and the thread root before it enables Approve and again at the write. Beacon sends the exact frozen text.
     An unclear or lost receipt only permits read-back, never another post. Without one message carrying the
     operation UUID (as readable `beacon_post` metadata or `client_msg_id`), with the exact text (allowing
     Slack's equivalent `&amp;` escape), approved user and destination in the bounded time window, the outcome
     stays unknown. Text alone never confirms delivery.
     Intake lookups, window preflights and read-back each have their own read budget and their own 429 deadlines,
     so a proposer cannot spend the window's reads. The window's names (the workspace from `auth.test`, a channel's
     name, a direct message's other person, a group direct message's members) are display. They are resolved at
     intake, cached for ten minutes, and kept in the local journal. They are not a record field, not part of a hash,
     and not sent to Signal. A failed name lookup is shown as `unknown channel <id>` and does not change visibility,
     membership, or whether the proposal can be approved. The raw ids stay under Show exact parameters. A missing
     Slack scope is a scope name (`chat:write`), recorded in the result and shown in the window; the token and
     Slack's `provided` list are not. Saving the token checks `X-OAuth-Scopes` against the scopes Beacon needs.
16. **Enrollment does not grant a usable write key to an edited credential list.** The HTTP listeners stay on
     loopback. They enforce the canonical Host and, behind a private Tailscale proxy, the configured login header.
     Setup verifies that the canonical origin serves only Beacon, with no other Serve handlers on that origin.
     A browser needs a memory-only session to see proposals. The login header does not identify local processes;
     cookies can be received by another port on the same host. `beacon setup` prints a one-time passkey code and
     refuses while the service is running. `/passkeys/new` registers one passkey from that code. It stores
    a public key and a data key wrapped under a PRF output, not the output, plus a key-check sealed under
     that data key. The live journal refuses a second active enrollment and records the hash of the bytes
     sealed by the first. A later assertion can record an approval only
     for a resolved binding, and only if the PRF output unwraps that key and it opens the same vault bytes
     the service verified against its live enrollment hash. A swapped file is a journalled reset and approves nothing. This holds even
     for `agent.run`, which has no provider write credential and never joins a batch. Write
     credentials are sealed under the real data key, so a replacement key cannot open them. An unseal failure
     prevents approval and dispatch. A separate settings assertion is required to save tokens, and Beacon
      verifies the configured identity for both. The unattended KDBX vault holds a distinct read token for
      intake, preflight and recovery. GitHub's binding includes the configured HTTPS origin and user ID.
      Slack's is the fixed `https://slack.com` origin, user ID and workspace; the unattended token can hold
      only the allowed read scopes. Slack placement and the operation UUID stay on the machine, except that
      the UUID goes to Slack with the post. Only `github.merge` and `slack.post` can share an assertion (up to
      ten, in display order), and every write token in the batch must unseal before any approval is recorded.
      Each write requires a durable start, fresh provider checks with that token, and a one-shot dispatch claim.
      If the service stops or the request is cancelled before that claim, and the journal can still record, the
      action fails as `stopped` rather than becoming an unknown write. A journal that had already failed is not
      rewritten. A failed append of that stop record fails shutdown. A lost reply permits only read-back with
      the unattended token. A merged pull request with a different actor, head, or base fails as
      `merged_otherwise`. Incomplete attribution stays unknown, and a pull request already seen merged is
      never later recorded as not merged. Automatic merge read-back backs off after the settlement window,
       up to 15 minutes, and is checked every 15 seconds so one stuck read does not spend the recovery budget. Without provider config there is no resolver
       or outbound provider request; otherwise `beacon egress --state-dir DIR` lists the allowed hosts.
       A run can launch only in an explicitly configured checkout. A descriptor stays pinned from freeze and
        the bootstrap enters it, then refuses exec if the approved directory or HEAD has changed. The reaper
        registers its group before a durable one-shot permit releases the child. The exact approved prompt is
        staged on an anonymous descriptor before release. No service environment, credential or unrelated
        descriptor reaches the child. OpenCode can use the person's
       own configuration and sign-in; it is not sandboxed. Run output stays in private files. With delivery
       configured, the ordered outbox sends the capped report or failure detail sealed as `owner+agents`, marked
       as untrusted runner text. It uses a separate, origin-bound app key from the unattended vault, never a
       provider write token or a credential from the runner.
      See [Go approvals](go-approvals.md).

## Known limits

- **An agent run is not a sandbox (13.00-D12, D22).** It runs as the person, with `--auto` and their files, and can
  do anything they can. Beacon supplies a fixed environment, not their login environment.
  Approving it approves a launch in one checkout, not what the agent
  then does; a checkout's own `bin/run` guardrails are not applied. A descendant that leaves its process group in the
  ways [supervision](supervision.md) describes can outlive Stop on macOS. The run's output and error files stay
  under `runs/`, readable by the person. They are not service logs.

- **Signal cannot delete plaintext.** Events are immutable and redaction is not built. A rule that sends too much in
  plain cannot be undone after the send. New plain rules should start as `hold` until their output has been checked
  (`beacon rules check` warns on a `send` rule with plaintext personal content, DEC-36). A sealed item's sealed part
  can be removed by the person (`sealed.removed`, Signal DEC-256); its envelope stays, and a backup restored from
  before the removal brings it back.
- **Signal can read sealed items.** Sealing protects content at rest: a database dump, a backup or an admin-minted key
  for `owner` items sees ciphertext. Signal itself decrypts through KMS to answer reads; who can make it do so, and
  how each is recorded, is Signal DEC-261. `owner+agents` items open for the person's agents, which may pass the text
  to model providers.
- **The raise is only as good as its lists.** `seal.owner` matches whole words, senders and labels the source read
  on the machine. A Google body is matched only when a sealed rule made the source read it, and a Slack thread root
  only when the pass read it. Content that does not use the listed words leaves at its rule's level.
- **Signal has no per-event visibility yet.** Until it does, any key that can read the store can read every event
  Beacon sends, including full bodies a rule allowed. Rules that send bodies from personal sources (direct mail,
  direct messages, private files) should wait for visibility, or be limited to content the whole organization may
  read. Gmail, Calendar, Drive and Slack direct-message and private-channel events are the exception: Beacon sends
  them only once Signal restricts their prefix (property 9), and holds them until then. Slack public channel events
  are not personal: a rule that sends a channel message's `body` or `references` sends what it wrote to every reader. That check is only as good as Signal's answer, and a restriction lifted
  after an event was sent does not hide that event again (Signal keeps the owner it recorded).
- **Drive comments are found through recent activity.** A comment does not reliably change a file's modified time,
  so Beacon reads the comments of files modified since the last pass and files you opened in the last 14 days. A
  mention on a file you have not opened in that time, and that was not otherwise modified, is not seen.
- **Gmail's history expires.** After about a week without a pass, Gmail cannot list what changed. Beacon re-reads
  open state and inbox and sent mail since the last pass, and says so; other changes in that gap are lost.
- **A `fromGmail` calendar event is not Gmail.** Calendar events of type `fromGmail` can carry text taken from mail
  (a flight, a reservation). Beacon sends them under `calendar:`, so a Signal restriction on `gmail:*` does not cover
  them. They leave only when a calendar rule matches, and only the fields that rule lists.
- **Google read scopes are broad.** `drive.readonly` can read every file the person can, and `gmail.metadata` every
  message's headers. Beacon asks for names, links, headers and comment metadata, and reads bodies only when a rule
  lists `body`, but the grant itself is not narrower. Revoke it at
  [myaccount.google.com/permissions](https://myaccount.google.com/permissions).
- **Slack read scopes are broad, and the token is the person's.** `channels:history` can read every public channel,
  `search:read` everything the person can find, and `users:read` every member's profile. Beacon calls nine read methods and reads the conversations its
  collection names, but the token itself is not narrower. Setup refuses extra scopes; a later out-of-band grant change
  is outside that check. A user token does not expire unless the app turns on token rotation. Revoke it from the app's page at
  [api.slack.com/apps](https://api.slack.com/apps).
- **Slack approval checks are snapshots.** Visibility, membership, permissions and the root can change after
  the final read; Slack alone enforces its rules when it accepts a post. A channel made private after intake keeps
  the original public proposal record (sensitive fields are sealed). Slack can rewrite text. A user-token post to
  the person's own DM on 23 Nov 2026 kept both the operation metadata and `client_msg_id` (read back with
  `include_all_metadata=true`). A later post that keeps neither still leaves a missing reply unknown. Intake reuses
  a verified `auth.test` for ten minutes; the window and the write always read it afresh.
- **Slack is read by search and by watermark.** Search indexes with a delay, which the 15-minute lookback covers;
  mentions are searched over 7 days, which also finds one an edit added. A quiet followed thread or group direct
  message is read every 15 minutes, so a message there can arrive that late; a deletion is checked every 15 minutes,
  20 a pass, so it can take longer when many asks are open. An edit that adds a mention in a message Beacon read
  only by history or replies, not search, is not seen.
- **An installed app's OAuth secret is not secret to Google.** Google treats a Desktop app's client secret as
  public; what protects the grant is the person's consent, PKCE and the refresh token in the unattended vault.
- **Other people are attributed in metadata, not `by`.** Signal accepts `by` only for identities an actor already
  holds, so Beacon records other people in `metadata.actor` (DEC-27). Signal treats it as plain data: it does not
  link it to an actor or let a search by actor find it.
- **Asks are tracked locally.** Beacon knows which asks are open from its own outbox. If the person closes an ask in
  Signal directly, Beacon's next own act on that subject still names it in `closes`, Signal refuses the event with
  `409 conflict`, and the event is recorded as rejected instead of sent.
- **Signal stores state snapshots.** Beacon sends the state fields a rule lists, nested under `state.fields`, with
  `observed_at` and `version` beside them (DEC-28). Signal keeps the latest snapshot per subject. List state fields
  with the same care as other fields: they leave the machine, and Signal does not interpret them.
- **A 400 from before the decided item was stored cannot be rebuilt from the outbox.** The refused body is not the
  item. `beacon cursor reset github`, then `beacon run --once`, re-collects open state and decides a key again if
  that collect yields it. It does not re-read timeline history. An entry with a recorded `409`, or one Signal
  accepted, is not sent again.
- **The hold queue holds content at rest.** Held items are stored in full in the state directory (`0600`) until
  reviewed.
- **A public channel's messages, with a rule that lists `body`, go to everyone who reads Signal.** Slack channel
  messages (`channel_message`) and review requests in the channels Beacon reads are recorded so agents need not scan
  Slack. A rule that sends a public channel's `body` plain, as the starter rules do, sends text the whole company can
  already read in Slack, and makes it readable to every reader of Signal too, agents and their model providers
  included; Signal cannot delete it. That is the same audience, not a wider one, only while the channel stays public
  and Signal's readers are the company. A private channel's messages are under `slack:dm:` and leave only sealed.
  With `channels: member`, whether a channel is private is read once an hour: a channel made private in that hour is
  still read as public until then, and anything Beacon already sent from it stays where it was sent.
- **A public channel's thread title is public.** With a rule that lists Slack's `title`, a public channel thread is
  named for its channel and who started it ("#eng: Bob Ross mentioned you"), and every reader of Signal sees that
  name, as they see the event. Direct message, group direct message and private channel threads are personal and
  wait for `slack:dm:*` to be restricted, title included.
- **The Slack name cache holds names at rest.** Display names and channel names, by id, are kept in the state
  directory (`slack-names-<source>.json`, `0600`) for a day, so a pass does not ask Slack again. A dry run does not
  write it.
- **A lease in Signal's lease API is not in the outbox.** It is not an event: renewing it sends the same name, run and
  period each time, and nothing is replayed. Every event body, the claims and the event that closes old claims
  included, is still appended first.
- **Coverage shows when the machine is reading.** A source's heartbeat lease is renewed every few minutes while it
  collects, and its snapshot records its last success and last failure. Any key that can read Signal's leases or
  `beacon:` subjects can tell from them when your Beacon was running and when it stopped or slept, a little more finely
  than the send lease already shows. They say nothing about what was read.
- **The lease follows the local clock.** A handover takes up to two lease periods, and machines whose clocks differ
  by more than a period may disagree on the current period.
- **Beacon trusts the machine it runs on.** Your user's processes can read plaintext state and the unattended
  vault, replace the executable, unit or proxy, or tamper with the browser. A passkey proves presence, not what a
  compromised browser displayed. A memory read during approval can expose the data key or a PRF output. A backup
  containing both the unattended database and key opens it. A terminal dry run can print private item content.
- **Shutdown can interrupt a collect or send.** The reaper escalates from TERM to KILL after 15 seconds. An
  unfinished send is recovered by the collector's existing outbox rules. A failed collector can restart with
  backoff only while the app is running. Cleanup without a verified acknowledgement is
  unconfirmed. These guarantees cover app-supervised services, not an independent terminal run or legacy job.
- **Beacon trusts the Signal host it is pointed at.** It checks TLS. It does not protect what the store does with
  data after it arrives.
