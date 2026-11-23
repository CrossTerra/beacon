# Go approval service (13.01, in progress)

This code does not replace Beacon.app or its signed client. There is no migration or live Signal credential
source in this slice of Part 2. No Go code calls a provider action or starts a runner.

## Journal and record delivery

`journal.log` is an owner-only append-only file, protected by `instance.lock` in the private state directory.
The first frame fixes the format version and the journal's UUIDv7. A frame has a length (and its bitwise
complement), a JSON payload and a checksum. A short final frame is truncated to the last complete frame;
a bad checksum or invalid complete frame stops startup rather than silently losing prior work. Each append is
synced (`fsync`, plus `F_FULLFSYNC` on macOS) before publishing its result. The portable `golang.org/x/sys/unix`
dependency supplies the lock, no-follow file opens and the macOS full flush.

An approval that did not start expires at restart or shutdown. A durable start without a result becomes
`outcome_unknown` at restart; only a read-back result can settle it. Replay does not dispatch. A supplied
archive ID is a lower bound for the new journal, not an import: the Go ID must sort strictly after it.

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
over loopback. The sender has no live caller yet; the collector's outbox and its egress are unchanged.

## Proposal socket

The Go socket lives at `run/act.sock` (`0600`, in a `0700` directory). It admits only the service's own
user ID from the kernel (`SO_PEERCRED` on Linux; `LOCAL_PEERCRED` and `LOCAL_PEERPID` on macOS). The
client checks the socket and its directories for ownership and mode before it connects, then checks the
listener's kernel user ID. Neither check verifies code signatures. The signed Swift socket and client
are unchanged.

The PID and executable path are local, unverified labels. The path lookup is best effort (`/proc/PID/exe`
on Linux; the `proc_pidpath` kernel query on macOS without cgo). Neither reads arguments or environment.
An internal supervisor hook can attribute a
connection to a live run using the kernel PID, never the request's agent or context. Attribution is pinned
at connect time; PID reuse is a limit. `get`, `wait` and `withdraw` are scoped to the same run, or to the
shared unattributed identity. The socket also supports `types`, `propose` (which returns `review_url` from
the configured origin) and `open` (a hook for a read-only sign-in code). There is no `quit`, list or approve.

Limits from Swift are retained: newline-delimited JSON at most 256 KiB per frame, 32 connections, 16 waits
of up to 300 seconds, 20 pending and 60 proposals per hour per identity, and 40 pending overall. Hourly
counters are in memory. Partial frames and idle connections time out. A disconnected waiter frees its slot
without waiting for a journal change. This slice has no preflight or provider calls to budget.

## Browser review

The review listeners bind only `127.0.0.1:7317` and `[::1]:7317`. Both must bind; there is no fallback
port. The canonical origin is `http://localhost:7317` or one `https://{machine}.{tailnet}.ts.net`
name. The service does not set up or contact Tailscale. In tailnet mode the reverse proxy must preserve
the canonical `Host` and pass the configured `Tailscale-User-Login`. This header filters proxy traffic;
it does not authenticate a local process. Old `GET` links on `localhost:7317` redirect to the canonical
path without forwarding the query. Other hosts and the loopback IP literals are not valid origins.

An admitted socket peer can mint a two-minute, single-use `open` code. `/open` exchanges it for a
12-hour, memory-only review session and redirects without the code. The cookie is HttpOnly and
SameSite=Lax; tailnet mode uses `Secure` and `__Host-`. Cookies are not isolated by port, so a server
on another port of the same host can receive one if the signed-in browser visits it. Restart invalidates
all codes and sessions.

Only a signed-in browser sees the pending list and proposal detail. If the journal already holds a
frozen snapshot for a proposal, its frozen parameters and resolved binding have a separate detail view.
No web request can freeze, approve, decline, dispatch or change settings. POSTs require the canonical
`Origin` (or `Sec-Fetch-Site: same-origin` if absent), a session and its form CSRF token, and there are
no POST handlers. The embedded HTML and CSS need no scripts or external resources. Templates escape
proposer content and make invisible formatting visible without changing the stored bytes. Responses are
not cached or frameable, and the service does not log requests, codes or proposal text.
