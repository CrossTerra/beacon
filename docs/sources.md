# Sources

A source yields items. Phase 2 has GitHub and Jira, beside the [fixture](configuration.md#fixture-source). Each has a
fixed field list and a fixed state field list. A rule names what leaves; anything else stays.

Credentials are in the keychain, under the service `beacon`, account `source:<name>`. They are read at the start of
each pass and set only as an Authorization header. They are never written to the config, the log, or the outbox.

## Cursor

`Collect` returns the items and an opaque next cursor. The runner stores it in `cursors.json` only after every item
of that pass has been sent to the outbox, held, or dropped, and only on a send pass. A crash before that collects
the same items again; keys make that harmless (DEC-9). A dry run reads the cursor and does not write it. A rate
limit, a failed collect, an invalid item, or an item that could not be projected leaves the cursor where it is.

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
`gh auth token` prints, after confirmation (`--yes` when input is not a terminal).

**Host.** `api.github.com`, or `host:` for GitHub Enterprise (`https://<host>/api/v3`). Listed by `beacon egress`.

**Scope.** `repos`: `owner/name` or `owner/*`. Nothing outside them is requested. `owner/*` is searched as the
owner's user or organization, and a result from any other repository is not fetched.

**Each pass.** Pull requests in the configured repositories updated since the cursor, through the search API, one
repository qualifier per query (an OR of `repo:` qualifiers matches nothing), then each pull request's timeline
since the cursor. The search does not use a login qualifier: some tokens can read the repositories and still cannot
search a user. A pull request the person is not involved in (author, assignee, reviewer, review requested,
mentioned, or commenter) yields no items. Own open pull requests are also read for check suites, because a check
conclusion does not change the pull request's `updated` time. A pull request or issue the token cannot read (404
or 403) is skipped; it does not freeze the cursor.
Check-suite conclusions on the person's own open pull requests, when they change. The token needs permission to
read those repositories and their checks.

**First run.** Open pull requests in the configured repositories, kept when the person wrote them or is a
requested reviewer. Not their history: no old comments. The current review requests and, on the person's own pull
requests, the current check conclusion.

**Items.** `opened`, `ready`, `review_requested`, `review_request_removed`, `reviewed` (field `review_state`),
`commented`, `pushed` (only on a pull request the person has reviewed), `checks_changed` (own open pull requests
only), `merged`, `closed`. A merge yields `merged` only, never also `closed`. `closed` is a close without a merge.
`own` is set when the person caused the change.

**Subject.** `github:pr:<owner>/<repo>#<n>`. **Key.** `github:<owner>/<repo>#<n>:<change id>`. The change id is the
timeline event's or review's or comment's id, the pull request's id for `opened` (the open has no timeline event),
the commit sha for `pushed` (a committed event has no numeric id), or `checks:<head sha>:<conclusion>`.

**Addressed.** A review request naming the person, or `@login` in a comment or review. `@login` does not match a
longer login.

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
`beacon source add jira --site SITE` reads them from the terminal and does not print the token.

**Host.** `<site>.atlassian.net`, from the site label. A site is not a hostname, so the config cannot name an
arbitrary host. Listed by `beacon egress`.

**Scope.** `projects`: project keys. Nothing outside them is requested. A search result from another project is
not fetched.

**Each pass.** Issues in those projects where the person is assignee, reporter, watcher, or mentioned, updated
since the cursor. The JQL date is formatted in the account's timezone, which is how Jira evaluates `updated` for
this token. Then the changelog and comments since the cursor.

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
