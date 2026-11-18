# Phase 11 — Sources on their own loops; coverage is reported

Status: built. What was built is described in [`docs/commands.md`](../docs/commands.md#beacon-run),
[`docs/configuration.md`](../docs/configuration.md#sources), [`docs/architecture.md`](../docs/architecture.md#source-loops)
and [`docs/events.md`](../docs/events.md#coverage). Decisions: DEC-41, DEC-42.

## Goal

A change in a source reaches Signal as soon as that source can read it, not after every other source has been read,
and a source that stops reading is visible in Signal rather than only in a local log.

## Why

A measurement over two days (915 passes) of the running agent:

- One pass read GitHub, Gmail, Calendar, Drive, Jira and Slack in turn, then waited a fixed 60 seconds. A pass took
  81 seconds at the median; GitHub, first, about 29 seconds, and Slack, last, about 40.
- A Slack message reached Signal at a median of about 160 seconds and a 90th percentile of about 12 minutes. The tail
  is the quiet channels, read every 15 minutes and at most 40 a pass.
- The laptop sleeping left long stalls that looked like long passes.
- Gmail refused its grant for six hours. Each pass logged `collect_failed`, and nothing in Signal showed it.

## Scope

1. **A loop per source (DEC-41).** Each source makes a pass, waits its own interval, and makes the next. Defaults:
   Slack 15 s, Gmail 30 s, GitHub 60 s, Jira 60 s, Calendar and Drive 120 s; `sources[].interval` overrides, at least
   1 s and at most half the lease period. A fixture keeps `poll_interval`, which also stays the base of the backoff
   after a failure. A source's collect runs while other sources' passes decide and send; deciding, recording and
   sending is one pass at a time under one lock, and every pass reads the cursors and the acceptance again first. Each
   source's own rate limiting (Slack's per-method spacing, Gmail's quota budget, `Retry-After` holds) is unchanged and
   holds across passes, so a shorter interval cannot exceed an API's limit. A pass that fails the run stops every
   loop and goes through the existing backoff. `--once` and `--first` stay one sequential pass.
2. **Wake from sleep.** Every five seconds the scheduler compares the wall clock with the monotonic clock, which does
   not run while the machine sleeps (macOS and Linux). A difference of 30 seconds or more is a sleep: every source
   makes a pass at once, the log says `woke` with how long, and `coverage.json` records the span. Pass times in the log
   (`collected` `ms`) and in coverage are monotonic. A pass right after a wake that fails is tried again 10 seconds
   later.
3. **Coverage (DEC-42).** After each source pass the send loop records the source's coverage in `coverage.json`:
   interval, last pass and how long its collect took, last success, last failure with its reason code, failures in a
   row and since when, the last pass's `cut`, `history_expired`, `paused`, `rate_limited` and names flags, and for Slack
   the channels it reads, read this pass, never read, waiting past the per-pass limit, with a gap, and the oldest last
   read. `beacon status --coverage` prints it. While the instance holds the send lease it reports to Signal the way
   Signal's executors keep a heartbeat (Signal DEC-427): a heartbeat lease per source, renewed after a pass that
   collected, and a `beacon.coverage` state snapshot on `beacon:coverage:<handle>:<source>` when the coverage changes.
4. **Tests and docs.**

## Out of scope

- Slack Socket Mode ([phase 12](phase-12.md)). Slack's latency after this phase is its own pass (about 40 seconds,
  bound by the per-method spacing) plus its interval, and quiet channels still wait up to 15 minutes.
- Changing what a pass reads, the per-pass limits, or the quiet-channel interval.
- An ask to the person when a source fails (DEC-12: Beacon asks only when a rule says so). A reading agent can ask
  from the snapshot.
- Reloading the config while running.

## Design

### Loops

`internal/schedule` runs one goroutine per source and one that watches the clocks. `runner.Run` is split into
`runner.Gather`, which collects one source and touches nothing the runner holds, and `runner.Process`, which is the
rest of the pass (absorb, decide, record, commit, send, retitle). The app's pass for a source takes the lock to read its
cursor, releases it to collect, and takes it again to read the store again and process. A thread retitle uses only
the pass's sources: another source may be collecting, and a titler reads the source's state. Cursors are read again
under the lock before processing, so one source's commit cannot overwrite another's with a stale store.

`pass.json` is written after every source pass; a personal source's `withheld:` line stays until that source's next
pass. SIGTERM ends every loop after the pass it is making, with no further send, snapshot or heartbeat.

### Coverage in Signal

Two parts, as the send lease has two (DEC-39):

- **Heartbeat lease** `beacon:heartbeat:<handle>:<source>`, run `<instance>:heartbeat`, time to live three intervals
  and at least six minutes, renewed after a pass that collected once a third of it has passed. A row, not an event:
  liveness without filling the log. A failing source, or a stopped or sleeping machine, lets it expire. `404` from a
  Signal without leases turns heartbeats off for an hour. Another instance's heartbeat is `409` until it expires.
- **Snapshot** `beacon.coverage` on `beacon:coverage:<handle>:<source>`, keyed by the subject and the time in
  milliseconds, with the coverage in `state.fields` so Signal keeps the latest per source. Written at a source's first
  pass, then only when the status, its reason, a flag, the interval, or the shape of Slack's reach changes, at most once
  in five minutes per source, and not on a single failure (two in a row is failing). It goes through the outbox (kind
  `coverage`): appended and synced first, a pending one sent first as stored, a refused one recorded.

Nothing in either is an item, a key, an id, a name or an error's text. Both are bounded by the send lease's `until`.

## Tests

- `internal/schedule`: the sleep arithmetic (a long pass is not a sleep, a short nap under the threshold, a clock set
  back); a slow source not delaying another; a wake running every source at once, once, and an ordinary check after it
  not another; a failed pass after a wake retried soon; a pass error stopping every loop; Stop waiting for the pass
  under way.
- `internal/coverage`: observe and status (one failure is not failing, two are, recovery keeps the last failure,
  Slack's waiting and behind channels); the record's file mode, round trip, and refusal of free text; the status text;
  the reporter writing on change, at most once in five minutes, nothing on one failure, no heartbeat for a failing
  source, a pending snapshot sent first as stored, nothing after the lease, a Signal without leases.
- `internal/slack`: a pass's reach over member channels past the per-pass limit, and the next pass's.
- `internal/config`: kind defaults, `interval` overrides, its bounds.
- `cmd/beacon`: a send loop with a fixture and a Gmail source whose grant is refused: the fixture keeps passing while
  Gmail fails; Gmail is failing with `source_unauthorized` in `coverage.json`, `beacon status --coverage` and a Signal
  snapshot; the fixture holds a heartbeat and Gmail none; no token or item content anywhere. A dry-run loop writes no
  coverage. The running loop sees `accept-first` on Gmail's own interval.

## Exit gate

`scripts/gates.sh` passes. A simulation of the measured pass times through the scheduler with fake sources (one
simulated second = 4 ms) moves a new Slack item's collect from a median of 113 s and a 90th percentile of 169 s to 68 s
and 90 s, and Gmail's from 76 s and 133 s to 21 s and 35 s. The live agent's figures are measured again after it runs
this build for a few days.
