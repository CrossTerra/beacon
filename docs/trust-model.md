# Trust model

Beacon sits between accounts that belong to one person and a store that many people and agents read. This file
says what Beacon protects, from whom, and how. Code that changes any of these properties changes this file in the
same pull request.

## What crosses the boundary

| Leaves the machine | Never leaves the machine |
| --- | --- |
| Events a rule sent: the projected fields and state fields, plus an event title that describes the change | Source credentials (OAuth clients, refresh and access tokens, API tokens) |
| The Signal key's bearer header, to the configured Signal host | Items the rules dropped or that are waiting for review |
| Requests to the source APIs, made with your credentials, including on a dry run, and to Google's token endpoint | The rules file, the review queue, the outbox (the stored decision keeps listed fields and the event title, not an unlisted body), the cursors, the seed file, classifier input and output |
| Lease claims: your Signal handle, a random instance id and the lease period | Rule names, and why an item matched |
| With a Google source, a read of Signal's restricted prefixes (`GET /api/restrictions`), which sends nothing but the key | |
| Your own identity in a source, as `by`, on your own acts | Who else caused a change, unless a rule lists `actor` |

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
   issue, mail or file title. It is built only from the change the source already recorded (who was requested, the
   review state, a short commit sha, the check conclusion, the assignee's display name, the status names, the due
   date; for Google, the item type and the person's calendar response) and it never includes comment, review or
   mail text, a mail subject, an event summary or a file name. The object's title leaves only when a rule lists
   `title`, `summary` or `subject`, as the thread title. A Google source does not even read a mail body, an event
   description, comment text or meeting notes unless a rule that can match it lists `body`.
4. **Local classifier by default.** The classifier runs on the machine. Using a hosted model is an explicit
   setting, shown by `beacon egress`, because it sends item content to that model's vendor before the filter
   decides.
5. **The outbox is the record.** Every request body, lease claims included, is appended to the outbox and synced
   to disk before it is sent. A pending retry resends those exact bytes, which is what Signal requires of a key it
   already holds. A body Signal accepted (`sent`, `replayed`) is never resent and never rewritten. A `409` is never
   retried and never decided again. A `400` with no stored item is not a decision: `beacon run` may decide that key
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
   redirects are not followed. Each Google source also refuses any URL outside the API paths it uses, because
   `www.googleapis.com` serves other APIs too. `beacon source add google` listens on `127.0.0.1` for one OAuth
   redirect; the person's browser, not Beacon, opens Google's consent page. There is no telemetry, no update check
   and no crash reporting.
7. **Credentials stay in the keychain.** Only the keychain package reads them, when they are needed. They are
   never written to disk, logs or the outbox; a credential prints as `[redacted]` however it is formatted. The
   Signal URL must be `https`, except to a loopback address. The per-user agent is a LaunchAgent in the user's GUI
   session, never a LaunchDaemon. `beacon run` and `beacon service` refuse to start as root, so a plist copied into
   `/Library/LaunchDaemons` exits instead of running as root. The plist names the signed binary, the config path and
   the log paths. It has no environment variables and no credential. The agent reads keys from the keychain when a
   pass sends, the same as a terminal `beacon run`. Google access is
   read-only scopes on the person's own OAuth client (DEC-2). The client and the refresh token are in the keychain;
   the keychain package builds the token requests, reads the authorisation code from the redirect (checking `state`,
   with a PKCE verifier) and reads the token responses, so no other code holds a token's bytes. An access token is
   kept in memory, not on disk.
8. **Logs carry no content.** Log lines have fixed event names and carry counts, source names, outcomes, statuses,
   Signal error codes and fixed reason codes: no item content, no rule matches, no keys, no bodies, no tokens, and no
   error text, which can name any of those. `beacon run` prints a fixed message for each reason.
9. **Personal subjects stay personal.** Gmail, Calendar and Drive subjects carry the person's handle
   (`gmail:<handle>:…`, DEC-10). A send pass reads Signal's restricted prefixes first, and sends an event on such a
   subject only when the longest restricted stem it is under is its own (`gmail:*`), which is what makes Signal keep
   it to its owner. Otherwise the source's send items are held locally; if the restrictions cannot be read, they are
   not recorded and the cursor stays. A pending body, or a refusal `beacon outbox retry --rejected` would resend, on
   such a subject waits the same way. Your own acts are recorded from the source's own record only: a Gmail message is
   yours only with the `SENT` label, never because its From header names you, and a calendar change is yours only when
   it is your response or an event your account created.
10. **One sender per person.** Only the instance holding the lease sends (DEC-25), and no write outlasts the lease.
    Another instance collects and keeps its bodies pending.
11. **Reproducible and signed.** Builds are reproducible from a commit (checked by `scripts/repro.sh` on every pull
    request). Signed releases, notarized macOS builds and an SBOM per release arrive in Phase 7.

## Known limits

- **Signal cannot delete.** Events are immutable and redaction is not built. A rule that sends too much cannot be
  undone after the send. New rules should start as `hold` until their output has been checked.
- **Signal has no per-event visibility yet.** Until it does, any key that can read the store can read every event
  Beacon sends, including full bodies a rule allowed. Rules that send bodies from personal sources (direct mail,
  direct messages, private files) should wait for visibility, or be limited to content the whole organization may
  read. Gmail, Calendar and Drive events are the exception: Beacon sends them only once Signal restricts their prefix
  (property 9), and holds them until then. That check is only as good as Signal's answer, and a restriction lifted
  after an event was sent does not hide that event again (Signal keeps the owner it recorded).
- **Drive comments are found through recent activity.** A comment does not reliably change a file's modified time,
  so Beacon reads the comments of files modified since the last pass and files you opened in the last 14 days. A
  mention on a file you have not opened in that time, and that was not otherwise modified, is not seen.
- **Gmail's history expires.** After about a week without a pass, Gmail cannot list what changed. Beacon re-reads
  open state and inbox and sent mail since the last pass, and says so; other changes in that gap are lost.
- **Google read scopes are broad.** `drive.readonly` can read every file the person can, and `gmail.metadata` every
  message's headers. Beacon asks for names, links, headers and comment metadata, and reads bodies only when a rule
  lists `body`, but the grant itself is not narrower. Revoke it at
  [myaccount.google.com/permissions](https://myaccount.google.com/permissions).
- **An installed app's OAuth secret is not secret to Google.** Google treats a Desktop app's client secret as
  public; what protects the grant is the person's consent, PKCE and the refresh token, which stays in the keychain.
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
  that collect yields it. It does not re-read timeline history. A `409`, and anything Signal accepted, is not sent
  again.
- **The hold queue holds content at rest.** Held items are stored in full in the state directory (`0600`) until
  reviewed.
- **The lease follows the local clock.** A handover takes up to two lease periods, and machines whose clocks differ
  by more than a period may disagree on the current period.
- **Beacon trusts the machine it runs on.** Anyone with your user account can read the outbox, the queue and the
  keychain items Beacon uses. The launchd agent is that same user. Its standard output file holds item keys and rule
  names, not bodies, unless the agent was started with `--bodies`. A terminal dry run still prints bodies. The JSON
  log has neither. Both files stay in the state directory.
- **launchd can kill a stop.** After SIGTERM, launchd waits 180 seconds and then sends SIGKILL. A send still in a
  retry wait can be killed. The outbox recovers that the same way it recovers a crash. A crash is restarted after
  the 10-second throttle. A persistent failure (a refused key, a refused lease, an egress block, a state file) is
  not: the process records the reason and waits one poll interval, doubling up to 30 minutes, before the non-zero
  exit that launchd restarts.
- **Beacon trusts the Signal host it is pointed at.** It checks TLS. It does not protect what the store does with
  data after it arrives.
