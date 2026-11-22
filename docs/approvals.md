# Approvals: the proposal socket, the vault and Signal's contract

Plan 13.00 has Bosun propose an exact action to Beacon.app over a local socket. The person approves it in the app
window, and Beacon carries it out with its own credentials. This page describes the parts that exist so far.

**Status.** The code is in `mac/Sources/BeaconCore`, with the window in `mac/Sources/Beacon`. When it runs as
`dev.beacon.app`, signed with a certificate, the app opens its vault key, its journal and the socket. An unsigned or
ad-hoc build opens none of them. `github.merge` is enabled once a GitHub account is added in Settings
([merges](#merges)). `slack.post` and `agent.run` have no handler yet. Once Signal is added in Settings, the
outbox goes to Signal's record API ([delivery](#delivery-to-signal)).

## The contract with Signal

Signal publishes what it accepts from the app in `docs/approval-app.md` and three fixture files. Beacon builds to
them and tests against copies in `mac/Tests/BeaconCoreTests/Fixtures/approval-app`, taken from Signal at
`7a20b1d2cd11c99b23c17194705caa1a2295855f`. Replace the copies when Signal changes them.

- **Canonical JSON and hashes.** Keys sorted by UTF-16 code unit, no insignificant whitespace, integers only
  (`1.0`, `1e0`, and integers beyond 2^53 − 1 are refused), strings escaped as `JSON.stringify` does. A hash is
  `sha256:` and the hex SHA-256 of those bytes. Duplicate keys, lone surrogates and invalid UTF-8 are refused.
- **The salt.** Each proposal draws 128 random bits (32 hex characters). The salt is inside every hashed object:
  `params_hash` is the hash of `{ type, params, salt }`, and `approved_hash` of
  `{ id, type, params, origin, account, destination, salt }`. The hashes go to Signal in plain text and the salt
  sealed, so someone who sees only a hash cannot confirm a guess of short parameters such as a Slack text.
- **Ids.** `apr_…` for a proposal and `jrn_…` for a journal: a UUIDv7 as 26 lowercase Crockford base32
  characters, so ids sort by time.
- **Parameters.** `github.merge`, `slack.post` and `agent.run`, with exactly Signal's fields and patterns. A pattern
  must match the whole value, as JavaScript's `^…$` does: a value ending in a line terminator is refused (ICU's
  `$` would accept it). Extra fields are refused, and a refusal names the field without echoing the value.

## The proposal socket

The app's `ProposalServer` listens on `~/Library/Application Support/beacon/run/act.sock`. The `run` directory
must be `0700` and owned by the person, and the socket is `0600`. It refuses to start if another listener already
answers there.

**Who may connect.** Each connection is checked before a byte is read. The kernel's audit token for the
connection names the exact peer process. That process must run as the same user and satisfy one of the code
requirements admitted at setup, for example the signed `beacon` CLI's
`identifier "dev.beacon.cli" and certificate leaf = H"…"`. A path or a user id is not an identity. Any other
process is disconnected without a reply.

**Frames.** Each request and reply is one JSON object on one line, at most 256 KiB including the newline, parsed by
the strict parser above. While a connection waits, it may hold at most one more frame and 4 KiB unread. Past that
it is closed with `frame_too_large`, which frees its wait.

| `op` | Request | Reply |
| --- | --- | --- |
| `types` | nothing else | `{ types: [{ type, params }] }`, the enabled types and their JSON Schemas |
| `propose` | `type`, `params`, `reason`, `agent: { name, session? }`, `context?: { subject }` | `{ id, params_hash, status: "pending", version }` |
| `get` | `id` | `{ id, status, version }` |
| `wait` | `id`, `after`, `timeout` (1 to 300 seconds) | the same, once `version` is past `after` or the timeout ends |
| `withdraw` | `id` | the same, while pending; otherwise `not_pending` |
| `quit` | nothing else | `{ status: "quitting" }`, then the app quits as its menu's Quit does; only for the bundled CLI (`scripts/install.sh`) |

The proposals live in the [journal](#the-journal-and-the-outbox). There is no operation to approve, list proposals, read history or see runners. `get`, `wait` and `withdraw` answer
only the client identity that made the proposal. Any other id reads as `not_found`. Clients with the same admitted
identity cannot tell their proposals apart. Each proposal's `proposed` entry records, locally, the client's
executable path and the requirement it satisfied, from the kernel's code reference for the connection. Neither is
sent to Signal. A self-reported agent name or session is recorded as such
(`verified: false`).

**Limits.** At most 32 connections, 16 concurrent waits, 20 pending proposals per client identity, 40 pending in
all and 60 proposals per identity per hour. Past a limit the reply is `busy`. An incomplete frame is dropped with
its connection after 10 seconds, and an idle connection after 60. Errors are `{ error }`, or
`{ error: "invalid", field }`: `invalid_frame`, `frame_too_large`, `type_disabled`, `busy`, `not_found`,
`not_pending`. Preflight reads and their limits arrive with the handlers.

A pending proposal expires after 24 hours.

## Who trusts whom, without manual steps

- **The app admits the bundled CLI.** It reads the designated requirement of `Contents/Helpers/beacon`, signed with
  the same certificate (`identifier "dev.beacon.cli" and certificate leaf = H"…"`), and admits that one client.
- **The CLI trusts the app.** On its first signed launch, the app writes its own designated requirement to
  `~/Library/Application Support/beacon/act.json` (`0600`). On later launches it refuses to open the socket if the
  file names another requirement. For example, after a rebuild signed with another certificate, the menu says so.
  Remove the file to trust the new build.

`beacon act` (see [commands](commands.md#beacon-act)) reads `act.json`. It connects and checks the listener's
audit token against that requirement before it sends anything. A listener with any other signature, or another
user's, receives no bytes. `"socket"` overrides the path.

## The approval window

Beacon's menu-bar item is an icon (a beacon light), filled and followed by the number of pending proposals when any
are waiting. The window opens from the menu's **Approvals…**, with ⌥⌘B, or by opening Beacon.app again from Finder,
Spotlight or `open` while it runs. Its **Settings…** button (⌘,) opens Settings.

The window lists pending proposals, oldest first. The right side names the selected action in Beacon's words:
the account (for a merge, the GitHub login), origin, destination (`main · #147 · CrossTerra/signal`) and the
provider facts Beacon resolved, such as **Checks: 3 passed**, with a link to the pull request, kept apart from the
agent's reason. The message or prompt shows invisible and bidirectional characters as `⟨U+…⟩`. _Show exact
parameters_ shows the exact account (`github:user:{id}`), the destination and params as canonical JSON, and the
approval hash. Once approved, the selected proposal stays shown with its result: _Started…_, **Merged as 18637a4**,
_Not merged: head moved_, or an unknown outcome being read back, with the link to the pull request.

**Approve** (or ⌘↩ in the window) is enabled only once Beacon has resolved the action and frozen a snapshot of
the proposal id, its version, the final params, account, origin, destination and `approved_hash` (13.00-D29).
The control carries that snapshot's token. Selecting another row, changing the model, _Read again_, a withdrawal,
an expiry or Quit clears the snapshot first, so a click or key bound to the old view approves nothing. Approving
consumes the snapshot before the journal commits. A second click, the keystroke racing the click, or a held key
approves nothing more, and the selection does not move to the next row. A proposal's record must still be
pending, unexpired and at the frozen version, or the journal refuses it. **Decline** takes an optional note for the
agent. Only an agent run's model can change; the picker that lists opencode's models arrives with the run
handler.

## The journal and the outbox

`~/Library/Application Support/beacon/app/journal.sqlite` (in a `0700` directory, WAL, `synchronous = FULL`,
`fullfsync` and `checkpoint_fullfsync`) is the local record (13.00-D10). The journal and its `-wal` and `-shm` files
are `0600`: Beacon creates the journal itself before SQLite opens it (SQLite gives the other two the journal's mode),
narrows any of the three an earlier build left wider each time it opens, and refuses one that is a symlink or not
the person's own file. On macOS a plain fsync leaves writes in
the drive's cache, so each commit is flushed with `F_FULLFSYNC`: a start is on disk before anything is sent. The
vault file and `act.json` are written the same way. Each entry and its outbox row commit in one transaction. Triggers refuse any update
or delete of an entry. The journal has one `jrn_…` id. Entries are numbered from 1, and each entry's Signal key
`beacon:{journal}:{seq}` and its replay nonce are fixed when it is written.

| Entry | When |
| --- | --- |
| `proposed` | a `propose` over the socket; a run also gets its `run_…` id here |
| `approved` | a click or keystroke on the frozen snapshot |
| `declined`, `withdrawn` | the person in the window; the proposer over the socket, while pending |
| `expired` | 24 hours pending; ten minutes approved without a start; Quit (`quit_before_start`); relaunch (`restart_before_start`). Written only if the proposal is still in the state that expires, checked under the journal's lock, so the window and the socket can never write two |
| `started` | the durable start, before any final provider read or write; only within ten minutes of the approval, and never after Quit |
| `outcome_unknown` | a lost reply, or a start without a result found at relaunch (`app_restarted`) |
| `executed`, `failed` | a definite result; after an unknown outcome only by read-back |

State is rebuilt from the entries at launch without checking them against today's schemas, which apply to new
requests only: tightening a schema later cannot make an old journal or its outbox unreadable. An entry of a type
this build no longer knows is left out of the window, and its record is still delivered. Recovery then expires every approval without a start and marks every
start without a result unknown, before the window or the socket opens. Nothing dispatches an action again.

Each entry becomes one Signal record ([the contract](#the-contract-with-signal)). Its exact bytes are built once,
on first delivery with the person's `act_…` id, and kept. A merge is about its pull request
(`github:pr:{owner}/{repo}#{n}`), with the repository as the proposal spelled it, never lowercased. From the approval
on, the records use GitHub's own spelling (`full_name`, read in the preflight and kept in the local approval entry)
when it names the same repository, case aside; Signal compares merge subjects case-insensitively. A post or a run is about the person's own subject (`signal:actor:{act_…}`) until
its handler can tell whether the conversation is public. The sender posts only the first undelivered record. It
moves on only after Signal stores it or answers with its identical replay. A lost answer or an unreachable Signal
resends the same bytes later, and a `409` or another refusal stops delivery at that record.

## Delivery to Signal

**Settings.** Settings → Signal has one field, **Beacon setup code**, which the person pastes from Signal's `/beacon`
page. The code is `beacon-setup:v1:` followed by base64url (padded or not) of the JSON `{ "origin", "key" }`: the
Signal origin (`https://host[:port]`) and the key of their app actor (13.00-D25: an integration actor marked
`approval_app` that works for them, with a key of exactly `read` and `write`). A code with another prefix, that is
not base64url of a JSON object, or that lacks a usable origin or key is refused before anything is sent, and the
refusal never repeats the code. Beacon never asks for an `act_…` id. It asks Signal (`/api/me` does not include
`works_for` or `approval_app`, so the actor is read too):

1. `GET /api/me` with the key: the actor must be an integration, and the key's scopes exactly `read` and `write`;
2. `GET /api/actors/{app}`: the actor must be active and marked `approval_app`, and its `works_for` must name
   another actor;
3. `GET /api/actors/{works_for}`: that actor must be an active person (`human`). It is the person, every record's
   `metadata.person`.

Any other answer saves nothing and says why. An outage is never taken as a fact about the key: no answer, a `429`
or a `5xx` says to try again (after `Retry-After` seconds when Signal sends them, at most an hour); `401` and `403`
mean Signal refused the key (rotate it on `/beacon`); a `404` for the app actor or the person means that actor is
missing, asked only after `/api/me` accepted the key; any other status is reported as it is. Once saved, Settings shows
**Connected to {origin} as {name} (@{handle})**.
The key goes only to the vault, bound to the origin and `signal:actor:{act_…}`; the code is not kept.
`settings.json` keeps the origin, the person with their name and handle, and the actor and its handle. Settings
saved by an earlier build, which asked for the person, could name the app actor itself; Beacon sends nothing under
them, and Settings asks for the setup code again.

**Sending.** After every journal entry, and every 30 seconds, the app drains the outbox in journal order with
`POST /api/app-records`. Each request carries the record's exact stored bytes and the app actor's key, sent only
to that origin, with no redirects, proxies or retries.

| Signal answers | Beacon |
| --- | --- |
| `201`, or `200` with `replayed: true` | marks the record delivered and sends the next |
| `401`; `429` (not before `Retry-After`); `403` not about `/subject`; `400` about `/occurred_at` | sends the same record again later |
| no answer; `5xx` (sealing unavailable included) | sends the same bytes again later, and never changes them: Signal may hold them |
| `409` (`idempotency_conflict`, `journal_order`, `transition`, …); `403` about `/subject`; any other `400`; `413`; another `4xx` | stops delivery at that record, with the reason; nothing after it is sent until delivery is resumed |

A record that cannot be built (for example, the person in Settings is not an `act_…` id) also stops delivery at
its sequence, with the reason. After a failed attempt the next waits for `Retry-After`, or a backoff doubling from
30 seconds up to 15 minutes.

**Resume delivery** (in the menu and Settings, and on saving Signal's key or person again) puts stopped records back
to pending and sends them at once, past any backoff. A resume while a send is running is kept: as soon as that send
ends, records it stopped are resumed too and sending starts again, without waiting for its backoff. A record Signal definitely never stored (every answer it got was a definite refusal, or
it was never sent) has its bytes built again with the current settings, so a corrected person takes effect. A record
that ever went without a definite answer (a lost reply, a `5xx`) keeps its exact bytes; a trigger refuses any
change to them.

The menu and Settings show the result: delivered, waiting to be sent again, or stopped at a record and why. Sending a record never executes an action. `beacon egress` lists the Signal origin from the app's
settings.

## Merges

`github.merge` (13.00, _Merge at the write_) is the first handler.

**Settings.** The person pastes a GitHub token and the API origin (default `https://api.github.com`, or a GitHub
Enterprise API origin) into Beacon's Settings window. Beacon calls `GET /user` with it and accepts only a user's
token (`type: User`), never a bot or an app installation. The first account saved becomes the account: a token for
another user is refused until that account is removed. The token goes straight into the [vault](#the-vault), bound
to that origin and `github:user:{id}`. `settings.json` keeps only the origin, the account and the login. The token
is never shown, logged, written elsewhere or passed to a child process.

**Which token.** Settings → GitHub says what to create and has a **Create a token on GitHub** button. It opens, in
the browser, GitHub's page for a new **classic** token with the `repo` scope, for your own account:
`https://github.com/settings/tokens/new?scopes=repo&description=Beacon%20(merges)`. For GitHub Enterprise it opens
`/settings/tokens/new` on the configured origin's web host (`api.{host}` becomes `{host}`). If the organization uses
SAML single sign-on, click **Configure SSO → Authorize** next to the new token, or GitHub refuses it on that
organization's repositories. The token acts as you: GitHub applies your own permissions and branch rules to the merge.

A **fine-grained** token works too (under _Or a fine-grained token_ in Settings): the organization that owns the
repositories as its resource owner, with access to them, and these repository permissions: Metadata read; Contents
read and write; Pull requests read and write; Checks read; Commit statuses read; Administration read (branch
protection's required checks). The organization may need to approve fine-grained tokens before they work; until
then, GitHub refuses the token's reads and Approve stays disabled.

**Before approval** (the preflight that freezes the action), Beacon reads, with that token and nothing else:

1. `GET /user`: the token is still the configured account's;
2. `GET /repos/{owner}/{repo}/pulls/{n}`: open, not merged, base equal to the proposal's, head equal to the full
   proposed SHA, `mergeable` true and `mergeable_state` `clean`, `unstable` or `has_hooks`;
3. the reviews: no reviewer's latest review requests changes;
4. the required checks on the base: contexts from repository rules and classic branch protection, where this
   account can read them;
5. the head's check runs and commit statuses: **every** check reported on the head, required or not, and every
   required one, must have completed and passed (`success`, `neutral` or `skipped` for a check run, `success` for a
   commit status). A repository with no required checks is no exception: a red check that branch protection does not
   require still stops the merge, and a required check nobody reported is pending. As in Signal's executor (#131),
   only the latest run of each check name and app counts: GitHub's run id decides, else the later start, and a
   completed run that never started is the oldest. A re-run that passes clears an older failure. Between apps
   reporting one name, a failure wins, then a run still in progress. The window shows **Checks: N passed**, or which
   failed and which are pending.

Any other answer, or one that cannot be read, leaves Approve disabled with the reason. The frozen action adds the
repository id, the PR and the base (the contract's `destination`), the account and the origin. Preflights are
bounded: two at once, and 120 GitHub reads an hour counted as each request is made, every page and _Read again_
included. A preflight that reaches the limit stops there as `busy`, and a later one makes no read until the hour
allows it.

**After approval**, Beacon records the durable start, then reads all of it again, and finally reads the PR once
more right before the write. A changed repository id, base, head, mergeability, review or check stops the merge as
`failed` (`pre_dispatch`), with nothing sent. Otherwise it
sends one `PUT /repos/{owner}/{repo}/pulls/{n}/merge` with the frozen method and `sha: head`, so GitHub refuses it if
the head moved and enforces its own branch rules for this account.

| GitHub answers | Beacon records |
| --- | --- |
| `200` with the merge commit | `executed`, `confirmed_by: reply`, `native: github:merge:{sha}` |
| `405`, `409`, `403`, `404`, `422`, another `4xx` | `failed` (`reply`): `not_mergeable`, `head_moved`, `forbidden`, `not_found`, `rejected`, `github_{status}` |
| no answer, a `5xx`, a `2xx` without a merge | `outcome_unknown`, then a read-back |

**Read-back** reads the PR, never merges again, and only with the approved account at the approved origin. If
Settings changed since, the outcome stays unknown. Merged by the approved account with the approved head into the
approved base is `executed` (`read_back`). Merged otherwise is `failed` (`merged_otherwise`), and closed unmerged
is `failed` (`not_merged`). Still open is `not_merged` only after two reads, both at least a minute after the
unknown outcome was recorded (after the write was sent). So a merge GitHub is still finishing is never recorded as
failed. Any other result stays unknown and is read again every minute while the app runs, and at
every launch. A start that recovery finds without a result is settled the same way.

Every request goes only to the configured origin. Paths are built from the validated repository, number, branch and
SHA (`.` and `..` refused). Redirects are refused, so the token goes nowhere else. Proxy settings are ignored, an
answer is at most 8 MiB, and nothing is retried. `beacon egress` lists the origin from the app's settings.

## The vault

`Vault` keeps Beacon's service credentials in one file, encrypted with AES-GCM (mode `0600` in a `0700`
directory). Each credential is bound to a service, an HTTPS origin (`https://host[:port]`, nothing else), the
provider's stable account id and, for Slack, the workspace (13.00-D11, D31). A client asks for a credential by
that exact binding, taken from Beacon's settings, never from an agent. A different origin, port, account or
workspace finds nothing. There is no way to list secrets, and a secret prints as `[redacted]`.

The 256-bit key is a generic password item (`dev.beacon.app`, account `vault-key`) in the login Keychain. Its
access list names only the app's own designated requirement.

**Without a vault file**, a `vault-key` item can protect nothing yet. Crashing before the vault was written could
have left it, or another program running as the person could have planted it, knowing its key. So Beacon never
adopts one. It deletes any existing item, makes a fresh key and then writes the vault. If the delete is refused
(another program's access list, or a denied prompt), it opens nothing, and the menu says **Not opening the vault: a
Keychain vault key from before the vault could not be removed**. Remove the item in Keychain Access (or with
`security delete-generic-password -s dev.beacon.app -a vault-key`) and relaunch.

**With a vault file**, the app only reads the
existing item. If that item is gone, the vault is unreadable and the menu says so; nothing replaces the key over an
existing vault. Before opening any keychain, the code checks that the running process is `dev.beacon.app` signed
with a certificate. An ad-hoc build, a test host or any other program is refused first. Tests use fixture keys
and fixture secrets. Nothing in this repository's tests or CI opens a keychain.

## What the files do not protect

The journal and the outbox hold prompts, messages, notes and parameters in plain text: plan 13.00 cut their
encryption. "Append-only" describes how Beacon writes them. Any process running as the person can read the
files, and could edit, truncate or roll them back without Beacon noticing. The vault's Keychain protection
covers only service credentials.
