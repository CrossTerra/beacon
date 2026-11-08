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
| `internal/project` | Every subset of fields: an unlisted field never appears in the bytes. `by`, `to`, `closes`, `priority`, `due_at`, the state snapshot. |
| `internal/outbox` | Append, mark, reload; torn last line; tampered body; the lock; expired lease claims. |
| `internal/queue` | Hold and drop; a torn last line is cut so the next append stays readable. |
| `internal/signal` | 201, replay, 409, 422, 401, 429 with `Retry-After`, 5xx backoff with the same bytes, the egress guard, redirects not followed, a deadline ending a wait. |
| `internal/egress` | The host list; the guard; the production transport ignores proxy settings in the environment and follows no redirect. |
| `internal/lease` | One holder; renewal; takeover within two periods; restart keeps the lease; claim shape; stale claims expire; refusals; a claim ends with its period. |
| `internal/runner` | A pass on its own: dry run writes nothing, send, the dropped record does not grow, the lease bounds a send, lease errors fail the pass, error text never reaches the log, error classes. |
| `cmd/beacon` | The commands against a fake Signal, through the egress-recording transport wrapped around the production transport: tiers, nothing held or dropped on the wire, `beacon egress` covers every host seen, a crash between append and send, a crash after send, a retry after a rules change, 409, a second instance, logs, exit codes on refusal, bodies printed only on request, error text kept off standard error. |

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
expected threads, with the ask to the person, `by` on the own act, and each ask closed by the right event, and Signal
accepts the event that carries a state snapshot; held and
dropped items have no event and no thread; a crash between append and send produces one event; a crash after send
is recorded as replayed; a 409 stops the key; and a second instance sends nothing while the lease is held.

## Dependencies

| Module | Why |
| --- | --- |
| `go.yaml.in/yaml/v3` | The rules file is YAML (DEC-5) and the standard library has no YAML parser. This is the YAML organisation's maintained copy of `gopkg.in/yaml.v3`. It reports duplicate keys and unknown fields, which the rules file needs. |

Everything else is the standard library.
