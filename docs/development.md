# Development

Go 1.24 (see `go.mod`). On macOS the keychain uses the Security framework through cgo; elsewhere Beacon builds
without it and `beacon init` and `beacon run` in send mode report that there is no system keychain.

## Gates

```bash
scripts/gates.sh
```

runs every gate from [`AGENTS.md`](../AGENTS.md): `gofmt -l`, `go vet`, `staticcheck`, `go test -race`, and
`scripts/repro.sh`, which builds the committed tree twice from two copies at different paths with `-trimpath` and
requires identical bytes. That check runs on one machine with one toolchain: it shows the build does not depend on
the checkout path or on timestamps, not that another machine reproduces the same bytes; Phase 7 adds that. CI
(`.github/workflows/ci.yml`) runs the same script on macOS and Linux for every pull request, with its actions pinned
to commit SHAs.

On macOS with recent Xcode, `go test -race` prints `ld: warning: … malformed LC_DYSYMTAB`; it is a known warning
from the linker and does not affect the tests.

## Tests

| Where | What |
| --- | --- |
| `internal/rules` | Every match type, first match wins, default `never`, unknown and blank keys rejected, every YAML spelling of `own` and `addressed`, one document only, `state` lists, unreachable rules, the own-act identity warning. |
| `internal/project` | Every subset of fields: an unlisted field never appears in the bytes. `by`, `to`, `closes`, `priority`, `due_at`, the state snapshot. An event title describes the change; the object's title stays on the thread and leaves only when listed. A retry keeps the event title and not an unlisted body. |
| `internal/outbox` | Append, mark, reload; torn last line; tampered body; the lock; expired lease claims; a refused event requeued, and a sent, replayed, pending, lease, or 409 entry not. |
| `internal/queue` | Hold and drop; a torn last line is cut, under the outbox lock, so the next append stays readable; read-only opens change nothing and refuse writes; opening for writing needs the outbox lock on the same directory. |
| `internal/signal` | 201, replay, 409, 422, 401, 429 with `Retry-After`, 5xx backoff with the same bytes, the egress guard, redirects not followed, a deadline ending a wait. |
| `internal/egress` | The host list; the guard; the production transport ignores proxy settings in the environment. |
| `internal/lease` | One holder; renewal; takeover within two periods; restart keeps the lease; claim shape; stale claims expire; refusals; a claim ends with its period. |
| `internal/runner` | A pass on its own: dry run writes nothing, send, the dropped record does not grow, a key dropped earlier by the same rule is reported as skipped, the lease bounds a send, lease errors fail the pass, error text never reaches the log, error classes, the cursor moving only after a send pass decides every item, a rate limit and an unaccepted first run not moving it, a bad item and a projection error dropped by count without freezing the cursor, a seed `never` dropping before the rules file. |
| `internal/github`, `internal/jira` | Every item type against `httptest` fixtures: own acts, mentions, pagination, rate-limit backoff, a cursor that does not move on a rate limit, keys stable across a pass, no request outside the configured repositories, projects and hosts, a 15-minute lookback and a server `Date` cursor, pushes not filtered by commit time, a bare 403 failing the pass, team review requests, a restart that does not send twice, an event title that describes the change and does not repeat the object title or include comment text. The starter rules against the GitHub fixtures. Golden request bodies for a review request and a Jira transition. |
| `internal/cursor`, `internal/seed`, `internal/limited` | Cursor commit, a completed first run, and acceptance; seed parse; rate-limit wait and early end. |
| `cmd/beacon` | The commands against a fake Signal, through the egress-recording transport wrapped around the production transport: tiers, nothing held or dropped on the wire, `beacon egress` covers every host seen, a crash between append and send, a crash after send, a retry after a rules change, 409, a second instance, logs, exit codes on refusal, bodies printed only on request, error text kept off standard error, a dry run and explain leaving the state directory byte-for-byte unchanged, an interrupt during `init` writing nothing, `source add` not printing or writing a token, a piped Jira token kept, `--from-gh --host` passing the host, `accept-first` requiring a completed `--first`, egress listing GitHub and Jira hosts, `outbox retry --rejected` re-projecting a 400 and not touching a 409, a sent entry, or an entry with no stored item, `cursor reset` clearing a cursor without sending, and a legacy `400` with no stored item sent once after cursor reset and `beacon run --once`. |
| `examples` | The starter rules file checks clean against the GitHub and Jira field lists. |

The crash tests run the test binary as a helper process that stops itself with `os.Exit` at the exact point: after
the body is appended and synced, or after Signal answered and before the outbox records it. `os.Exit` skips deferred
calls and leaves no chance to flush, like a kill, but the operating system still closes the files; a power loss,
where the page cache is lost, is covered by the syncs rather than by a test.

`internal/egress/egresstest` is the test transport: it fails the test on any request to a host not on the list it
was given, and records every host it saw. The command and end-to-end tests put it in front of the production
transport (`egress.Transport`), not `http.DefaultTransport`, so proxy settings in the environment cannot change what
they test.

`TestSystem` in `internal/keychain` uses the real login keychain and runs only with `BEACON_TEST_KEYCHAIN=1`.

## End to end against Signal

```bash
scripts/signal-e2e.sh up        # needs Docker and a Signal checkout at ~/Repos/signal (or SIGNAL_REPO)
set -a; . ./.e2e.env; set +a
go test -race -count=1 -run E2E -v ./cmd/beacon/
scripts/signal-e2e.sh down
```

`up` builds Signal's image from `git archive` of `SIGNAL_REF` (default `main`), so the Signal checkout's branch and
working tree do not matter. It starts Signal and Postgres 17 on a private Docker network with Signal on
`127.0.0.1:18080`, creates the person `@winton` with the identity `fixture:user:winton`, and Beacon's integration
actor with `write`, `read` and `relay` limited to `fixture:*`. It writes the URL and keys to `.e2e.env` (ignored by
git). `TestE2E` is skipped without them, and CI does not run it.

It checks, against real Signal: a dry run contacts nothing; a send pass writes the expected events into the
expected threads, with the ask to the person, `by` on the own act, and each ask closed by the right event; an event
that carries a state snapshot is accepted (`201`) and stored with the listed fields under `state.fields`; held and
dropped items have no event and no thread; a crash between append and send produces one event; a crash after send
is recorded as replayed; a 409 stops the key; and a second instance sends nothing while the lease is held.

## Dependencies

| Module | Why |
| --- | --- |
| `go.yaml.in/yaml/v3` | The rules file is YAML (DEC-5) and the standard library has no YAML parser. This is the YAML organisation's maintained copy of `gopkg.in/yaml.v3`. It reports duplicate keys and unknown fields, which the rules file needs. |

Everything else is the standard library.

## Local builds and the keychain

`go build` signs the binary ad hoc, so each rebuild has a new code signature. The keychain treats every rebuild as a
different program, and each read of a Beacon secret asks for the login password again; "Always Allow" does not
survive the next build.

For local use, sign with a stable identity instead:

1. Once: `scripts/dev-signing-setup.sh` creates the self-signed identity "Beacon Local Signing" in the login
   keychain and trusts it for code signing for your user. macOS asks for your login password twice: to let
   `codesign` use the key, and to add the trust setting.
2. Each build: `scripts/build-local.sh` builds with the same flags as `scripts/repro.sh`, signs with that identity
   and the identifier `dev.beacon.cli`, and installs to `~/.local/bin/beacon`.
3. The first read of each secret after switching asks once. Choose "Always Allow". Later rebuilds keep access,
   because the keychain checks the identifier and the signing certificate, not the binary's hash.

Do not work around the prompt by letting every application read the items (`security … -A`) or by reading them
through `/usr/bin/security`: either lets any process running as you read Beacon's credentials without asking.
Tests never touch the login keychain unless `BEACON_TEST_KEYCHAIN=1`. Release builds are signed with a Developer ID
(Phase 7).
