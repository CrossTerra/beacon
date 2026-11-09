# Sources

A source yields items. Phase 2 has GitHub and Jira, beside the [fixture](configuration.md#fixture-source). Each has a
fixed field list and a fixed state field list. A rule names what leaves; anything else stays.

Credentials are in the keychain, under the service `beacon`, account `source:<name>`. They are read at the start of
each pass and set only as an Authorization header. They are never written to the config, the log, or the outbox.

## Cursor

`Collect` returns the items and an opaque next cursor. The runner stores it in `cursors.json` only after every item
of that pass has been sent to the outbox, held, or dropped, and only on a send pass. A crash before that collects
the same items again; keys make that harmless (DEC-9). A dry run reads the cursor and does not write it. A rate
limit or a failed collect leaves the cursor where it is. An item that fails validation, or that a rule matched but
could not be projected, is dropped and logged by count, with a fixed reason code and no item content. That does not
freeze the source: the cursor still moves.

The next cursor is the first `Date` header of the pass, or the stored cursor if that is later. It is never the
local clock, and it never moves backwards. A later `Date` in the same pass is ignored, so a pass longer than the
lookback does not skip events that arrived while it was running. A `Date` more than an hour ahead of the local
clock fails the pass. With no `Date`, the cursor is the newest source event time, not a local-clock stamp on a
push or a check change. If there is no `Date` and no such event time, the cursor is not moved. A pass reads from
the committed cursor minus 15 minutes, so indexing lag and clock skew land in an overlap.

A source with no cursor reads open state, not history (DEC-23). That first pass is a dry run until
`beacon accept-first`. See [commands](commands.md#beacon-run).

## Seed

`beacon seed` imports lines of `<subject> never|send`. A `never` line drops that subject before the rules file, so
an item already dismissed stays dismissed (DEC-24). A `send` line is recorded and does not force a send: the rules
file still decides, and the default remains deny. The format is in [commands](commands.md#beacon-seed).

## Rate limits

Each source honours `Retry-After` and, when present, `X-RateLimit-Remaining` and `X-RateLimit-Reset`. A wait of at
most two minutes is slept, then the request is retried. A longer wait ends the pass. The cursor is not moved, so
the next pass resumes from it.

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
`own` is set when the person caused the change. `checks_changed` has no actor and `own` is false: a check finishing
is not the person's act, and must not close their asks. `pushed` is not filtered by the git commit's author time, which is often older than the push. It is emitted for a
commit after the person's last review whose committer time, or a force-push's event time, is inside the lookback
window. A push older than the first run is not emitted on the first incremental pass (DEC-23). The key is the sha,
so a later pass that sees it again does not send twice. A key longer than 200 characters is shortened with a hash
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
person caused the change.

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

DEC-29 (one authoritative event per act) needs Signal phase 12 and is not part of Phase 2. Beacon records every
change it is sent.
