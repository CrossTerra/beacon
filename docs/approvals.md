# Approvals: the proposal socket, the vault and Signal's contract

Plan 13.00 has Bosun propose an exact action to Beacon.app over a local socket. The person approves it in the app
window, and Beacon carries it out with its own credentials. This page describes the parts that exist so far.

**Status.** The socket server, the vault and the contract code are in `mac/Sources/BeaconCore` with tests.
`beacon act` is the client. The app does not open the socket or the vault yet; that comes with the approval
window and the journal. No action type is enabled and nothing is executed: the handlers are plan 13.00 part 3.

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

There is no operation to approve, list proposals, read history or see runners. `get`, `wait` and `withdraw` answer
only the client identity that made the proposal. Any other id reads as `not_found`. Clients with the same admitted
identity cannot tell their proposals apart; a self-reported agent name or session is recorded as such
(`verified: false`).

**Limits.** At most 32 connections, 16 concurrent waits, 20 pending proposals per client identity, 40 pending in
all and 60 proposals per identity per hour. Past a limit the reply is `busy`. An incomplete frame is dropped with
its connection after 10 seconds, and an idle connection after 60. Errors are `{ error }`, or
`{ error: "invalid", field }`: `invalid_frame`, `frame_too_large`, `type_disabled`, `busy`, `not_found`,
`not_pending`. Preflight reads and their limits arrive with the handlers.

A pending proposal expires after 24 hours.

## `beacon act`

The client is `beacon act` (see [commands](commands.md#beacon-act)). It reads
`~/Library/Application Support/beacon/act.json`, which the person writes at setup:

```json
{ "requirement": "identifier \"dev.beacon.app\" and certificate leaf = H\"…\"" }
```

It connects and checks the listener's audit token against that requirement before it sends anything. A listener
with any other signature, or another user's, receives no bytes. `"socket"` overrides the path.

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
