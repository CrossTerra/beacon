# Development

Go 1.24 (see `go.mod`). On macOS the keychain uses the Security framework through cgo; elsewhere Beacon builds
without it and `beacon init` and `beacon run` in send mode report that there is no system keychain.

## Gates

```bash
scripts/gates.sh
```

runs every gate from [`AGENTS.md`](../AGENTS.md): `gofmt -l`, `go vet`, `staticcheck`, `go test -race`, and
`scripts/repro.sh`, which builds the committed tree twice from two copies at different paths with `-trimpath` and
requires identical bytes. CI (`.github/workflows/ci.yml`) runs the same script on macOS and Linux for every pull
request.

On macOS with recent Xcode, `go test -race` prints `ld: warning: … malformed LC_DYSYMTAB`; it is a known warning
from the linker and does not affect the tests.

## Tests

| Where | What |
| --- | --- |
| `internal/rules` | Every match type, first match wins, default `never`, unknown and blank keys rejected, unreachable rules. |
| `internal/project` | Every subset of fields: an unlisted field never appears in the bytes. `by`, `to`, `closes`, `priority`, `due_at`. |
| `internal/outbox` | Append, mark, reload; torn last line; tampered body; the lock. |
| `internal/signal` | 201, replay, 409, 422, 401, 429 with `Retry-After`, 5xx backoff with the same bytes, the egress guard. |
| `internal/lease` | One holder; renewal; takeover within two periods; restart keeps the lease; claim shape. |
| `cmd/beacon` | The commands against a fake Signal, with the egress-recording transport: tiers, nothing held or dropped on the wire, `beacon egress` covers every host seen, a crash between append and send (a real process killed at that point), a crash after send, a retry after a rules change, 409, a second instance, logs. |

`internal/egress/egresstest` is the test transport: it fails the test on any request to a host not on the list it
was given, and records every host it saw.

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
expected threads, with the ask to the person, `by` on the own act, and each ask closed by the right event; held and
dropped items have no event and no thread; a crash between append and send produces one event; a crash after send
is recorded as replayed; a 409 stops the key; and a second instance sends nothing while the lease is held.

## Dependencies

| Module | Why |
| --- | --- |
| `go.yaml.in/yaml/v3` | The rules file is YAML (DEC-5) and the standard library has no YAML parser. This is the YAML organisation's maintained copy of `gopkg.in/yaml.v3`. It reports duplicate keys and unknown fields, which the rules file needs. |

Everything else is the standard library.
