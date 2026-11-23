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
