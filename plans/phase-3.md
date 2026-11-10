# Phase 3 — Google: Gmail, Calendar, Drive

Status: built against fixtures; the live dry run against a real Google account is pending (the person authorises
after this pull request). What was built is described in [`docs/sources.md`](../docs/sources.md),
[`docs/commands.md`](../docs/commands.md) and [`docs/configuration.md`](../docs/configuration.md).

## Goal

The person's own mail, calendar and document comments as sources, shaped by DEC-9, DEC-10, DEC-17 to DEC-21,
DEC-23 and DEC-28 like GitHub and Jira, with read-only Google access the person grants to their own OAuth client,
and a starter rules file that sends what is actionable, holds mail for review, and sends no body. At the end,
Beacon can run in dry-run against a real person's Google account and its report reads like that person's inbox,
invitations and comment mentions.

## Scope

### Google OAuth (shared by the three sources)

- **The person's own client (DEC-2).** The person creates an OAuth client of type *Desktop app* in their own Google
  Cloud project and downloads its JSON. `beacon source add google --account <address> --client-file <json>` stores the
  client id and secret in the keychain (`google:<address>:client`). Each Google source names its `account`. Google documents an installed app's secret as not
  confidential; Beacon still treats it as a credential: keychain only, never printed, logged or written to disk.
- **Installed-app flow.** Authorization code with PKCE (S256) and a loopback redirect: Beacon listens on
  `127.0.0.1` on a random port for one redirect, checks `state`, and exchanges the code at the token endpoint.
  It prints the consent URL; `--open` opens it in the default browser. The browser, not Beacon, contacts
  `accounts.google.com`. `access_type=offline` and `prompt=consent` so Google returns a refresh token.
- **Refresh token in the keychain** (`google:<address>:refresh`). Each pass exchanges it for an access token,
  held in memory only and reused by the three sources until a minute before it expires. A refused refresh
  (`invalid_grant`) is `source_unauthorized`; the person runs `beacon source add google` again.
- **Only the keychain package touches a credential.** Building the token request, reading the code from the
  redirect, and reading the token response are in `internal/keychain`; callers hold `Secret` values.
- **Scopes, read-only, the fewest that work:**

  | Scope | For | Why not narrower |
  | --- | --- | --- |
  | `gmail.metadata` | Gmail headers, labels, history | It is the narrowest Gmail scope that reads headers and history. |
  | `gmail.readonly` | Gmail bodies, only with `--bodies` | `gmail.metadata` cannot read a body. |
  | `calendar.events.readonly` | Calendar events; Drive meeting attendance | Narrower than `calendar.readonly`: no calendar list or settings. |
  | `drive.readonly` | Drive files, comments, meeting notes | `comments.list` requires it; `drive.metadata.readonly` cannot read comments. |

  The grant asks only for the scopes of the Google sources in the config. A source whose scope was not granted fails
  with `source_unauthorized` and names nothing.
- **Hosts.** `oauth2.googleapis.com` (token endpoint, every Google source), `gmail.googleapis.com` (Gmail),
  `www.googleapis.com` (Calendar and Drive). Listed by `beacon egress`, and `beacon source add google` uses the
  same guarded transport. Each source also checks every URL against the API paths it uses, and each
  path against the query keys Beacon sends there (no `alt=media`, no search, no raw message), since
  `www.googleapis.com` serves other APIs too.
- **Rate limits.** `429`, and a `403` whose reason is `rateLimitExceeded`, `userRateLimitExceeded` or
  `RESOURCE_EXHAUSTED`, back off like Phase 2 and end the pass when the wait is long. A `403` for a missing scope is
  `source_unauthorized`. Any other `403` fails the pass; it is not read as "this object is gone".

### Personal subjects (DEC-10)

The person in a subject is the Signal handle without `@` (`gmail:winton:thread:…`), which is where Signal phase 11
reads the owner of a restricted prefix. A config with a Google source needs `person.handle` in `@handle` form.

**Enforced, not assumed.** A send pass with a Google source reads `GET /api/restrictions` (Beacon's key needs
`read`) and sends a personal subject only when the longest restricted stem over it is its own (`gmail:*`). Not
restricted: the source's send items are held, and `run` says which prefix to restrict. Unreadable: they are not
recorded and the cursor stays. Pending bodies and `outbox retry --rejected` wait the same way. The pass report carries
this (`Withheld`) for `beacon service status` to show once the service (#10) lands.
`person.identities` for each Google source is `email:<address>` (DEC-27); Gmail checks it against the profile,
Drive against `about`, Calendar against the attendee or organizer marked `self`.

### Gmail (`kind: gmail`)

- **Cursor.** The Gmail history id and the time of the pass. Each pass reads `users.history.list` from the id
  (`messageAdded`, `labelAdded`). The next cursor is the mailbox's history id from that response. An expired history
  id (`404`) reads open state again, and inbox and sent mail since the stored time by label (`gmail.metadata` cannot
  search), and the gap is reported.
- **First run (DEC-23).** Inbox threads with a message in the last 14 days (the latest message of each), and
  starred messages from the last 14 days. The cursor is the profile's history id read before listing.
- **Items (DEC-18).** `received` (a new message from someone else), `sent` (the person's own message, by the
  `SENT` label only: a From header can be forged), `starred` (the `STARRED` label added). Drafts, spam, trash and chats are
  skipped. Label changes other than starring are not events.
- **Subject:** `gmail:<person>:thread:<thread id>`. **Key:** `gmail:<thread id>:<message id>` and
  `gmail:<thread id>:starred:<message id>`. The version is the message id.
- **Fields:** `from`, `to`, `subject`, `date`, `labels`, `link`, `body`. `body` (the `text/plain` part) is read only
  when a rule lists `body` for the source and the grant includes `gmail.readonly`. **State:** `unread`, `starred`,
  `labels`, `last_from`, over the whole thread.
- **Addressed** when the person's address is in To. **Own** when the message is from the person.
- **Event titles:** New message, Reply received, Sent, Replied, Starred. Never the subject, a name or the body.

### Calendar (`kind: calendar`)

- **Cursor.** The first server `Date` of the pass (as Phase 2), plus a fingerprint per event in the window: a hash
  of its summary, start, end, location and status, and the person's response. A change of state is a change of
  that fingerprint, so another attendee's response, which bumps `updated`, is not an event.
- **Each pass:** events on the primary calendar updated since 15 minutes before the cursor, expanded to single
  instances, that end after now and start within 30 days, always with deleted events; and, without the window, known
  events that moved into the past or beyond 30 days. On a `410` the whole window is read, and a known in-window
  event missing from it is read by id: gone is `cancelled`.
- **First run (DEC-23).** Events in the next 7 days. The rest of the 30-day window is fingerprinted, not emitted.
- **Items (DEC-18):** `created`, `changed`, `cancelled`, `response_needed` (the person's response is
  `needsAction` on someone else's event), `responded` (the person's own response changed; own, so it closes the
  ask; with `created` by the person's account, the only own Calendar items: Calendar does not say who changed an
  event), `upcoming` (first run only). One item per event per pass: cancelled, then a response change, then a change.
- **Subject:** `calendar:<person>:<event id>`. **Key:** `calendar:<event id>:<kind>:<etag>`; the version is the
  event's etag (DEC-9 marker: `updated` and `etag`).
- **Fields:** `summary`, `start`, `end`, `organizer`, `attendees` (a count), `response`, `link`, `body`
  (description, only when a rule lists `body`). **State:** `status`, `start`, `end`, `response`.
- **Due:** `due_at` is the start (DEC-21).
- **Event titles:** Created, Changed, Rescheduled, Cancelled, Response needed, Responded: accepted (declined,
  tentative), Upcoming.

### Drive (`kind: drive`)

- **Cursor.** The first server `Date` of the pass, as Phase 2.
- **Comments.** A comment does not reliably update a file's `modifiedTime`, so the files read are those modified
  since the window and those the person opened in the last 14 days, then `comments.list` with
  `startModifiedTime`. Items: `mentioned` (a comment or reply by someone else that
  mentions the person), `assigned` (a comment or reply by someone else that assigns it to the person; wins over
  `mentioned`), `commented` and `resolved` (the person's own comment, reply or resolve; own).
- **Meeting notes.** Events on the primary calendar that ended in the last 7 days, updated since the window, that
  the person organised or accepted, with a Google Doc attachment titled `Notes by Gemini`. One `notes_created` item
  per notes file, with a link. Its content is read (`files.export`, plain text, first 32 KiB) only when a rule
  lists `body`.
- **First run.** Unresolved comments from the last 7 days that mention or are assigned to the person, and notes
  from meetings in the last 7 days. No own acts.
- **Subject:** `drive:<person>:<file id>`. **Key:** `drive:<file id>:comment:<comment id>`,
  `drive:<file id>:reply:<comment id>:<reply id>`, `drive:notes:<file id>`.
- **Fields:** `title` (file name), `link`, `file_type`, `author` (display name; Drive does not return the
  author's address), `meeting`, `meeting_start`, `body` (comment text or notes text, only when a rule lists it).
  **State:** `resolved`, `assignee`.
- **Event titles:** Mentioned, Assigned, Commented, Replied, Resolved, Meeting notes created.

### Framework changes

- Sources that read open state on a first run and need `beacon accept-first` are every source but the fixture.
- A source may be told whether any rule lists `body` for it (`SetBodies`); Google sources read bodies only then.
- The projection treats `subject` like `summary`: the thread title when a rule lists it and lists neither `title`
  nor `summary`.

### Starter rules

`examples/rules.yaml` gains Google rules: own acts as `send` (they close asks; own sent mail leaves as its link and
ids only); starred mail as `send` with
`ask: me`; bulk categories `never`; other mail addressed to the person `hold` with `ask: me`; calendar
`response_needed` as `send` with `ask: me`; changes and cancellations of others' events as `send`; first-run
`upcoming` as `send`; Drive assignments and mentions as `send` with `ask: me`; meeting notes as `send`. No rule
lists `body`. Everything else stays `never`.

## Out of scope

Slack, attachments and file contents other than meeting notes text, shared calendars other than the primary, Gmail
push notifications, Drive push notifications, the review queue UI, a live run against a real account in this
pull request.

## Tests

- Each source against recorded fixtures served by `httptest`, covering every item type, own acts, mentions and
  assignment, pagination, rate-limit backoff, a refused grant, an expired Gmail history id, and a cursor that only
  moves after commit.
- The OAuth flow end to end against a fake token endpoint and a simulated browser: PKCE challenge matches the
  verifier, a wrong `state` is refused, the refresh token lands in the keychain and is never printed.
- Keys are stable across passes; a re-collected change yields the same key.
- No request leaves the declared hosts and API paths (egress guard plus a URL scope check).
- The starter rules against the fixtures: the expected items are sent or held, the rest dropped, and no body leaves.

## Exit gate

All gates green. Against fixtures, the three sources produce the documented items, subjects, keys, fields and state
snapshots. After this pull request, the person authorises a Google account and a live dry run (`beacon run --first`)
completes, its report is checked by the person, and nothing is sent. Sending personal subjects waits for the Signal
admin to restrict `gmail:*`, `calendar:*` and `drive:*` (Dependencies on Signal, item 1).
