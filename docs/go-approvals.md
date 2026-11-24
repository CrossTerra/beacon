# Go approval service (13.01 read-only preview)

This slice of Part 2 is an opt-in [CLI preview](commands.md#go-review-preview) on macOS and Linux. Use a
separate `--state-dir` for `beacon serve`, `beacon open` and the preview form of `beacon act`. Default
`beacon act` (including `quit`) still uses the signed Beacon.app socket; neither route falls back to the other.
The preview does not read collector config, Keychain items, vaults or the Swift journal. There is no installer
cutover, migration, live Signal credential source, provider lookup, provider write or runner launch. The three
schemas (`github.merge`, `slack.post`, `agent.run`) are for review only. There is no approve or decline
control, batch decision, or notification. `beacon setup` prints a one-time passkey code for that directory. `/passkeys/new` takes the code
and registers one passkey. The PRF output is not stored. Approval is not on the page yet.

## Passkey setup

`beacon setup --state-dir DIR` prints a 10-minute, one-use code and does not store the code. It holds the
instance lock, so it refuses while `beacon serve` is running. The code starts a registration: a create
ceremony that must report PRF, then an assertion whose 32-byte PRF output wraps a new write-vault data key.
The output is not written. A second passkey is refused until a later assertion-gated path exists. There is
`/passkeys/new` takes that code. The script registers with PRF and posts both ceremonies. Registration
takes the instance lock, so it cannot run beside `serve` unless this process already holds that lock.
The first registration writes `write-vault.json`, a key-check sealed under the new data key. Its SHA-256
is stored on the `passkey_added` journal frame. `OpenVault` refuses a file that does not match the latest
`passkey_added` with no later `passkeys_reset`. Later means a higher frame in the journal, not a later
clock. It records that mismatch as `passkeys_reset` (`vault_mismatch`). The page then says to stop
`beacon serve` and run `beacon setup --reset-passkeys`; the files stay until that command removes them.
A same-user process can still replace both files and the check. The real backstop is
that write credentials are sealed under the real data key, so a swapped vault approves nothing that can
execute. Handlers must fail closed when they cannot unseal. Deleting `passkeys.json` does not allow
another first registration while a recorded passkey exists. `beacon setup --reset-passkeys` (service
stopped) opens the journal and records `passkeys_reset`. A later setup code is bound to that frame's
position in the journal, not to the clock, including when the reset marker file is missing. A ceremony started before the reset cannot finish. A vault
left behind with no `passkey_added` is failed-registration debris and is removed.

A signed-in browser can freeze a proposal only when a resolver supplies the account binding. This preview
does not set one, so the page does not freeze a live proposal and does not invent an account. When a
binding is supplied, one assertion covers that frozen hash, or a batch of at most 10 merges or posts.
An `agent.run` cannot join a batch. The freeze nonce belongs to that browser session; a new freeze
cancels the session's earlier nonce. The assertion is refused unless its PRF output unwraps the write
vault and that key opens the key-check. A missing write vault approves nothing. The new sign count is
stored before the journal records `approved`. A batch is several `approved` frames plus an
`approved_commit` marker; replay applies the frames only when the marker is present. Each approved frame
records the credential id and, for a batch, the batch challenge. `approvalserve.Dispatch` is the only
path to a handler. It is nil: `github.merge`, `slack.post`, and `agent.run` exist in Swift, not in Go,
so nothing is started or sent.

## Unattended vault

`unattended.kdbx` is a KDBX 4 database opened by `unattended.keyx`, a KeePassXC XML v2.0 key file created
`0600`. Create links both files into place and refuses if either name already exists; it does not replace
a key file or a database. There is no master password. The cipher is ChaCha20. The KDF is Argon2id with 64 MiB of memory
and 2 iterations. Entries are found by `beacon.service`, `beacon.origin`, `beacon.account`,
`beacon.workspace`, and `beacon.use`, not by title. A write that finds the file changed re-reads and
retries once when the target entry is unchanged; a changed entry, or a second conflict, writes nothing
(`vault_conflict`). After the rename it reads the file back and returns `vault_conflict` if the bytes
differ. Linux Secret Service is not wired; both platforms use the key file. The preview does not yet
read a collector token from this vault.

## Journal and record delivery

`journal.log` is an owner-only append-only file, protected by `instance.lock` in the private state directory.
The first frame fixes the format version and the journal's UUIDv7. A frame has a length (and its bitwise
complement), a JSON payload and a checksum. A short final frame is truncated to the last complete frame.
A complete frame with a bad CRC, or any other invalid complete frame, stops startup and leaves `journal.log`
unchanged. `beacon serve` prints `approval journal is corrupt or from an unsupported format` and exits. It
does not listen, deliver, or mint a new journal id. Copy the state directory before changing anything.
Restore `journal.log` from a copy that ends on a good frame and has the same journal id. Do not delete the
damaged file to start over: a new id would begin Signal sequence 1, and the damaged file may be the only
record of bytes already sent. Beacon will not drop a bad-CRC frame. Truncating one by hand is only safe
after that copy, and only when the bad frame is the final frame, every earlier frame is intact, and dropping
it cannot hide a send Signal may already have stored. If that cannot be shown, leave the service stopped.
Each append is synced (`fsync`, plus `F_FULLFSYNC` on macOS) before publishing its result. The portable
`golang.org/x/sys/unix` dependency supplies the lock, no-follow file opens and the macOS full flush. The
directory rename that publishes a new journal is flushed on the directory descriptor itself. On macOS that
is `F_FULLFSYNC`. If that call fails, Beacon calls `unix.Fsync` (`syscall.Fsync`) on the same descriptor.
It does not call `os.File.Sync`: on macOS that is also `F_FULLFSYNC` and would fail the same way. Linux uses `unix.Fsync`. The flush reports which call reached the disk, `F_FULLFSYNC` or `fsync`.
Proposals and any frozen snapshots are
plaintext on disk. Keep the preview directory private and use synthetic proposals.

An approval that did not start expires at restart or shutdown. A durable start without a result becomes
`outcome_unknown` at restart; only a read-back result can settle it. Replay does not dispatch. A supplied
archive ID at the journal API is a lower bound for a new journal, not an import: the Go ID must sort strictly
after it. Pending proposals survive a restart until their 24-hour deadline. The service sweeps expiries even
without a browser or socket read, waking waiters after the expiry is durable. Ctrl-C or SIGTERM stops the service,
closes both listeners and the socket, cancels any fixture delivery, and releases the instance lock.

An action frame fixes its Signal record sequence and replay nonce. Local markers and cursor changes consume
no sequence numbers. The ordered sender projects the head of the outbox into Signal's fixed envelope, syncs
the exact request bytes together with an in-flight marker, then makes one `POST /api/app-records` to the
configured Signal origin, without a proxy or redirects. Only a synced acknowledgement advances the cursor. A
restart with an unfinished attempt, a lost response or a 5xx can have reached Signal; retry and resume keep
those bytes unchanged. A conflict stops the head until explicit resume. When there was only a definite refusal,
resume can rebuild for corrected settings. Local labels (PID and executable) and a proposer's context never go
to Signal. Without a trusted Slack intake lookup, Slack records use the person's own subject.

The delivery worker waits 30 seconds after a retryable failure, then doubles the delay up to 15 minutes. A
longer `Retry-After` from a rate limit or server failure takes precedence (up to an hour). New proposals do not
shorten the wait or clear a stop. An explicit resume can bypass backoff, including when requested during a send.

The Go contract tests use the unmodified [Signal fixtures](../internal/approval/testdata/) and a fake Signal
over loopback. `approvalserve.Options.ConnectRecords` lets tests drive the worker through the service lifecycle.
The CLI leaves that hook unset, so no records leave via `beacon serve`; the collector's outbox and egress are
unchanged.

## Proposal socket

The Go socket lives at `run/act.sock` (`0600`, in a `0700` directory). Binding can leave a wider mode if
the process dies before the following chmod. Restart removes that dead socket when this user owns it;
it does not remove a live socket, a symlink, a non-socket, or another user's socket. The client still
requires exact `0600` before it connects. The listener admits only the service's own user ID from the
kernel (`SO_PEERCRED` on Linux; `LOCAL_PEERCRED` and `LOCAL_PEERPID` on macOS). The client then checks
the listener's kernel user ID. Neither check verifies code signatures. The signed Swift socket and client
are unchanged.

The PID and executable path are local, unverified labels. The path lookup is best effort (`/proc/PID/exe`
on Linux; the `proc_pidpath` kernel query on macOS without cgo). Neither reads arguments or environment.
An internal hook for the future supervisor can attribute a connection to a live run using the kernel PID,
never the request's agent or context. All CLI preview peers are unattributed. Attribution is pinned
at connect time; PID reuse is a limit. `get`, `wait` and `withdraw` are scoped to the same run, or to the
shared unattributed identity. The socket also supports `types`, `propose` (which returns `review_url` from
the configured origin) and `open` (issues a read-only sign-in code). There is no `quit`, list or approve.

Limits from Swift are retained: newline-delimited JSON at most 256 KiB per frame, 32 connections, 16 waits
of up to 300 seconds, 20 pending and 60 proposals per hour per identity, and 40 pending overall. Hourly
counters are in memory. Partial frames and idle connections time out. A disconnected waiter frees its slot
without waiting for a journal change. This slice has no preflight or provider calls to budget.

## Browser review

The review listeners bind only `127.0.0.1:7317` and `[::1]:7317`. Both must bind; there is no fallback
port. The canonical `--origin` defaults to `http://localhost:7317`; one `https://{machine}.{tailnet}.ts.net`
name can be configured together with `--tailscale-login`. The service does not set up, check or contact Tailscale.
In tailnet mode the existing private reverse proxy must preserve the canonical `Host` and pass the configured
`Tailscale-User-Login`. This header filters proxy traffic;
it does not authenticate a local process. Old `GET` links on `localhost:7317` redirect to the canonical
path without forwarding the query. Other hosts and the loopback IP literals are not valid origins.

An admitted socket peer can mint a two-minute, single-use `open` code. `beacon open` prints the validated link
instead of passing the bearer code to a browser opener. Treat it as a secret until used or expired. `/open`
exchanges it for a 12-hour, memory-only review session and redirects without the code. The cookie is HttpOnly and
SameSite=Lax; tailnet mode uses `Secure` and `__Host-`. Cookies are not isolated by port, so a server
on another port of the same host can receive one if the signed-in browser visits it. Restart invalidates
all codes and sessions.

Only a signed-in browser sees the pending list and proposal detail. If the journal already holds a
frozen snapshot for a proposal, its frozen parameters and resolved binding have a separate detail view. The
preview cannot create one through the CLI or the browser.
No web request can freeze, approve, decline, dispatch or change settings. POSTs require the canonical
`Origin` (or `Sec-Fetch-Site: same-origin` if absent), a session and its form CSRF token, and there are
no POST handlers. The embedded HTML and CSS need no scripts or external resources. Templates escape
proposer content and make invisible formatting visible without changing the stored bytes. Responses are
not cached or frameable, and the service does not log requests, codes or proposal text.
