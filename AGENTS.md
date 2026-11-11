# beacon — agent notes

Read [`plans/README.md`](plans/README.md) and [`docs/trust-model.md`](docs/trust-model.md) before writing code.
The trust model is the product. A change that weakens one of its properties is wrong even if the tests pass.

## Rules

- Default deny. Code that sends must go through the rules engine and the projection. There is no other path to
  the network for item content.
- Nothing reads, logs or writes a credential except the keychain package. Logs never contain item content, rule
  matches on content, tokens or request bodies.
- Every host Beacon contacts is declared where `beacon egress` reads it. Adding a host without adding it there is
  a bug.
- Agents and scripts never touch the login keychain. Test with `go test` or a `go build` binary: an ad-hoc build
  refuses the keychain. Do not run `scripts/build-local.sh`, set `BEACON_TEST_KEYCHAIN`, or run the installed
  `beacon` for a command that reads a credential; each can raise a prompt on the person's screen or add a build to
  an item's access.
- No telemetry, update checks or crash reporting.
- New dependencies need a reason in the pull request. Prefer the standard library.
- Docs describe the code as it is. When behaviour changes, change the doc in the same pull request.

## Gates

Once code exists: `gofmt`, `go vet`, `staticcheck`, `go test ./...` with the race detector, and a reproducible
build check. All must pass.
