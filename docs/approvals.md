# Approvals: the proposal socket, the vault and Signal's contract

Plan 13.00 has Bosun propose an exact action to Beacon.app over a local socket. The person approves it in the app
window, and Beacon carries it out with its own credentials. This page describes the parts that exist so far.

**Status.** The code is in `mac/Sources/BeaconCore`, with the window in `mac/Sources/Beacon`. When it runs as
`dev.beacon.app`, signed with a certificate, the app opens its vault key, its journal and the socket. An unsigned or
ad-hoc build opens none of them. Adding a service account in Settings enables [GitHub merges](#merges) or
[Slack posts](#slack-posts), independently, and adding a checkout enables [agent runs](#agent-runs). Once Signal is
added in Settings, the outbox goes to Signal's record API ([delivery](#delivery-to-signal)).

The [Go service work](go-approvals.md) is separate from this signed install. It does not import or act on the
Swift journal.

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
| `propose` | `type`, `params`, `reason`, `agent: { name, session? }`, `context?: { subject }` | `{ id, params_hash, status: "pending", version }`, and `runner_id` for an `agent.run` |
| `get` | `id` | `{ id, status, version }` |
| `wait` | `id`, `after`, `timeout` (1 to 300 seconds) | the same, once `version` is past `after` or the timeout ends |
| `withdraw` | `id` | the same, while pending; otherwise `not_pending` |
| `quit` | nothing else | `{ status: "quitting" }`, then the app quits as its menu's Quit does; only for a peer that matches the bundled CLI's code requirement (`scripts/install.sh`) and is confirmed to be external |

The proposals live in the [journal](#the-journal-and-the-outbox). There is no operation to approve, list proposals, read history or see runners. `get`, `wait` and `withdraw` answer
only the client identity that made the proposal, and the same run: a connection from a process in the process group
of a run Beacon launched is that run's ([agent runs](#agent-runs)). Any other id reads as `not_found`, so Bosun and a
run cannot read or withdraw each other's proposals, nor one run another's. Clients with the same admitted identity
and no run cannot tell their proposals apart. Each proposal's `proposed` entry records, locally, the client's
executable path and the requirement it satisfied, from the kernel's code reference for the connection. Neither is
sent to Signal. A self-reported agent name or session is recorded as such (`verified: false`). A proposal from a
run is recorded with `verified: true` and the run's `runner_id` in `proposer`, and the window shows which run it came
from.

Before the startup sweep and after an unconfirmed tether stop, Beacon keeps any known run assignments, but a peer
it cannot attribute to a run cannot `propose`, `get`, `wait` or `withdraw`. It gets `busy`, including for waits
already in flight and for a Slack proposal whose intake lookup was in flight (nothing is stored); `types` is still
available. After the sweep and the check that the old run groups are gone,
external clients must reconnect. A connection that was not cleared does not gain access by staying open. The quit
operation refuses runs, descendants known to have left a run's group, and unclassified peers, even when they use
the bundled CLI. The installer can fall back to SIGTERM if the socket refuses the request.

**Limits.** At most 32 connections, 16 concurrent waits, 20 pending proposals per client identity, 40 pending in
all and 60 proposals per identity per hour. Past a limit the reply is `busy`. An incomplete frame is dropped with
its connection after 10 seconds, and an idle connection after 60. Errors are `{ error }`, or
`{ error: "invalid", field }`: `invalid_frame`, `frame_too_large`, `type_disabled`, `busy`, `not_found`,
`not_pending`, `unavailable`. A Slack proposal does a bounded account and channel lookup before it can be
recorded. If Slack does not confirm the workspace, account and visibility, no proposal is stored; a rate limit
on that lookup returns `busy`. A name that cannot be read does not block the proposal. Intake has its own budget
per client identity: two lookups at once and 30 Slack reads an hour, reusing a verified `auth.test` for ten
minutes and caching the names it shows for ten minutes, so a proposer cannot spend the window's reads. Window
preflights share two slots and 120 provider reads per hour across GitHub and Slack, including pages and _Read
again_. Slack read-back has a third budget ([Slack posts](#slack-posts)). At a limit Beacon returns `busy`
rather than starting another read.

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
provider facts Beacon resolved, such as **Checks: 3 passed** or a Slack conversation's visibility, kept apart from
the agent's reason. The message or prompt shows invisible and bidirectional characters as `⟨U+…⟩`. _Show exact
parameters_ shows the stable account id, the destination and params as canonical JSON, and the approval hash.
Once approved, the selected proposal stays shown with its result, including an unknown outcome being read back. For
an agent run the window shows the checkout's name, branch and path, the full prompt (scrollable), and a model
picker; once launched, whether it is running, with **Stop the run**, then its report or why it ended. The menu counts
the runs working.

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
| `started` | the durable start, before any final provider read or write; only within ten minutes of the approval, and never after Quit; includes Slack's operation UUID locally |
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
when it names the same repository, case aside; Signal compares merge subjects case-insensitively. A run is about
itself, `agent-run:{handle}:{runner id without run_}`, with the person's Signal handle from Settings; without the
handle its records cannot be built, and delivery stops there until Signal is connected again. For Slack the
subject is fixed at intake for the entire lifecycle: `slack:{channel}:{thread_ts}` for a public reply or
`slack:{channel}:{YYYY-MM-DD}` for a top-level public post, using the proposal's local day even if approval is
after midnight. For private channels, DMs and group DMs it is the person's own `signal:actor:{act_…}` subject.
Beacon never uses the proposer's `context.subject` to route these records. Signal seals the draft, reason and
approved destination at `owner+agents`, even on a public subject. A launched run's later life is recorded as runner
events (`app.runner.working`, `finished`, `failed`, `closed`, …), each in the journal and the outbox like any entry.
The sender posts only the first undelivered record. It
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
repository id, the PR and the base (the contract's `destination`), the account and the origin. Each request counts
against the shared preflight read budget, including every page and _Read again_.

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
is `failed` (`not_merged`). Still open is `not_merged` only after two open reads, both at least a minute after the
unknown outcome was recorded (after the write was sent) and at least a minute apart, whether the timer, a relaunch
or the person's _Read again_ asked for them. So a merge GitHub is still finishing is never recorded as failed, and
clicking _Read again_ shows the current state without hurrying the decision. Any other result stays unknown and is
read again every minute while the app runs, at every launch, and on _Read again_. A start that recovery finds without a result is settled the same way.

Every request goes only to the configured origin. Paths are built from the validated repository, number, branch and
SHA (`.` and `..` refused). Redirects are refused, so the token goes nowhere else. Proxy settings are ignored, an
answer is at most 8 MiB, and nothing is retried. `beacon egress` lists the origin from the app's settings.

## Agent runs

`agent.run` (13.00, _Agent runs_, D21, D37) launches opencode in one of the person's checkouts with a full prompt
and a model. Its params are Signal's: `checkout` (a name from Settings, never a path), `prompt` (1 to 32,000
characters, exactly as proposed) and `model` (an opencode `provider/model` id). The agent's name and session are the
proposal's `agent`, as for any type. Beacon allocates the run's runner id when the proposal arrives and returns it
from `propose` as `runner_id`: every record of the run carries it, and Bosun's watch routes the run's report by it
to the session that asked. A run's records are about `agent-run:{handle}:{runner id without run_}`.

**Settings** lists the checkouts: a name and a directory the person picks. The path is stored resolved; two names may
point at one directory.

**Before approval** Beacon reads the checkout's directory identity (`vol:{device}/inode:{inode}`, the contract's
`checkout_id`; both numbers are the unsigned decimals of the directory's device and inode bits, so a negative
Darwin device is the same volume) and its branch from `.git/HEAD` (a worktree's too; `detached:{sha7}` or `none`
otherwise), and the
models opencode lists (`opencode models --standalone`, read at most once a minute). The proposed model must be one
of them. The window shows the checkout's path and branch, the full prompt and a model picker that starts at the
proposed model; the model is the one field the person may change (D7), recorded as `edited: ["model"]` with the
original. It says when the checkout has its own `bin/run`: Beacon runs bare opencode without it, as Bosun's plain
mode does. The account is the macOS user (`macos:user:{name}`) and the origin `null`.

**Only while tether is ready.** Before the startup sweep or if tether stops, Approve is disabled for runs. The desk
drops any run snapshot, rejects clicks or keystrokes bound to it, and shows that the supervisor is not ready. The
proposal stays pending; nothing is consumed or recorded as failed. Once the new session has swept and the old run
groups are gone, the desk resolves the selected run again. Merge snapshots do not depend on tether.

**Approval claims the checkout.** One run per checkout, under any of its names; runs in different checkouts run in
parallel. Approving a proposal for a busy checkout is refused with the reason, and it stays pending until approved
again once that run ends. A claim is freed when the run's processes are all gone, or when a start fails before
launch. Freeing a checkout never starts anything by itself.

**Launch.** After the durable start Beacon checks again: the checkout still has the approved identity (otherwise
`checkout_changed` or `checkout_missing`), opencode is on the login `PATH` (`opencode_missing`), the model is still
configured (`model_unavailable`). Each refusal is `failed` (`pre_dispatch`) with nothing launched. It then writes
`~/Library/Application Support/beacon/runs/{runner id}/` (`0700`): `prompt`, the approved prompt, and `launch`, which
pins the checkout's approved device and inode, and asks [tether](tether.md) to start

```
opencode run --standalone --format json --auto --model {model}
```

in the checkout, with the prompt on standard input (opencode reads a piped prompt as is), its own process group,
and its output and errors in `output` and `errors` beside the prompt (`0600`). tether opens the checkout's directory
once, compares its device and inode with the pinned ones and starts the run in it by that descriptor, so a directory
swapped in at the path after Beacon's check (reading the login environment and the models can take a minute) is
refused, and nothing starts. `--standalone` gives the run a private opencode server inside that group, so stopping
the group stops the agent. Tether spawns the process suspended. Beacon registers its process group and then
acknowledges it over the lease; tether resumes it only after receiving that acknowledgement. If there is no
acknowledgement within five seconds, tether kills and reaps the suspended process. A confirmed resume is `executed`
(`reply`, with the runner id), then `app.runner.working`; refusal is `failed` (`checkout_changed` for a swapped
directory, `launch_failed` for a timeout or other failed launch). With no answer (tether ended), the outcome is
unknown until the next launch reads it back.

**A run's own proposals.** A run can propose with `beacon act` like any client. Beacon finds the connecting process's
process group from the kernel's audit token for the connection; one that is the group of a run it launched (tether
keeps each run's leader unreaped until the whole group is gone, so that group id cannot name another) makes the
proposal that run's: `verified: true` with its `runner_id` in the record, "from run …" in the window, and readable
only by the same run. The group is registered before the process can connect. Beacon rechecks attribution per
request and before completing asynchronous requests. A socket that changes identity must reconnect. A process in
an unrecorded group whose ancestor belongs to a run is neither the run nor Bosun: it can neither read, withdraw nor
create proposals, nor quit. A peer whose ancestry cannot be checked is not treated as external. Beacon keeps
known group assignments after an unconfirmed stop until the next session's sweep and a kernel check confirm the
groups are gone. Approving a run's proposal is a separate approval (D22).

**`--auto`.** Nobody is at a terminal to answer a permission prompt, so every run passes `--auto`: opencode approves
any permission the checkout's opencode configuration does not explicitly deny, and cancels questions. The person
chose this; the approval window says so for every run. A launch approval is not a sandbox (D22): the agent works as
the person, with their files, sessions and credentials.

**What a run inherits.** The person's login environment, as their Terminal has it: Beacon runs their login shell
once per launch (`$SHELL -l -i -c`, bounded to ten seconds, in its own process group) and passes the variables it
prints, so `PATH` and anything their profile exports, tokens included, reach the run. Nothing comes from Beacon's
own environment or vault: no GitHub token, Signal key or other credential Beacon holds is passed, and variables
starting with `BEACON_` are dropped. `PWD` is set to the checkout and `OLDPWD` is not passed: the login
snapshot's `PWD` is not that directory, and a shell that trusts `$PWD` would otherwise run outside it.
`opencode run` has no directory flag; a path passed to it would be the message, so the prompt stays on standard
input. opencode uses its own sign-in for the model. tether adds only its
`BEACON_SUPERVISOR` marker, and sets `PWD` to the directory it enters.

**The report.** When opencode exits, Beacon reads its JSON events: the report is the text of the last assistant
message (a part opencode completes after the session goes idle is joined to the one before), capped at 32,000
characters by keeping the end with a note of the full length. Exit 0 is `app.runner.finished` with the report, or
"The run ended without a final message." Any other exit is `app.runner.failed`: `exit_{code}`, `signal_{n}` or
`interrupted` (130), with opencode's last error message as the detail, or else the end of its standard error.
`app.runner.closed` follows once every process of the run is gone. The report and detail are sealed and marked
`from: "runner"`; Signal and Bosun treat them as data (contract, _Delivering a run's report to Bosun_).

**Stopping.** The person can stop a running run from its detail in the window: its whole group is stopped and it is
`failed`, `stopped`. A run whose output and errors together pass 64 MiB is stopped as `output_too_large`. The report
is read off the main thread, and a run's records keep their order: its report or failure always comes before
`closed`. Quit records each run still working as `failed`, `stopped_at_quit`; tether stops them with everything else,
and they are closed once it confirms. Quit does not acknowledge a still-suspended launch. A resume tether confirms
after Quit began is recorded `executed`, then
`failed`, `stopped_at_quit`, never `working`; no run is closed without an outcome first. A Stop that cannot reach
tether (its end of the lease is closed: the write fails with `EPIPE`, which never raises SIGPIPE in the app) leaves
the run unmarked, and tether's end settles it. If tether ends on its own while the app runs, each run still working is
`failed`, `supervisor_ended`, and closed if tether confirmed its processes gone; a launch still waiting for its answer
is `outcome_unknown` (`supervisor_ended`), read back later. Either way the session's claims end with it. A launched
run cannot become unknown (the contract allows only runner events after `executed`), so an unconfirmed one stays
unclosed: the app starts tether again (at most three times in ten minutes), whose new session first stops every
process the old one left, runs included, and then closes them. If tether is gone at Quit while a run is unclosed,
Quit starts a session only to do that sweep, then quits. After a crash, the next launch, once tether has stopped what was left: a launched run not closed is
`failed`, `interrupted`, and closed; a start whose launch went unanswered is read back from its run directory.
tether writes `started`, with the pid, only after it started a run (it creates `output` before, so `output` proves
nothing): with `started` the run started (`executed`, `read_back`, then interrupted and closed), and without it
nothing started (`failed`, `not_launched`, `read_back`).

Beacon deletes `launch` (which holds the environment, and so any token the profile exports) and `prompt` as soon as
tether has answered, when a launch fails before or without an answer, and, at the next launch, from every run
directory a crash left. `output`, `errors` and `started` stay in the run's directory, readable only by the person:
they hold what opencode wrote, the report included.

## Slack posts

**Settings.** In [Slack apps](https://api.slack.com/apps), open your workspace app (or create one from scratch)
and go to **OAuth & Permissions → User Token Scopes**. Add `chat:write`, `channels:read`, `groups:read`, `im:read`,
`mpim:read` and `users:read`. For thread previews and recovery, also add the history scopes for the conversations
you use: `channels:history`, `groups:history`, `im:history` and `mpim:history`. Install or reinstall the app in
your workspace (an admin may need to approve it). Copy the **User OAuth Token** (`xoxp-…`) into **Beacon → Settings
→ Slack** and save it. Use a non-rotating user token; Beacon does not refresh tokens. This is separate from the
collector's read credential and authorizes posting as you, not as a bot.

Beacon calls `auth.test` and shows the workspace and user names and ids it returned. It also reads
`X-OAuth-Scopes` and refuses to save a token missing one of the scopes above, naming each missing one. A
`missing_scope` answer later records the scope Slack named (`needed`) in the result, which the window shows; it
does not record Slack's `provided` list. To switch to a different
workspace or user, remove the saved account first. Only the vault holds the token, bound to `https://slack.com`,
the workspace id and `slack:user:{id}`. `settings.json` holds only the ids and names. The token is never printed,
logged, put in an action record or passed to a child process.

**Before approval.** The schema is `{ workspace, channel, thread_ts, text }`, with `thread_ts: null` for a
top-level post. Beacon verifies the saved user and workspace, checks the conversation's visibility, archive state
and membership, and for a reply confirms that `thread_ts` is a root in that conversation. Unverified destinations
cannot be approved. The window and the frozen view show the workspace name (`auth.test`'s `team`, the name
`team.info` returns), who posts, and the conversation in words: `#name` for a channel, `DM with <real name>` for a
direct message (`users.info`; `DM with yourself` when it is the person's own), and `Group DM with <names>` for a
group direct message (`conversations.members`, then `users.info`). Visibility is `public`, `private`, `DM` or
`group DM`. Names are resolved at intake and cached for ten minutes. If a name cannot be read, the window says
`unknown channel <id>` rather than hiding the destination. The raw workspace, channel and thread ids stay under
**Show exact parameters**, with the root's first line and a noninteractive plain preview above it. **Exact Slack
mrkdwn** (when the preview differs) and **Show exact parameters** show what is frozen. The names are local display:
not a record field, not part of a hash, and not sent to Signal. Slack renders mentions and other Slack-specific
formatting itself; the preview does not fetch or open links. A broadcast is marked as one, not shown as plain
text: `<!here>` previews as
**[@here — notifies everyone present]**, and `<!channel>`, `<!everyone>` and a user group (`<!subteam^…>`) likewise.
When the text holds any `<!…>`, the exact mrkdwn is open from the start. To avoid Slack splitting longer messages,
Beacon refuses drafts over 4,000 UTF-16 code units.

**After approval.** Beacon durably records the start, rechecks the user, workspace, visibility, membership and root,
then sends one `chat.postMessage` with the frozen text, channel and optional root, with link and media unfurls off.
Slack may change the destination or its permissions after the last read; Slack enforces its own rules at the write.
A verified success records `native: slack:message:{channel}:{ts}`. Slack's documented refusals that are checked
before anything is written (an archived or unknown conversation, not a member, a missing scope, `msg_too_long`,
`message_limit_exceeded`, invalid blocks or attachments, `no_text`, `restricted_action…`, refused metadata and the
like) are a definite failure. Anything else, `internal_error`, `fatal_error` and `request_timeout` included, may
follow a write and leaves the outcome unknown. A `429` uses `Retry-After` to block further calls to that Slack method
for the workspace while Beacon runs. It does not sleep and resend the old post.

**Read-back, never retry.** Beacon saves a fresh operation UUID in `started` before the write and sends it twice:
as message metadata, `{ "event_type": "beacon_post", "event_payload": { "op": "<uuid>" } }` (plan 13.00), and as
`client_msg_id`. Nothing is inserted into the text. If the reply is lost or unclear, Beacon reads the approved
conversation (`conversations.history` or `conversations.replies`, with `include_all_metadata=true`) from a minute
before the durable start to five minutes after it, up to four pages of 15. A message carries the operation when its
`beacon_post` metadata names the UUID, or when it has no `beacon_post` metadata and its `client_msg_id` is the UUID;
a message whose two ids disagree makes the outcome stay unknown. Only one such message, with byte-exact text, the
approved user and destination, and a timestamp in that window, confirms the write. Missing ids, rewritten text,
missing pages or more than one match leave the outcome unknown.

Read-back never posts. It runs right after the lost reply, then on a backoff (15, 30, 60, 120 seconds) until a
minute after the search window closes; after that, once at each relaunch and whenever the person clicks _Read
again_ on the unknown outcome. Its reads have their own budget (two at once, 60 an hour), and a `429` met while
reading back blocks only read-back, not a new proposal's preflight or intake.

**Checked against live Slack (23 Nov 2026).** A user-token (`xoxp`) post to the person's own DM,
`D08Q5C2G98W` / `1795468960.789859`, was read back with `conversations.history` and `include_all_metadata=true`.
Both identifiers were intact: `client_msg_id`, and metadata
`{ "event_type": "beacon_post", "event_payload": { "op": "<uuid>" } }`. Slack still documents
`metadata_must_be_sent_from_app` for a token that may not post metadata; that refusal fails the post as
`metadata_refused` and nothing is written. If a later post keeps neither id, a lost reply stays unknown and is
never sent again. The TLS fake covers each case and that a dropped reply causes only one wire POST. The handler
is not a live Bosun cutover.

The app sends these requests only to `https://slack.com`; `beacon egress` lists it for the saved account. Intake
lookups have a four-second deadline, so `beacon act propose` answers within its ten seconds. The window, the final
reads, the write and read-back use their own client with a 25-second deadline, so a slow `chat.postMessage` is not
cut short into an unknown outcome. Every answer is capped at 8 MiB, and no redirect is followed. The Swift fixture
tests need Go on `PATH`; they use a loopback TLS server and a fixture vault key, not a live Slack token or the
Keychain. The fixture clients use generous deadlines, because the whole suite's fixtures run at once and a slow
runner can take longer than the production deadlines to schedule a request.

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
