# Sources

A source yields items. Beacon has GitHub, Jira, Gmail, Google Calendar, Google Drive and Slack, beside the
[fixture](configuration.md#fixture-source). Each has a fixed field list and a fixed state field list. A rule names
what leaves; anything else stays.

Credentials are in the keychain, under the service `beacon`: account `source:<name>` for GitHub and Jira, and
`google:<address>:client` and `google:<address>:refresh` for the [Google grant](#google) a Google account's sources
share, and `slack:<workspace id>:<user id>` for a [Slack](#slack) user token. They are read
when a pass needs them and set only as an Authorization header (or, for the Google grant, sent only to Google's
token endpoint). They are never written to the config, the log, or the outbox.

## Cursor

`Collect` returns the items and an opaque next cursor. The runner stores it in `cursors.json` only after every item
of that pass has been sent to the outbox, held, or dropped, and only on a send pass. A crash before that collects
the same items again; keys make that harmless (DEC-9). A dry run reads the cursor and does not write it. A rate
limit or a failed collect leaves the cursor where it is. An item that fails validation, or that a rule matched but
could not be projected, is dropped and logged by count, with a fixed reason code and no item content. That does not
freeze the source: the cursor still moves.

For GitHub, Jira and Drive, the next cursor is the first `Date` header of the pass, or the stored cursor if that is
later. It is never the
local clock, and it never moves backwards. A later `Date` in the same pass is ignored, so a pass longer than the
lookback does not skip events that arrived while it was running. A `Date` more than an hour ahead of the local
clock fails the pass. With no `Date`, the cursor is the newest source event time, not a local-clock stamp on a
push or a check change. If there is no `Date` and no such event time, the cursor is not moved. A pass reads from
the committed cursor minus 15 minutes, so indexing lag and clock skew land in an overlap. Gmail's cursor is the
mailbox's history id, and Calendar's adds a fingerprint per event; see [Gmail](#gmail) and [Calendar](#calendar).
Slack's adds a watermark per followed thread and configured channel; see [Slack](#slack).

A source with no cursor reads open state, not history (DEC-23). That first pass is a dry run until
`beacon accept-first`. See [commands](commands.md#beacon-run). `beacon cursor reset <source>` removes a committed
cursor so the next collect reads open state again, not history. Acceptance is kept. A `400` with no stored item
is decided again if that collect yields the key; a `409`, and anything Signal accepted, is not. See
[commands](commands.md#beacon-cursor-reset).

## Seed

`beacon seed` imports lines of `<subject> never|send`. A `never` line drops that subject before the rules file, so
an item already dismissed stays dismissed (DEC-24). A `send` line is recorded and does not force a send: the rules
file still decides, and the default remains deny. The format is in [commands](commands.md#beacon-seed).

## Rate limits

Each source honours `Retry-After` and, when present, `X-RateLimit-Remaining` and `X-RateLimit-Reset`. A wait of at
most two minutes is slept, then the request is retried. A longer wait ends the pass. The cursor is not moved, so
the next pass resumes from it. Google answers a rate limit with `429`, or with a `403` whose reason is
`rateLimitExceeded`, `userRateLimitExceeded`, `quotaExceeded` or `RESOURCE_EXHAUSTED`; both end the pass the same
way. Slack answers with `429` and `Retry-After`, or `"error":"ratelimited"`, which ends the pass; Beacon also spaces
its own Slack calls by each method's tier ([Slack](#slack)).

## GitHub

`kind: github`. A fine-grained or classic token. `beacon source add github --from-gh` imports the token
`gh auth token` prints, after confirmation (`--yes` when input is not a terminal). With `--host`, that command is
`gh auth token --hostname <host>` (a port on `--host` is not passed).

**Host.** `api.github.com`, or `host:` for GitHub Enterprise (`https://<host>/api/v3`). Listed by `beacon egress`.

**Scope.** `repos`: `owner/name` or `owner/*`. Nothing outside them is requested. `owner/*` is searched as the
owner's user or organization, and a result from any other repository is not fetched.

**Each pass.** Pull requests in the configured repositories updated since 15 minutes before the cursor, through the
search API. The qualifier is `updated:>=` with an RFC3339 timestamp, one repository qualifier per query (an OR of
`repo:` qualifiers matches nothing). A hit whose `updated_at` is before that window is not fetched, so a pass does
not re-read every pull request updated that day. If an owner search reports more than 1000 results, it is split into one query per repository in that owner, so the
newest are not cut off and the source does not stall. The token user's own repositories, including private ones, are listed from `/user/repos` with
`affiliation=owner`. Collaborator and organization-member repositories are not treated as that login's. A single repository that itself reports more than 1000 fails
the pass and the cursor stays: results are ordered oldest first. The search does not use a login
qualifier: some tokens can read the repositories and still cannot search a user. A pull request the person is not
involved in (author, assignee, reviewer, review requested of them or of a team they are in, mentioned, or commenter)
yields no items. The full timeline is fetched; events before the lookback window are not emitted. Own open pull
requests are also read for check suites, because a check conclusion does not change the pull request's `updated`
time. If that extra search fails, items from the updated search are still returned and the cursor stays. A pull
request the token cannot read (404, or a 403 that names a permission failure) is skipped. A bare 403 is not a skip:
a secondary rate limit often arrives as a 403 with no `Retry-After`, and skipping it would move the cursor past
unread events. That 403 fails the pass. A failure reading the timeline or reviews fails the pass and the cursor
stays. Check suites and pull-request review comments the token cannot read (404 or a permission 403) are omitted
rather than stalling every other event. A bare 403 on those fails the pass. The token needs permission to read the
configured repositories.

**First run.** Open pull requests in the configured repositories, kept when the person wrote them, is a requested
reviewer, or a requested team includes the person. Not their history: no old comments. The current review requests,
including a request for a team the person is in, and, on the person's own pull requests, the current check
conclusion. A team request is addressed.

**Items.** `opened`, `ready`, `review_requested`, `review_request_removed`, `reviewed` (field `review_state`),
`commented`, `pushed` (only on a pull request the person has reviewed), `checks_changed` (own open pull requests
only), `merged`, `closed`. A merge yields `merged` only, never also `closed`. `closed` is a close without a merge.
The event title describes the change. The pull request title stays the `title` field, and the thread title when a
rule lists `title`. The event title never includes comment or review text. Keys and subjects are unchanged; a push
key keeps the full sha.

| Item | Event title |
| --- | --- |
| `opened` | Opened |
| `ready` | Ready for review |
| `review_requested` | Review requested from `<login or team>`. A team is `org/slug`. With no login or team, Review requested. |
| `review_request_removed` | Review request removed |
| `reviewed` | Reviewed: approved, Reviewed: changes requested, or Reviewed: commented. Any other state is that state with underscores as spaces. With no state, Reviewed. |
| `commented` | Commented |
| `pushed` | Pushed `<short sha>`. The short sha is the first 7 characters, or the whole sha when it is shorter. |
| `checks_changed` | Checks: passing, Checks: failing, or Checks: pending. `pass` is passing and `fail` is failing. A conclusion of `none` is Checks: none. |
| `merged` | Merged |
| `closed` | Closed |

`own` is set when the person caused the change. `checks_changed` has no actor and `own` is false: a check finishing
is not the person's act, and must not close their asks. `pushed` is not filtered by the git commit's author or committer time when it is the current head. A commit made
earlier can be pushed later. The head sha is emitted when it differs from the commit the person last reviewed,
stamped with the pass `Date`. A `committed` push has no actor and is not own: a git author, including a noreply address, is not a signed-in act and must not close the person's asks (DEC-20). The actor is set only from a force-push's GitHub user. Other commits stay inside
the lookback window, so history from before the first run is not emitted (DEC-23). The key is the sha, so a later
pass that sees the same head does not send twice. A key longer than 200 characters is shortened with a hash
of the original, so it stays stable and is not dropped.

**Subject.** `github:pr:<owner>/<repo>#<n>`. **Key.** `github:<owner>/<repo>#<n>:<kind>:<id>`. The kind is `opened`
(the pull request's id; the open has no timeline event), `timeline` (ready, review requested or removed, merged,
closed), `review`, `comment` (an issue comment), `review-comment`, `push` (the commit sha), or
`checks:<head sha>:<conclusion>`. Kinds keep ids that overlap across those APIs from colliding.

**Addressed.** A review request naming the person, or naming a team the person is in. A team is `org/slug`, and only
a team whose organization is a configured owner counts. `@login` in a comment, a review, or the pull request body
or title on `opened`. `@login` does not match a longer login.

**Fields.** `repo`, `number`, `title`, `url`, `author`, `review_state`, `body` (comment or review text), `base`,
`head_ref`.

**State.** `state` (`open`, `closed`, `merged`), `draft`, `head_sha`, `review_decision` (`approved`,
`changes_requested`, `review_required`), `mergeable` (`true`, `false`, `unknown`), `checks` (`pass`, `fail`,
`pending`, `none`), `requested_reviewers` (logins, comma-separated). `checks` is set when Beacon read check suites.
A failure, timeout, cancellation, or action required is `fail`; an incomplete suite is `pending`; success, neutral,
and skipped are `pass`.

**References.** An issue key matching `[A-Z][A-Z0-9]+-\d+` in the title or head branch, limited to the GitHub
source's `projects` and to any Jira source's projects, becomes `jira:<KEY>`.

**Identity.** `person.identities.<name>` is `github:user:<login>`, the login the token's user has. A mismatch is
`identity_mismatch` and the pass does not use that source.

## Jira

`kind: jira`. The Atlassian account email and API token, basic auth over HTTPS, both in the keychain.
`beacon source add jira --site SITE` reads the email, then the token, and does not print the token. A pipe is read
with one reader, so the token line is not lost to buffering.

**Host.** `<site>.atlassian.net`, from the site label. A site is not a hostname, so the config cannot name an
arbitrary host. Listed by `beacon egress`.

**Scope.** `projects`: project keys. Nothing outside them is requested. A search result from another project is
not fetched.

**Each pass.** Issues in those projects where the person is assignee, reporter, watcher, or mentioned, updated
since 15 minutes before the cursor. The JQL date is formatted in the account's timezone, which is how Jira
evaluates `updated` for this token. The `text ~ account id` clause is not verified against live ADF mentions.
Mentions are also taken from fetched comments, including ADF mention nodes, so a mention in a comment Beacon
fetched is addressed even when that search clause misses it. The changelog is fetched in full; entries before the
lookback window are not emitted. Comments before that window are not emitted. The next cursor is the first server `Date` of the pass, or the stored cursor if that is later. It never moves
backwards, and a `Date` more than an hour ahead of the local clock fails the pass. An issue that has moved out of
the configured projects is dropped and counted. It does not fail the pass.

**First run.** Unresolved issues assigned to the person. Not their history: comments are not read. One `assigned`
item per issue, keyed by the latest assignment's changelog id, or `open` when the issue has none.

**Items.** `assigned`, `unassigned`, `transitioned` (`from_status`, `to_status`), `commented`, `mentioned`,
`due_changed`. A comment is one item, `commented`, with `addressed` when it mentions the person. `mentioned` is
only a mention added to the description or another field, so one change is not two items. `own` is set when the
person caused the change. The event title describes the change. The issue summary stays the `summary` field, and
the thread title when a rule lists `summary`. The event title never includes comment text or the text of a field
a mention was added to. Keys and subjects are unchanged.

| Item | Event title |
| --- | --- |
| `assigned` | Assigned to `<name>`. The name is the display name the changelog already returned, or the current assignee's display name on a first run that has none. With no name, Assigned. |
| `unassigned` | Unassigned |
| `transitioned` | `<from> → <to>`, the status names already on the item. With neither, Transitioned. |
| `commented` | Commented |
| `mentioned` | Mentioned |
| `due_changed` | Due date set to `<date>`, the date the changelog already returned. A cleared due date is Due date cleared. |

**Addressed.** Assignment to the person; a comment or field change that mentions the person; a transition, by
someone else, of an issue assigned to the person.

**Subject.** `jira:<KEY>`. **Key.** `jira:<KEY>:<changelog id>` for a field change, and
`jira:<KEY>:<comment id>:comment` for a comment. Comment ids and changelog ids are separate sequences and can
overlap, including across passes, so a comment key always ends in `:comment`. When one changelog history holds
more than one relevant field change, the id is suffixed with the field (`jira:ENG-1:100:status`) so the two stay
distinct. A history with one field change uses the changelog id alone. Both forms are stable.

**Fields.** `project`, `key`, `summary`, `url`, `issue_type`, `priority`, `body` (comment text), `from_status`,
`to_status`.

**State.** `status`, `assignee` (display name), `resolution`, `due` (`YYYY-MM-DD`), `priority`.

**Due.** The issue's due date is `due_at`, the end of that date in the account's timezone (DEC-21).

**Identity.** `person.identities.<name>` is `jira:account:<account id>`. A mismatch is `identity_mismatch`.

## Google

`kind: gmail`, `kind: calendar`, `kind: drive`, each with `account:`, the Google account's primary address. The
sources of one account share one OAuth grant, from an OAuth client the person creates in their own Google Cloud
project (DEC-2). Every scope is read-only.

**The client.** In Google Cloud Console, create an OAuth client of type *Desktop app* and download its JSON.
`beacon source add google --account <address> --client-file <json>` stores its id and secret in the keychain as
`google:<address>:client`. A web
client is refused: Beacon's redirect is a loopback address. Google documents an installed app's secret as not
confidential; Beacon still keeps it only in the keychain.

**Authorising.** `beacon source add google` runs the installed-app flow (RFC 8252): it prints the consent URL (and
opens it with `--open`), listens on `127.0.0.1` on a random port for one redirect, and exchanges the code, with a
PKCE (S256) verifier, at `oauth2.googleapis.com`. The person's browser, not Beacon, contacts the consent page at
`accounts.google.com`, with `login_hint` set to the account. A request to the listener without the flow's `state` is
refused and the flow keeps waiting. The refresh token is stored in the keychain as `google:<address>:refresh`. If the person does not grant every scope asked for,
nothing is stored and the command names the missing scopes. The grant covers every Google source in the config for that
account, so adding one later (`--drive`) asks again for all of them. Without `--client-file`, the account's stored
client is used.

**Each pass** exchanges the refresh token for an access token, held in memory only and reused by the three sources
until a minute before it expires. A refused refresh (revoked, expired, or a changed client) is
`source_unauthorized`; run `beacon source add google` again.

**Scopes.** The grant asks for the fewest that work for the sources being enabled:

| Scope | Asked for | Why |
| --- | --- | --- |
| `gmail.metadata` | Gmail | Headers, labels and history, and nothing of a body. The narrowest Gmail scope that reads them. |
| `gmail.readonly` | Gmail, instead of `gmail.metadata`, with `--bodies` | A body can be read only with it. A grant holding `gmail.metadata` cannot read a body even beside another Gmail scope, so `--bodies` replaces it. |
| `calendar.events.readonly` | Calendar, and Drive | Events on the primary calendar. Narrower than `calendar.readonly`: no calendar list or settings. Drive uses it to find meetings the person attended. |
| `drive.readonly` | Drive | `comments.list` needs it; `drive.metadata.readonly` cannot read comments. It can read every file the person can, but Beacon asks only for file names and links, comment metadata, and meeting notes text when a rule lists `body`. |

A source whose scope was not granted fails with `source_unauthorized`. A `403` for a missing scope
(`insufficientPermissions`, `ACCESS_TOKEN_SCOPE_INSUFFICIENT`) is the same. On Drive, a `403` that one object cannot be read
(`forbidden`, `insufficientFilePermissions`), or a `404`, on a request for one object (a file's comments, a notes
file) skips that object. On Gmail only a `404` skips a thread (see [Gmail](#gmail)). Any other `403`, or one on a
list, fails the pass.

**Hosts.** `oauth2.googleapis.com` (every Google source), `gmail.googleapis.com` (Gmail), `www.googleapis.com`
(Calendar and Drive). Listed by `beacon egress`. `www.googleapis.com` serves other APIs too, so each source also
refuses any URL outside the paths it uses: Gmail `/gmail/v1/users/me/` `profile`, `labels`, `history`, `threads`,
`messages` and one thread or message by id; Calendar `/calendar/v3/calendars/primary/events`; Drive
`/drive/v3/about`, `/drive/v3/files`, one file by id with `comments` or `export`, and the primary calendar's events.
Each path also takes only the query keys Beacon sends there, so `alt=media` (a file's content), a search on Gmail,
`format=raw` on a message, or an export to anything but plain text is refused before it is sent.

**Personal subjects (DEC-10).** The person in a subject is `person.handle` without `@`: `gmail:winton:thread:…`.
Signal reads the owner of a restricted prefix there. A config with a Google source needs `person.handle` as an
`@handle`.

**Nothing personal is sent unless Signal keeps it private.** The personal stems are fixed (`gmail:`, `calendar:`,
`drive:`, `slack:dm:`), not taken from the enabled sources, so removing Gmail does not let a pending or rejected
`gmail:` body leave. A send pass reads Signal's restricted prefixes (`GET /api/restrictions`, which needs the `read`
scope on Beacon's key) when a personal source is enabled or the outbox holds a pending or rejected event under one
of those stems, and again immediately before pending bodies are sent. A subject is private only when the longest
restricted stem it is under is its own (`gmail:*` for `gmail:winton:…`): Signal reads the owner right after that
stem. Then:

- When Signal answers and the stem is not restricted (or a longer stem such as `gmail:winton:*` would move the
  owner), the source's `send` items are held in the local queue instead, `beacon run` says which prefix to restrict,
  and the log has `withheld` with `prefix_unrestricted`. Held is a decision: the cursor moves.
- When the restrictions cannot be read (no `read` scope, `401`, Signal unreachable), the source's `send` items are
  reported and not recorded, and its cursor stays, so a later pass that can read them decides those items again.
  The log has `restrictions_unread` and `withheld` with `restrictions_unknown`.
- A body already pending on a personal subject is not sent while its stem is not shown to be restricted, including
  after that source is removed from the config; it stays pending. `beacon outbox retry --rejected` skips such an
  entry the same way and counts it as `withheld`.

A dry run does not contact Signal and does not check.

**Identity.** `account` and `person.identities.<name>` (`email:<address>`) must both be the account's primary
address. Gmail checks them against the profile, Drive against `about`, Calendar against the attendee, organizer or
creator marked `self`. A mismatch is `identity_mismatch`, so a grant for another account reads nothing.

**Bodies.** A Google source reads a body only when a `send` or `hold` rule that can match it lists `body`. Without
one, Gmail does not fetch a message, Calendar does not ask for descriptions, and Drive asks for neither comment text
nor notes text.

### Gmail

**Cursor.** The mailbox's history id, and the time of the pass that stored it (the server's `Date`, else the newest
message seen). Each pass reads `users.history.list` from the id (`messageAdded`, `labelAdded`), and the next cursor
is the history id that response reports.

**An expired history id** (`404`, after about a week) cannot say what changed. The pass reads open state again (as a
first run), and every inbox and sent message since 15 minutes before the stored time (at most 14 days), so earlier
messages in a thread and the person's own mail in the gap are not lost. It lists by label, newest first:
`gmail.metadata` cannot search, so there is no `q=after:`. Keys make the overlap harmless. `beacon run` reports the
gap and the log has `history_gap`: a change in the gap to mail no longer in the inbox (archived or deleted), or a
star on mail older than 14 days, is not recovered. The re-read lists at most 5000 messages per label; past that it
stops, `run` says so, and the log has `cut` 1. The pass still moves to the new history id: failing it would stop every
later pass at the same place. The history id has no lookback, so a thread the history names is skipped only when
it is gone (`404`, deleted since). Any other failure reading it, a `403` included, fails the pass and the cursor
stays. A body is optional: a message that will not give up its body (`404`, or a permission `403`) is sent without
it; a rate limit or a refused grant there still fails the pass.

**First run.** The latest message of each inbox thread with a message in the last 14 days, and starred messages from
the last 14 days. Both lists are newest first, and paging stops at the first older entry. The inbox list is ordered by a thread's
newest message of any kind, so paging stops at a thread whose newest message, drafts included, is older; a thread
with a recent draft and older mail is skipped and paging goes on. Each list reads at most 5000 entries. Past that
it stops, `beacon run` says so, and the log has `read_cut`: older mail in the 14 days is not read unless it changes. The cursor is the profile's
history id, read before listing, so nothing that arrives during the pass is missed.

**Items.** `received` (a new message from someone else), `sent` (the person's own message: the `SENT` label, and
only that), `starred` (`STARRED` added). A From header is the sender's claim: mail that says it is from the person
and has no `SENT` label is `received`, not own, and does not close an ask. A draft that is sent may gain `SENT` without arriving as a new
message; that is `sent` too, with the same key. Drafts, spam, trash and chats are skipped. Other label changes,
reads and archiving are not events (DEC-18).

| Item | Event title |
| --- | --- |
| `received` | New message, or Reply received when the thread has an earlier message |
| `sent` | Sent, or Replied when the thread has an earlier message |
| `starred` | Starred |

**Subject.** `gmail:<person>:thread:<thread id>`. **Key.** `gmail:<thread id>:<message id>` for a message and
`gmail:<thread id>:starred:<message id>` for a star. The version is the message id.

**Addressed.** The person's address is in To (not Cc). **Own.** `sent`, with `by`. `starred` has no actor and is not
own: starring is a note to oneself, not an answer, and must not close an ask.

**Fields.** `from` (the address), `to` (addresses, comma-separated), `subject` (the thread's subject, its first
message's), `date` (RFC 3339, UTC), `labels` (label names, sorted, comma-separated; system labels by id, such as
`INBOX` and `CATEGORY_PROMOTIONS`), `link` (`https://mail.google.com/mail/u/0/#all/<thread id>`), `body`
(the `text/plain` part, only with `gmail.readonly` and a rule that lists `body`). `subject` is the thread title when
a rule lists it and lists neither `title` nor `summary`.

**State.** Over the whole thread: `unread` and `starred` (`true` or `false`), `labels`, `last_from` (the newest
message's sender).

### Calendar

**Each pass.** Events on the primary calendar, expanded to single instances, that end after now and start within 30
days, updated since 15 minutes before the cursor, with deleted events. An event Beacon knows that moved into the
past or beyond 30 days is not in that list, so the pass also reads events updated since then with no time window and
not expanded (a moved instance of a recurring event is its own event there), and compares the ones Beacon knows.
That move is `changed` (or `cancelled`), not a silence. Every list includes deleted events, so a cancel is seen
as a cancel. If Google refuses that `updatedMin` as too old (`410`), the whole window is read again; the
fingerprints keep unchanged events quiet. Then an event the cursor places in the window that the re-read did not
return is read by id: gone (`404`, `410`) or cancelled is `cancelled` (key version `gone` when Google returns
nothing), and moved is `changed`. One the grant can no longer read (a permission `403`) is dropped from the cursor
rather than read again on every full re-read.

**Cursor.** The first server `Date` of the pass, as above, and for each event in the window a fingerprint: a hash of
its summary, location, start, end and status, a hash of its start and end, the person's response, and whether it is
cancelled. The cursor holds hashes and response values, not event text. An entry is dropped a day after its event
ends. A change of state is a change of the fingerprint or the response, so another attendee's response, which bumps
the event's `updated` time, is not an event.

**First run.** Events in the next 7 days: `response_needed` for an invitation the person has not answered, and
`upcoming` for the rest. The rest of the 30-day window is fingerprinted and not emitted.

**Items.** One per event per pass, in this order:

| Item | When | Event title |
| --- | --- | --- |
| `cancelled` | The event was cancelled. | Cancelled |
| `response_needed` | Someone else's event, new to Beacon or with the person's response back to `needsAction`. Addressed. | Response needed |
| `responded` | The person's response to someone else's event changed to accepted, declined or tentative. Own. | Responded: accepted (declined, tentative) |
| `created` | An event Beacon has not seen, created since the window began. | Created |
| `changed` | The summary, location, start, end or status changed. An event that enters the window already old, unseen, is `changed` once. | Rescheduled when the start or end changed, else Changed |
| `upcoming` | First run only. | Upcoming |

Calendar does not say who changed an event. An own act closes the person's asks, so an item is own only when it is
provably theirs: `responded`, and `created` for an event whose creator is the person's account. A change or a
cancel of an event the person organises is not own (a co-organiser or a delegate can make it), and has no actor.
Otherwise the actor is the organizer. A `changed` event the person has not answered is addressed. The title never
includes the summary, a name or the description.

**Subject.** `calendar:<person>:<event id>`. **Key.** `calendar:<event id>:<item>:<etag>`; the version is the
event's etag.

**Fields.** `summary`, `start` and `end` (RFC 3339 in UTC, or a date for an all-day event), `organizer` (address),
`attendees` (how many are listed), `response` (the person's: `needsAction`, `accepted`, `declined`, `tentative`;
`accepted` when they organise it and are not listed), `link`, `body` (the description, only when a rule lists it).

**State.** `status` (`confirmed`, `tentative`, `cancelled`), `start`, `end`, `response`.

**Due.** `due_at` is the start; an all-day event's is midnight in the calendar's time zone (DEC-21).

### Drive

**Cursor.** The first server `Date` of the pass, as above.

**Comments.** A comment does not reliably change a file's `modifiedTime`, so each pass reads the comments of every
recently active Doc, Sheet and Slides file the person can read (`corpora=user`, shared drives included): those
modified since 15 minutes before the cursor, and those the person opened in the last 14 days (`viewedByMeTime`, the
200 most recent). For each, `comments.list` with `startModifiedTime` at the same time says which comments changed.
A mention on a file the person has neither opened in 14 days nor seen modified is not found. A comment or reply created since then yields one item when it concerns the person:

| Item | When | Event title |
| --- | --- | --- |
| `assigned` | Someone else assigned it to the person. Wins over `mentioned`. Addressed. | Assigned |
| `mentioned` | Someone else mentioned the person. Addressed. | Mentioned |
| `commented` | The person's own comment or reply. Own. | Commented, or Replied |
| `resolved` | The person's own reply that resolves the comment. Own. | Resolved |

Drive does not return a commenter's address, so `author` is a display name and another person's item has no actor.
A file or comment list that cannot be read (`404`, or a permission `403`) is skipped.

**Meeting notes.** Events on the primary calendar that ended since the window began, or ended in the last 7 days
and were updated since the window began, that the person organised or accepted, with a Google Doc attachment whose title contains
`Notes by Gemini` (English titles only). One `notes_created` item per notes file, titled Meeting notes created,
with a link. The file is read with `files.get` for its name, link and creation time. Its text is read
(`files.export` as plain text, the first 32 KiB) only when a rule lists `body`. "Attended" is "organised or
accepted": Google does not say who joined. Notes are usually attached while the meeting runs, which is the event's
last update, so a pass reads the meetings that ended since the last one without an `updatedMin`, and a meeting with
notes is found the pass after it ends.

**First run.** The last 7 days: unresolved comments by others that mention or are assigned to the person, and
meeting notes. No own acts.

**Subject.** `drive:<person>:<file id>`, for comments and for notes. **Key.**
`drive:<file id>:comment:<comment id>`, `drive:<file id>:reply:<comment id>:<reply id>`, `drive:notes:<file id>`.
The version is the comment, reply or file id.

**Fields.** `title` (file name), `link` (a comment's links to the comment), `file_type` (`document`,
`spreadsheet`, `presentation`), `author`, `meeting` and `meeting_start` (notes), `body` (comment or reply text, or
notes text; only when a rule lists it). **State.** For comments: `resolved` (`true` or `false`), `assignee` (address).

DEC-29 (one authoritative event per act) needs Signal phase 12 and is not built. Beacon records every change it is
sent.

## Slack

`kind: slack`, with `workspace` and `user`, and optional `channels`. A user token (`xoxp-…`) the person provides, from
a Slack app they create in their own workspace (DEC-2). Beacon calls only read methods with it.

**The app and the token.** At [api.slack.com/apps](https://api.slack.com/apps), create an app in the workspace (not
distributed), add the *User Token Scopes* below, and install it. Copy the *User OAuth Token* from *OAuth &
Permissions*, then run `beacon source add slack` and paste it. A bot or app token is refused: search needs a user
token. The token is stored in the keychain as `slack:<workspace id>:<user id>`, the ids `auth.test` reports.

**Scopes.** All user scopes, all read-only:

| Scope | For |
| --- | --- |
| `search:read` | `search.messages`: direct messages, mentions, the person's own messages |
| `channels:history`, `groups:history` | `conversations.replies` and `conversations.history` in public and private channels |
| `im:history`, `mpim:history` | The same in direct and group direct messages |
| `channels:read`, `groups:read` | `conversations.info` on a configured channel: is it private |
| `mpim:read` | `users.conversations`: the group direct messages the person is in |
| `usergroups:read` | `usergroups.list`: the user groups the person is in, so a group mention is a mention |

`auth.test` reports the token's scopes. A token missing one is refused by `source add`, and fails a pass with
`source_unauthorized`. A token that also holds other scopes (say, a write scope from an app the person already had)
is accepted, and `source add` names them; Beacon never calls a method that uses them. `conversations.unreads` is not
used: with a user token it is slow and ignores its filters.

**Host and methods.** `slack.com` only, listed by `beacon egress`. Every request is a `GET` to one of seven methods,
each with only the query keys Beacon sends:

| Method | Keys | Limited to |
| --- | --- | --- |
| `auth.test` | none | |
| `search.messages` | `query`, `count`, `page`, `sort`, `sort_dir` | the three queries below, naming only the token's user |
| `conversations.replies` | `channel`, `ts`, `oldest`, `inclusive`, `limit`, `cursor` | |
| `conversations.history` | `channel`, `oldest`, `latest`, `limit`, `cursor` | configured channels, and group direct messages the person is in |
| `conversations.info` | `channel` | configured channels |
| `users.conversations` | `types` (`mpim` only), `exclude_archived`, `limit`, `cursor` | |
| `usergroups.list` | `include_users` | |

Anything else is refused before it is sent (`source_scope`), so a request cannot post, mark read, list users or
channels, or read the history of a channel the config does not name.

**Rate limits.** Slack limits each method per workspace by tier. Beacon spaces its own calls: `search.messages` and
`usergroups.list` (Tier 2, 20 a minute) at most one every 3 seconds, the `conversations.*` methods and
`users.conversations` (Tier 3, 50 a minute for an app built for one workspace) at most one every 1.2 seconds. A `429`
honours `Retry-After` as above; `"error":"ratelimited"` ends the pass. An app distributed to other workspaces outside
the Slack Marketplace gets one `conversations.*` call a minute; create the app for your own workspace.

**Errors.** `invalid_auth`, `not_authed`, `token_revoked`, `token_expired`, `account_inactive`, `missing_scope`,
`not_allowed_token_type`, `no_permission` are `source_unauthorized`; run `beacon source add slack` again with a valid
token. `channel_not_found`, `thread_not_found`, `not_in_channel` or `is_archived` on one thread drops that thread, and
on a channel or group direct message skips it for the pass and keeps its watermark. Any other error fails the pass,
and the cursor stays.

**Each pass.**

- `auth.test`: the workspace and user must be the config's, and `person.identities.<name>` (`slack:user:<id>`) the
  token's user, or the pass fails with `identity_mismatch`.
- `usergroups.list`, once an hour: the user groups the person is in.
- `search.messages`, newest first, 100 a page, at most 20 pages a query: `is:dm` (one-to-one direct messages,
  including the person's own) and `from:<@USER>` (the person's own messages) over the window; `<@USER>` (mentions)
  over the last 7 days every pass. A mention older than the window that the cursor has not seen is kept: an edit
  added it, or search indexed it late. `after:` takes a date, so Beacon asks from two days before and keeps only
  messages in range. A query that reaches its page limit inside its range marks the pass cut; `run` says so.
- `conversations.info` and `conversations.history` for each configured channel: whether it is private, and its top
  level from its watermark, for review requests, `@here`/`@channel`/`@everyone`, user group mentions and the person's
  own messages.
- `users.conversations` and `conversations.history` for each group direct message the person is in, from its
  watermark: at most 20 a pass.
- `conversations.replies` for each followed thread: a thread the person posted in, or was mentioned in, in the last 7
  days, outside one-to-one direct messages (search reads every message there). A thread starts at the window of the
  pass that followed it, so a reply is not lost if the thread waits for a later pass.
- `conversations.replies` with `inclusive` and the message's own `ts`, for messages that addressed the person, to
  notice a deletion: each at most every 15 minutes, at most 20 a pass, for 7 days.

A thread or group direct message with a message in the last hour is read every pass; a quieter one every 15 minutes;
the longest unread first. Each keeps its own watermark, so one that waits loses nothing. History is returned newest
first: a read that stops at its page limit (2000 messages) keeps its watermark, records the gap below what it read,
and the next pass reads that gap before anything newer. Nothing is skipped and the pass is not cut.

**Cursor.** The first server `Date` of the pass, as above; search reads from 15 minutes before it. Beside it: each
followed thread (its kind, the person's first and latest post or mention there, the newest message read, when it was
read), each configured channel's and group direct message's watermark (and a gap, when a read stopped at its limit),
the mentions seen in the last 7 days, and the messages that addressed the person, with their subjects, for the
deletion check. The cursor holds ids, kinds, subjects and timestamps, no text.

**First run.** The last 7 days: direct and group direct messages, mentions, the person's own messages, replies in
threads they posted or were mentioned in, and the configured channels (DEC-23).

**Items.** One per message. When a message qualifies twice, the first in this table wins.

| Item | When | Addressed | Event title |
| --- | --- | --- | --- |
| `own_message` | The person's message: the API's `user` is the token's user and there is no `bot_id`. Only in a direct or group direct message, a private channel, a thread, or a configured channel. Own. | no | Sent, or Replied in a thread |
| `mentioned` | Someone else's message with `<@USER>`, a user group the person is in, or, in a configured channel only, `@here`, `@channel` or `@everyone`. | yes | Mentioned |
| `dm_received` | Someone else's message in a one-to-one or group direct message (`channel_type` says which). | yes | Direct message |
| `thread_reply` | Someone else's reply in a followed thread, after the person's first post or mention there. | no | Thread reply |
| `review_request` | Someone else's top-level message in a configured channel with a pull request or merge request link and review wording (`review`, `look`, `approve`, `approval`, `PTAL`, `eyes`). | no | Review requested |
| `message_deleted` | A message that was `mentioned` or `dm_received` no longer exists. On that message's subject, keyed `slack:<channel>:<ts>:deleted`, stamped with the pass's `Date`. A `close_by_others` rule makes it close the ask. | no | Deleted |

Joins, edits, other message subtypes and reactions are not events (DEC-18); an edit that adds a mention is found by
the mention search. A top-level message of the person's in a public channel that is not configured is not an item:
it answers no ask Beacon recorded. The thread it starts is still followed. An app's message has a `bot_id`: its
`author` is that id and its actor `slack:bot:<id>`, and it is never own. The title never includes message text or a
name.

**Answered (DEC-19, DEC-20).** An own message in a thread is on the thread's subject, so it closes the open asks
there. A one-to-one direct message has no thread for its top-level messages: each is its own subject. An own
top-level message there sets `close_siblings_before` to its own `ts`, so it also closes the open asks that opened the
conversation's other subjects before it: earlier top-level messages, not a question inside a thread there and not a
message posted after it, even one search indexed late ([`events.md`](events.md#items)). An own top-level message in a
channel or a group direct message closes nothing else: it may be about something else. Someone else's answer closes
an ask only through a `close_by_others` rule, for example on `thread_reply` in a thread the person was mentioned in.

**Subject (DEC-10).** In a public channel, `slack:<channel>:<thread ts>`: the thread's root `ts`, or the message's own
when it is not in a thread. In a one-to-one or group direct message, or a private channel,
`slack:dm:<person>:<channel>:<thread ts>`, the person being `person.handle` without `@`. Private channels use the same
stem as direct messages, so one restriction (`slack:dm:*`) keeps them all private. A channel is public only when a
read says so: `is_private: false` on a search match, or `conversations.info` for a configured channel. A channel no
read described, or any id that is not `C…`, is treated as private. **Key.** `slack:<channel>:<ts>`; the version is the
message `ts`.

**Personal subjects.** `slack:dm:` is a fixed personal stem. Direct messages, group direct messages and private
channels are held until Signal restricts `slack:dm:*`, exactly as Gmail's are held until `gmail:*` is ([Google](#google),
"Nothing personal is sent unless Signal keeps it private"). Public channel items are not personal and are not held.

**Fields.** `channel` (id), `channel_type` (`im`, `mpim`, `private` or `channel`), `ts`, `thread_ts` (the root, when
the message is a reply), `author` (user or bot id), `permalink` (Slack's, or built from the workspace address
`auth.test` reports), `body` (the message text, only when a rule lists `body`). No state fields: Slack has no object
state Beacon reads.

**Text is read on the machine.** Slack returns a message's text with the message, whatever the scope. Beacon reads it
to find mentions, review wording and links, and it leaves only as `body`, when a rule lists it.

**References (DEC-17).** A GitHub pull request link becomes `github:pr:<owner>/<repo>#<n>`, and a Jira link
`https://<site>.atlassian.net/browse/KEY-1` becomes `jira:KEY-1`. They come from the text, and leave only when a rule
lists `references`; the starter rules list them only for one-to-one direct messages.

**Identity.** `person.identities.<name>` is `slack:user:<user id>`. Own messages carry it as `by`. Others are
`slack:user:<id>` in `metadata.actor` when a rule lists `actor`. Display names need `users:read` and are not read.
