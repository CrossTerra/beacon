# Approvals: the proposal socket, the vault and Signal's contract

Plan 13.00 has Bosun propose an exact action to Beacon.app over a local socket. The person approves it in the app
window, and Beacon carries it out with its own credentials. This page describes the parts that exist so far.

**Status.** The code is in `mac/Sources/BeaconCore`, with the window in `mac/Sources/Beacon`. When it runs as
`dev.beacon.app`, signed with a certificate, the app opens its vault key, its journal and the socket. An unsigned or
ad-hoc build opens none of them. `github.merge` is enabled once a GitHub account is added in Settings
([merges](#merges)). `slack.post` and `agent.run` have no handler yet. The outbox is built and tested against a
fake Signal, but the app does not send it yet: it has no Signal key or person id until the app actor is set up.

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

The window lists pending proposals, oldest first. The right side names the selected action in Beacon's words:
the account, origin, destination and provider facts Beacon resolved, kept apart from the agent's reason. The
message or prompt shows invisible and bidirectional characters as `⟨U+…⟩`, and _Show exact parameters_ shows the
canonical JSON.

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
`fullfsync` and `checkpoint_fullfsync`) is the local record (13.00-D10). On macOS a plain fsync leaves writes in
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
(`github:pr:{owner}/{repo}#{n}`). A post or a run is about the person's own subject (`signal:actor:{act_…}`) until
its handler can tell whether the conversation is public. The sender posts only the first undelivered record. It
moves on only after Signal stores it or answers with its identical replay. A lost answer or an unreachable Signal
resends the same bytes later, and a `409` or another refusal stops delivery at that record.

## Merges

`github.merge` (13.00, _Merge at the write_) is the first handler.

**Settings.** The person pastes a GitHub token and the API origin (default `https://api.github.com`, or a GitHub
Enterprise API origin) into Beacon's Settings window. Beacon calls `GET /user` with it and accepts only a user's
token (`type: User`), never a bot or an app installation. The first account saved becomes the account: a token for
another user is refused until that account is removed. The token goes straight into the [vault](#the-vault), bound
to that origin and `github:user:{id}`. `settings.json` keeps only the origin, the account and the login. The token
is never shown, logged, written elsewhere or passed to a child process.

**Before approval** (the preflight that freezes the action), Beacon reads, with that token and nothing else:

1. `GET /user`: the token is still the configured account's;
2. `GET /repos/{owner}/{repo}/pulls/{n}`: open, not merged, base equal to the proposal's, head equal to the full
   proposed SHA, `mergeable` true and `mergeable_state` `clean`, `unstable` or `has_hooks`;
3. the reviews: no reviewer's latest review requests changes;
4. the required checks on the base: contexts from repository rules and classic branch protection, where this
   account can read them;
5. the head's check runs and commit statuses: every required context completed successfully (or neutral or
   skipped). A context that both passed and failed is not green.

Any other answer, or one that cannot be read, leaves Approve disabled with the reason. The frozen action adds the
repository id, the PR and the base (the contract's `destination`), the account and the origin. Preflights are
bounded: two at once and 120 GitHub reads an hour, after which the answer is `busy` and nothing is read.

**After approval**, Beacon records the durable start, then reads all of it again. A changed repository id, base,
head, mergeability, review or check stops the merge as `failed` (`pre_dispatch`), with nothing sent. Otherwise it
sends one `PUT /repos/{owner}/{repo}/pulls/{n}/merge` with the frozen method and `sha: head`, so GitHub refuses it if
the head moved and enforces its own branch rules for this account.

| GitHub answers | Beacon records |
| --- | --- |
| `200` with the merge commit | `executed`, `confirmed_by: reply`, `native: github:merge:{sha}` |
| `405`, `409`, `403`, `404`, `422`, another `4xx` | `failed` (`reply`): `not_mergeable`, `head_moved`, `forbidden`, `not_found`, `rejected`, `github_{status}` |
| no answer, a `5xx`, a `2xx` without a merge | `outcome_unknown`, then a read-back |

**Read-back** reads the PR, never merges again. Merged by this account with the approved head into the approved
base is `executed` (`read_back`). Merged otherwise is `failed` (`merged_otherwise`), and closed unmerged is `failed`
(`not_merged`). Still open is `not_merged` only a minute after the start, so a merge GitHub is still finishing is
never recorded as failed. Any other result stays unknown and is read again every minute while the app runs, and at
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
