# Phase 2 — GitHub and Jira

Status: complete. What was built is described in [`docs/sources.md`](../docs/sources.md),
[`docs/commands.md`](../docs/commands.md) and [`docs/configuration.md`](../docs/configuration.md).

## Goal

The first two real sources, at parity with what a person's sweep needs from them today, with every item shaped by
DEC-9, DEC-10, DEC-17 to DEC-21 and DEC-28, and with a starter rules file that sends what is actionable and nothing
else. At the end, Beacon can run in dry-run against a real person's GitHub and Jira and its report reads like that
person's queue.

## Scope

### Source framework

- **Cursor commit.** A source's cursor moves only after the runner has decided every item of a pass (sent to the
  outbox, held, or dropped). Add a commit step to the `Source` interface (`Collect` returns items and an opaque next
  cursor; the runner stores it in the state directory after deciding). A crash between collect and commit collects
  the same items again, and keys make that harmless.
- **Credentials.** `beacon source add <kind>` stores the source's token in the keychain under the service `beacon`.
  Tokens are read per pass and never logged.
- **Rate limits.** Each source honours its API's rate-limit headers and backs off; a limited pass ends early and
  resumes from the committed cursor.
- **First run (DEC-23).** A source with no cursor reads open state, not history. Its first pass is always a dry run
  (`beacon run --first` prints a summary; `beacon accept-first` turns on sending for that source).
- **Seed file (DEC-24).** `beacon seed <file>`: lines of `<source id> never|send`. Items whose subject matches a
  `never` line are dropped by a built-in rule that runs before the rules file. The format is documented.

### GitHub (`kind: github`)

- **Auth:** a fine-grained or classic token in the keychain; `beacon source add github --from-gh` may import the
  token `gh auth token` prints, with the person's confirmation.
- **Hosts:** `api.github.com` (and a configurable GitHub Enterprise host).
- **Scope (config):** repositories by `owner/name` or `owner/*`. Nothing outside them is read.
- **What it reads each pass:**
  - Pull requests the person is involved in (author, assignee, reviewer, review requested, mentioned, commenter)
    updated since the cursor, through the search API, then per pull request its timeline since the cursor (review
    requests, reviews with state and commit, comments, head pushes, ready-for-review, merge and close).
  - Check suite conclusions on the person's own open pull requests, when they change.
- **Items (DEC-18):** `opened`, `ready`, `review_requested`, `review_request_removed`, `reviewed`
  (field `review_state`), `commented`, `pushed` (only on pull requests the person reviewed), `checks_changed`
  (own pull requests only), `merged`, `closed`. A merge yields `merged` only, never also `closed`; `closed` is a
  close without a merge. The person's own reviews, comments, merges and closes have `own: true`.
- **Subject:** `github:pr:<owner>/<repo>#<n>`. **Key:** `github:<owner>/<repo>#<n>:<change id>`, where the change id
  is the timeline item's or review's id, or `checks:<head sha>:<conclusion>`.
- **Fields:** `repo`, `number`, `title`, `url`, `author`, `review_state`, `body` (comment or review text),
  `base`, `head_ref`. **State fields:** `state`, `draft`, `head_sha`, `review_decision`, `mergeable`, `checks`
  (`pass`, `fail`, `pending`, `none`), `requested_reviewers`.
- **Addressed:** a review request naming the person, or a mention of the person's login.
- **References (DEC-17):** issue keys matching configured patterns (`[A-Z][A-Z0-9]+-\d+`, limited to configured
  projects) in the title or head branch become `jira:<KEY>`.

### Jira (`kind: jira`)

- **Auth:** Atlassian account email and API token in the keychain (basic auth over https).
- **Hosts:** the configured site (`<site>.atlassian.net`).
- **Scope (config):** project keys. Nothing outside them is read.
- **What it reads each pass:** issues in the configured projects where the person is assignee, reporter, watcher
  or mentioned, updated since the cursor (JQL on `updated`, in the site's time zone), with the changelog and
  comments since the cursor.
- **Items (DEC-18):** `assigned`, `unassigned`, `transitioned` (fields `from_status`, `to_status`), `commented`,
  `mentioned`, `due_changed`. A comment yields one item: `commented`, with `addressed: true` when it mentions the
  person. `mentioned` is only for a mention added to the description or another field, so one change never yields
  two items. The person's own transitions and comments have `own: true`.
- **Addressed:** assignment to the person, a comment or field change that mentions the person, and a transition
  on an issue assigned to the person by someone else.
- **Subject:** `jira:<KEY>`. **Key:** `jira:<KEY>:<changelog id or comment id>`.
- **Fields:** `project`, `key`, `summary`, `url`, `issue_type`, `priority`, `body` (comment text).
  **State fields:** `status`, `assignee`, `resolution`, `due`, `priority`.
- **Due:** the issue's due date becomes `due_at` (DEC-21).

### Starter rules

`examples/rules.yaml`: review requests to the person and comments addressed to them as `send` with `ask: me`;
reviews, merges and check changes on the person's own pull requests as `send`; the person's own acts as `send`
(they close asks); Jira assignment, mentions and transitions on the person's issues as `send`; everything else
`never`. Bodies are not listed by default. Document each rule.

## Out of scope

Google, Slack, the review queue UI, the classifier, GitHub issues (pull requests only), webhooks.

## Tests

- Each source against recorded fixtures served by `httptest`, covering every item type, own acts, mentions,
  pagination, rate-limit backoff, and a cursor that only moves after commit.
- Keys are stable across passes and across a restart; a re-collected change yields the same key and is not sent
  twice.
- No request goes outside the configured repositories, projects and hosts (egress guard plus a scope check).
- The starter rules against the fixtures: the expected items are sent, the rest dropped.

## Exit gate

All gates green. Against fixtures, both sources produce the documented items, subjects, keys, fields and state
snapshots. A live dry run against a real account (read-only tokens) completes, its report is checked by the person,
and nothing is sent.
