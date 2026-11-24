# Go approval service (13.01 preview)

This is an opt-in [CLI preview](commands.md#go-review-preview) on macOS and Linux. Use a
separate `--state-dir` for `beacon serve`, `beacon open` and the preview form of `beacon act`. Default
`beacon act` (including `quit`) still uses the signed Beacon.app socket; neither route falls back to the other.
The preview does not read collector config, Keychain items or the Swift journal. There is no installer cutover
or migration. With no settings, proposals are review-only and no records leave the machine. GitHub merges,
Slack posts, agent runs and Signal record delivery each require explicit configuration in this state directory.

## Passkey setup

`beacon setup --state-dir DIR` prints a 10-minute, one-use code and does not store the code. It holds the
instance lock, so it refuses while `beacon serve` is running. The code starts a registration: a create
ceremony that must report PRF, then an assertion whose 32-byte PRF output wraps a new write-vault data key.
The output is not written. A second passkey is refused until a later assertion-gated path exists.
`/passkeys/new` takes that code. The script registers with PRF and posts both ceremonies. Registration
takes the instance lock, so it cannot run beside `serve` unless this process already holds that lock.
The first registration checks the reset position and absence of an active enrollment under the live journal
lock. It writes `write-vault.json`, a key-check sealed under the new data key, and commits the SHA-256 of
those bytes on the `passkey_added` frame without reopening the path. The service reads the vault once per assertion: the
bytes it hashes against the live journal's enrollment are the bytes it decrypts. A later reset clears
that enrollment, ordered by journal position, not the clock. It records an active enrollment's hash
mismatch as `passkeys_reset` (`vault_mismatch`). The page then says to stop
`beacon serve` and run `beacon setup --reset-passkeys`; the files stay until that command removes them.
A same-user process can still replace both files and the check. The real backstop is
that write credentials are sealed under the real data key, so a swapped vault approves nothing that can
execute. Handlers must fail closed when they cannot unseal. Deleting `passkeys.json` does not allow
another first registration while a recorded passkey exists. `beacon setup --reset-passkeys` (service
stopped) opens the journal and records `passkeys_reset`. A later setup code is bound to that frame's
position in the journal, not to the clock, including when the reset marker file is missing. A ceremony started before the reset cannot finish. A vault
left behind with no `passkey_added` is failed-registration debris and is removed.

A signed-in browser can freeze a proposal only when a handler resolves its account and destination. One
assertion covers that frozen hash, or a batch of at most 10 merges or posts in their pending-list order.
An `agent.run` cannot join a batch. The freeze nonce belongs to that browser session; starting a
new selection cancels the earlier nonce even if the new selection fails. The final selection check and
commit are serialized with refreeze. The assertion is refused unless its PRF output unwraps the write
vault and that key opens the key-check. A missing write vault approves nothing. The new sign count is
stored before the journal records `approved`. A batch is several `approved` frames plus an
`approved_commit` marker; replay applies the frames only when the marker is present. Each approved frame
records the credential id and, for a batch, the batch challenge. Before accepting the approval, Beacon
must also unseal every required write token. A run has no provider write token, but still needs its own
verified assertion, including the same PRF unwrap, key-check and journal hash. It does not bypass the vault
check. Beacon clears the vault key without passing it to a worker or a child. Each action needs a durable start
and a final destination check. No write or launch is retried, even after a lost reply or process restart.

## Provider credentials and GitHub

Put `action-providers.json` (`0600`) in the private state directory before starting `beacon serve`. For example:

```json
{"github":{"origin":"https://api.github.com","user_id":123456}}
```

Use your numeric GitHub user ID. Only the configured HTTPS API origin is contacted; there are no redirects
or environment proxies. `beacon egress --state-dir DIR` reports these hosts without loading credentials.
After registering a passkey, sign in with `beacon open --state-dir DIR` and go to **Provider credentials**.
Save two distinct tokens for that user with a separate settings assertion. The service verifies both
against `GET /user`, refusing bots and a different account. Choose a least-privilege read token (repo
metadata, pull requests, checks, commit statuses and the available rules) and grant merge permission only
to the write token. Identity validation does not establish that a GitHub token is read-only.

The read token goes in `unattended.kdbx`. The write token goes in `write-github.sealed`, authenticated with
the provider, origin and account under the passkey vault's data key. They cannot be the same credential.
After a passkey reset, register again and re-save the write credential; the earlier ciphertext cannot open
under the new key. The unattended read vault is not reset.

The preview reads the PR, reviews, rules and checks with the read token. Every reported check and every
visible required check must pass; only the latest run for each check name and app counts. The frozen view
names the approved head. After approval the write token is used for final reads, including one last PR
read, and at most one `PUT` with the exact approved method and head SHA. The provider alone enforces
branch rules at the write. A lost or ambiguous reply is read back with the unattended token. Success
requires the approved repository, base, head, this user, and a merge SHA. A merged pull request with a
different actor, head, or base fails as `merged_otherwise` and is not read again. A merge that matches
this account, head, and base but has no actor or merge SHA stays unknown: GitHub can omit the SHA briefly
after Beacon's own merge. The Swift app fails that incomplete case as `merged_otherwise`; the Go preview
does not, so it cannot record a false failure for its own write. An open PR settles as `not_merged` only
after two eligible reads at least a minute apart, both at least a minute after the outcome became unknown.
Automatic read-back keeps that one-minute cadence until then, then doubles the wait up to 15 minutes, so
a read that never settles does not spend the recovery budget every minute. The sweep asks each handler
every 15 seconds, so a slow read does not add another minute of wait. The confirming open read is due
one minute after the first open observation, measured when that read finishes. A later `merged: true`
answer, even without a SHA, discards open samples and cannot become `not_merged`. A read the recovery
quota refuses does not count as an attempt and is tried first on the next pass. **Read again** uses the
same budget, is not delayed by backoff, and does not count toward it; it cannot resend a merge. A start that stops or is cancelled
before the write is claimed fails as `stopped` (`pre_dispatch`) when the journal can still record. A
journal that had already failed is left unchanged. If appending that stop record fails, shutdown reports
the failure instead of success.

## Slack posts

Set the Slack workspace and user IDs in `action-providers.json`, alongside or instead of `github`:

```json
{"slack":{"workspace":"T0123456789","user_id":"U0123456789"}}
```

Slack's origin is fixed at `https://slack.com`. In [Slack apps](https://api.slack.com/apps), use **OAuth &
Permissions → User Token Scopes** to obtain two distinct, non-rotating **User OAuth Tokens** (`xoxp-…`)
for this user in this workspace. Use separate app grants so the unattended token has only these scopes:

- `channels:read`, `groups:read`, `im:read`, `mpim:read`, `users:read`
- `channels:history`, `groups:history`, `im:history`, `mpim:history`
- Optionally `metadata.message:read` for read-back, if the workspace requires it

The write token needs the required reads plus `chat:write`. Use **Provider credentials → Set slack tokens**
with a separate settings assertion. Beacon checks each token's user and workspace with `auth.test` and
its grants with `X-OAuth-Scopes` before saving. The write token is sealed in `write-slack.sealed` under
the passkey vault key, bound to Slack's origin, user and workspace. The read token goes in `unattended.kdbx`.

Propose with `{ workspace, channel, thread_ts, text }`; use `thread_ts: null` for a top-level post. When Slack
is configured, intake must verify the workspace and conversation visibility with the read token, or the
socket refuses the proposal. A verified public channel uses `slack:<channel>:<root ts>` for a reply, or
`slack:<channel>:<YYYY-MM-DD>` for the proposal's local day for a top-level post. Private channels, DMs and
group DMs stay on the person's Signal subject. This does not change after intake, and the proposer cannot
choose the record subject. With no Slack configuration, proposals stay review-only on the person's subject.
After enabling Slack, resubmit a pending proposal that had no verified intake.

Review rechecks the account, workspace, visibility, archive state, membership and (for a reply) the exact
thread root. Conversation names and the root's first line are for display only. A failed name lookup shows
`unknown channel <id>`. The preview has no live links. It flags broadcast and group mentions and opens
**Exact Slack mrkdwn** by default for special mentions. **Show exact parameters** includes the verbatim
text and destination IDs. Drafts over 4,000 UTF-16 code units cannot be approved.

After approval and durable start, Beacon repeats the checks with the unsealed write token and makes at
most one `chat.postMessage` with the frozen text and unfurls off. The start's operation UUID goes in both
`client_msg_id` and `beacon_post` message metadata, not in the text or Signal records. The provider alone
enforces permissions at the write. A definite refusal is terminal; a `429` respects `Retry-After` but never
retries the post. Stopping or cancelling before that post is claimed fails as `stopped` (`pre_dispatch`)
when the journal can still record, so restart does not treat an unsent draft as a lost reply. A journal
that had already failed is left unchanged. If appending that stop record fails, shutdown reports the failure.

A lost or unclear reply triggers only read-back with the unattended token. It searches up to four pages of 15
messages in the approved conversation or thread, from one minute before to at most five minutes after the
original start. It confirms only one unambiguous match for the UUID, exact text, user, destination and time.
Missing or conflicting evidence stays `outcome_unknown`. Automatic read-back runs at startup and after a lost
reply, backing off from 15 to 120 seconds until a minute after the search window ends. **Read again** can
still search that same window. Intake, review and recovery have separate read budgets and Slack method
cooldowns; none can resend a post.

## Agent runs and checkouts

Stop `beacon serve` and put `agent-runs.json` (`0600`) in the preview state directory to enable runs:

```json
{
  "opencode": "/home/me/.local/bin/opencode",
  "models": ["provider/model"],
  "checkouts": [
    {"name": "work", "path": "/home/me/checkouts/work"},
    {"name": "review", "path": "/home/me/checkouts/review"}
  ]
}
```

Choose the absolute path of the OpenCode executable you trust, and list the exact `provider/model` IDs to
offer. On Linux, use the native binary (a symlink to it is fine), not a script wrapper: the reaper verifies
the executable and its argument vector before confirming launch. The Go preview does not invoke a login
shell or start OpenCode to discover models during review.
OpenCode uses its own configuration and sign-in under your home directory. The fixed environment includes
the executable's directory, `~/.local/bin` and system directories on `PATH`, not the service's environment
or credentials. **Settings** shows the configured checkouts; change the file with the preview stopped.

The proposal names a checkout, not a path, with a model and exact prompt. `propose` returns `runner_id` for
routing the report. The page shows the resolved path, branch and device/inode, and lets you change only the
model. **Resolve and review** freezes the selection and displays the prompt and the exact chosen model.
The one-use assertion covers that snapshot. A checkout can have more than one name, but only one run can
claim its directory at a time. If it is busy the proposal stays pending. Other checkouts run in parallel.

Beacon keeps a directory descriptor from freeze through the handoff and reads branch metadata relative
to it. An abandoned or expired freeze releases the descriptor. After the durable start, Beacon checks
the checkout and branch again and passes the held descriptor to the [reaper](supervision.md). The
bootstrap enters through it and refuses to exec if the approved path no longer resolves to that device
and inode or HEAD no longer matches the frozen branch (the full commit when detached). While the bootstrap is still gated, Beacon
records its group locally as a one-shot launch permit. The child cannot make a proposal before the group
has been registered. Launch is `opencode run --standalone --format json --auto --model MODEL`, with the
unchanged prompt fully staged on an anonymous stdin descriptor before release: a sealed memfd on Linux,
or an unlinked file opened read-only for the child on macOS. `PWD` is set to the verified checkout and
`OLDPWD` is not passed. Beacon
does not run a checkout's `bin/run`. `--auto` allows permissions that OpenCode's configuration does not
deny. This is **not a sandbox**: the agent acts as your user and may use your files, sign-in and network.
Those connections are OpenCode's, not Beacon's. To use the preview proposal socket from a run, use
`beacon act --state-dir DIR` as for other preview clients.

Before acknowledging confirmed exec, the reaper syncs a local receipt revealing the nonce behind the
journalled permit's hash commitment. Reading the permit alone does not prove exec.
The service records `executed`, then `runner_working`. A definite refusal is `failed`; if a permit
was sent but there is no exec evidence, the launch stays unknown. After a crash, the next service can
read back a matching receipt once the reaper's sweep succeeds. Nothing restarts the run. No prompt file
is created. `output` and `errors` stay `0600` under `runs/{runner_id}/` (`0700`); they are not service logs.

**Stop run** on the proposal page uses the reaper's bounded TERM/KILL cleanup. Service shutdown stops
all active runs. Beacon checks the pinned output and error descriptors for the combined 64 MiB bound
and stops the run if the bound is exceeded or the files cannot be measured. It checks again before recording
success. At leader exit, exit 0 produces `runner_finished` with the last assistant message,
capped at 32,000 characters, or a fixed message if there is none. Other exits produce `runner_failed`
with a fixed code and, where available, the last OpenCode error or a bounded stderr tail. A requested stop
records a stop reason if the leader was still running. Shutting down during descendant cleanup does not
replace a leader's recorded result. `runner_closed` requires confirmed cleanup. Cleanup confirmation frees
the checkout even if the reaper could not persist a launch receipt. The next startup can close interrupted runs after its sweep. A durable start
without a permit is recovered as not launched; a permit alone is not evidence of launch.

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
without a browser or socket read, waking waiters after the expiry is durable. The [reaper](supervision.md)
sweeps the previous Go session under the instance lock before replay. Ctrl-C or SIGTERM stops the service,
closes both listeners and the socket, cancels provider work and any fixture delivery, stops supervision, and
releases the instance lock. If the reaper is lost, the service stops serving and the next start must sweep again.

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
shorten the wait or clear a stop. After addressing a delivery refusal, run
`beacon act --state-dir DIR resume-delivery`. It uses the same-user proposal socket and refuses connections
attributed to a run. For an operator it queues a resume of the stopped head, or bypasses backoff, including
when requested during a send. It cannot approve or retry
an action, skip a record or resend an acknowledged one.

To opt into delivery, add `signal` to `action-providers.json` with your **test** approval app actor ID, its
person's actor ID and handle, and the Signal origin. For example:

```json
{"signal":{"origin":"https://signal.example.com","actor_id":"act_01m6a6ft00e40r2081040g2082",
           "person_id":"act_01m6a6ft00e40r2081040g2081","handle":"example"}}
```

With the service stopped, put the corresponding approval-app key in an entry in the vault's `Beacon` group
using KeePassXC (the key is the entry's password). The `unattended.kdbx` entry needs these custom attributes:

| Attribute | Value |
| --- | --- |
| `beacon.service` | `signal` |
| `beacon.origin` | the exact configured Signal origin |
| `beacon.account` | the app's `actor_id`, not the person's ID |
| `beacon.workspace` | empty |
| `beacon.use` | `records` |

This is separate from the collector's Signal key. The preview does not set up an app actor, redeem a setup
code, or migrate Swift's records. Do not point a preview journal at the app actor in live use by Swift. Signal
enforces the app actor and person at `POST /api/app-records`. A missing or unreadable entry leaves the outbox
pending. Use HTTPS (the literal loopback HTTP exception is for fakes). `beacon egress --state-dir DIR` lists
this host alongside configured provider hosts, without opening the vault.

Run records use `agent-run:{handle}:{runner id without run_}`. The report or failure detail is sealed at
`owner+agents` and marked `from: "runner"`: it is data for the session that requested the run, never an
instruction or authorization. Checkout paths, the process group and the local launch permit are not record fields.
The Go contract tests use the unmodified [Signal fixtures](../internal/approval/testdata/) and a fake Signal
over loopback. The collector's outbox and egress are unchanged.

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
The Go supervisor can attribute a connection to a live run using the kernel PID, never the request's agent
or context. Attribution is pinned
at connect time; PID reuse is a limit. `get`, `wait` and `withdraw` are scoped to the same run, or to the
shared unattributed identity. The socket also supports `types`, `propose` (which returns `review_url` from
the configured origin) and `open` (issues a browser sign-in code, not authority to approve). There is no `quit`, list or approve.

Limits from Swift are retained: newline-delimited JSON at most 256 KiB per frame, 32 connections, 16 waits
of up to 300 seconds, 20 pending and 60 proposals per hour per identity, and 40 pending overall. Hourly
counters are in memory. Partial frames and idle connections time out. A disconnected waiter frees its slot
without waiting for a journal change. Slack intake has a four-second deadline, with at most two lookups in
flight, 120 reads per hour overall and 30 per hour per identity. Verified intake identity and user/group names
can be reused for ten minutes. Provider preflight reads are capped at two batches of reads in flight and 120
requests per hour, including pages. Final checks have their own budget, and recovery has a separate 60-read
hourly budget per provider.

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
frozen snapshot for a proposal, its frozen parameters and resolved binding have a separate detail view.
Use **Resolve and review** to freeze a supported proposal, or select up to 10 supported writes on the pending
page. Approval, token settings and registration require the embedded passkey script. The other pages need
no scripts or external resources. Apart from passkey registration (which needs the setup code and exact
origin), POSTs require the canonical `Origin` (or `Sec-Fetch-Site: same-origin` if absent), a session and its
form CSRF token. The session alone cannot execute a write or save credentials. Templates escape
proposer content and make invisible formatting visible without changing the stored bytes. Responses are
not cached or frameable, and the service does not log requests, codes or proposal text.
