# Local approvals

Use [`beacon serve`](commands.md#approval-service) under launchd or systemd. It replaces Beacon.app's approval
window. The browser runs on one canonical origin: `http://localhost:7317`, or the machine's `https://…ts.net`
name via private Tailscale Serve. The listener stays on loopback. There is no menu bar item or shortcut.

## Propose and review

`beacon act` uses the proposal socket at `run/act.sock` in Beacon's private state directory. The client checks
file ownership and the kernel peer user ID; no signature or `act.json` is used. The server admits only your user.
It records the PID and executable as unverified labels. A proposal from a verified supervised run belongs to
that run. `get`, `wait` and `withdraw` do not cross run ownership. There is no operation to approve, list, quit,
or issue a passkey enrollment code. `beacon open` uses the socket for a sign-in link only.

`types` lists the configured types and their schemas. `propose` answers with an ID, version, status, parameter
hash, and `review_url` on the canonical origin. A run proposal also returns `runner_id` for routing its report.
Forward the review URL, not a re-created URL. If Beacon is unavailable
or the type is disabled, stop. Never go directly to the provider or automatically submit an old proposal again.
An OS notification carries only pending counts and action types.

Open the review link, or run `beacon open` to print a one-time session link. A session can read, decline and stop
runs; it cannot approve. Resolve the selected proposal to freeze the exact action. Approve requires a fresh
passkey assertion binding the approved hash, with user verification and PRF to open the write vault. GitHub
merges and Slack posts can be batched (up to 10 explicitly selected actions); a run always has its own assertion.
Only the model of a run can be changed at review. An assertion on a stale, consumed, withdrawn or expired
snapshot approves nothing.

## Writes, recovery and delivery

Each handler checks the account and destination again after a durable start. It sends at most one write or
launch; losing the reply permits read-back, not another write. A restart expires approvals without a start.
A started action without a result is unknown until confirmed. The user service stops the collector and its
runs on shutdown. See [Go approval implementation](go-approvals.md) for exact checks, scopes, Slack previews,
the outbox, and the [Signal record contract](go-approvals.md#journal-and-record-delivery).

- [GitHub: which credentials and what is checked](go-approvals.md#provider-credentials-and-github)
- [Slack: scopes, destination and read-back](go-approvals.md#slack-posts)
- [Agent runs](go-approvals.md#agent-runs-and-checkouts) and [supervision](supervision.md)
- [Install, register, and migrate a previous Swift installation](install.md)
