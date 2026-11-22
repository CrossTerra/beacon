# Approvals: the proposal socket, the vault and Signal's contract

Plan 13.00 has Bosun propose an exact action to Beacon.app over a local socket. The person approves it in the app
window, and Beacon carries it out with its own credentials. This page describes the parts that exist so far.

**Status.** The code is in `mac/Sources/BeaconCore`, with the window in `mac/Sources/Beacon`. When it runs as
`dev.beacon.app`, signed with a certificate, the app opens its vault key, its journal and the socket. An unsigned or
ad-hoc build opens none of them. No action type is enabled, and nothing resolves an action, so nothing can be
approved or executed. The handlers arrive in plan 13.00 part 3. The outbox is built and tested against a fake
Signal, but the app does not send it yet: it has no Signal key or person id until the app actor is set up.

## The contract with Signal

Signal publishes what it accepts from the app in `docs/approval-app.md` and three fixture files. Beacon builds to
them and tests against copies in `mac/Tests/BeaconCoreTests/Fixtures/approval-app`, taken from Signal at
`0702e09ab2ba09e51d731972aaf8a47a1afca250`. Replace the copies when Signal changes them.

- **Canonical JSON and hashes.** Keys sorted by UTF-16 code unit, no insignificant whitespace, integers only
  (`1.0`, `1e0`, and integers beyond 2^53 − 1 are refused), strings escaped as `JSON.stringify` does. A hash is
  `sha256:` and the hex SHA-256 of those bytes. Duplicate keys, lone surrogates and invalid UTF-8 are refused.
- **The salt.** Each proposal draws 128 random bits (32 hex characters). The salt is inside every hashed object:
  `params_hash` is the hash of `{ type, params, salt }`, and `approved_hash` of
  `{ id, type, params, origin, account, destination, salt }`. The hashes go to Signal in plain text and the salt
  sealed, so someone who sees only a hash cannot confirm a guess of short parameters such as a Slack text.
- **Ids.** `apr_…` for a proposal and `jrn_…` for a journal: a UUIDv7 as 26 lowercase Crockford base32
  characters, so ids sort by time.
- **Parameters.** `github.merge`, `slack.post` and `agent.run`, with exactly Signal's fields and patterns. Extra
  fields are refused, and a refusal names the field without echoing the value.

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
the strict parser above.

| `op` | Request | Reply |
| --- | --- | --- |
| `types` | nothing else | `{ types: [{ type, params }] }`, the enabled types and their JSON Schemas |
| `propose` | `type`, `params`, `reason`, `agent: { name, session? }`, `context?: { subject }` | `{ id, params_hash, status: "pending", version }` |
| `get` | `id` | `{ id, status, version }` |
| `wait` | `id`, `after`, `timeout` (1 to 300 seconds) | the same, once `version` is past `after` or the timeout ends |
| `withdraw` | `id` | the same, while pending; otherwise `not_pending` |

The proposals live in the [journal](#the-journal-and-the-outbox). There is no operation to approve, list proposals, read history or see runners. `get`, `wait` and `withdraw` answer
only the client identity that made the proposal. Any other id reads as `not_found`. Clients with the same admitted
identity cannot tell their proposals apart; a self-reported agent name or session is recorded as such
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

`~/Library/Application Support/beacon/app/journal.sqlite` (in a `0700` directory, WAL, `synchronous = FULL`) is
the local record (13.00-D10). Each entry and its outbox row commit in one transaction. Triggers refuse any update
or delete of an entry. The journal has one `jrn_…` id. Entries are numbered from 1, and each entry's Signal key
`beacon:{journal}:{seq}` and its replay nonce are fixed when it is written.

| Entry | When |
| --- | --- |
| `proposed` | a `propose` over the socket; a run also gets its `run_…` id here |
| `approved` | a click or keystroke on the frozen snapshot |
| `declined`, `withdrawn` | the person in the window; the proposer over the socket, while pending |
| `expired` | 24 hours pending; ten minutes approved without a start; Quit (`quit_before_start`); relaunch (`restart_before_start`) |
| `started` | the durable start, before any final provider read or write; only within ten minutes of the approval, and never after Quit |
| `outcome_unknown` | a lost reply, or a start without a result found at relaunch (`app_restarted`) |
| `executed`, `failed` | a definite result; after an unknown outcome only by read-back |

State is rebuilt from the entries at launch. Recovery then expires every approval without a start and marks every
start without a result unknown, before the window or the socket opens. Nothing dispatches an action again.

Each entry becomes one Signal record ([the contract](#the-contract-with-signal)). Its exact bytes are built once,
on first delivery with the person's `act_…` id, and kept. A merge is about its pull request
(`github:pr:{owner}/{repo}#{n}`). A post or a run is about the person's own subject (`signal:actor:{act_…}`) until
its handler can tell whether the conversation is public. The sender posts only the first undelivered record. It
moves on only after Signal stores it or answers with its identical replay. A lost answer or an unreachable Signal
resends the same bytes later, and a `409` or another refusal stops delivery at that record.

## The vault

`Vault` keeps Beacon's service credentials in one file, encrypted with AES-GCM (mode `0600` in a `0700`
directory). Each credential is bound to a service, an HTTPS origin (`https://host[:port]`, nothing else), the
provider's stable account id and, for Slack, the workspace (13.00-D11, D31). A client asks for a credential by
that exact binding, taken from Beacon's settings, never from an agent. A different origin, port, account or
workspace finds nothing. There is no way to list secrets, and a secret prints as `[redacted]`.

The 256-bit key is a generic password item in the login Keychain. The app creates it on first use with an access
list naming only its own designated requirement. Before opening any keychain, the code checks that the running
process is `dev.beacon.app` signed with a certificate. An ad-hoc build, a test host or any other program is refused
first. Tests use a fixture key and fixture secrets. Nothing in this repository's tests or CI opens a keychain.

## What the files do not protect

The journal and the outbox hold prompts, messages, notes and parameters in plain text: plan 13.00 cut their
encryption. "Append-only" describes how Beacon writes them. Any process running as the person can read the
files, and could edit, truncate or roll them back without Beacon noticing. The vault's Keychain protection
covers only service credentials.
