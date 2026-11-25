# beacon — agent notes

Read [`plans/README.md`](plans/README.md) and [`docs/trust-model.md`](docs/trust-model.md) before writing code.
The trust model is the product. A change that weakens one of its properties is wrong even if the tests pass.

## Rules

- Default deny. Code that sends must go through the rules engine and the projection. There is no other path to
  the network for item content.
- Credential values stay behind redacting secrets and bound vault APIs. Logs never contain item content, rule
  matches on content, tokens or request bodies. Do not pass credentials to children.
- Every host Beacon contacts is declared where `beacon egress` reads it. Adding a host without adding it there is
  a bug.
- Agents and CI never read the person's Keychain, vault, or configuration, or start, stop or install a live service.
  Use temporary directories, fixture secrets, and fake service managers. Only the person runs the real installer,
  the one-time collector import, and the Swift cutover. The installer has a separate `--dry-run` with no live jobs.
- No telemetry, update checks or crash reporting.
- New dependencies need a reason in the pull request. Prefer the standard library.
- Docs describe the code as it is. When behaviour changes, change the doc in the same pull request.

## Gates

Once code exists: `gofmt`, `go vet`, `staticcheck`, `go test ./...` with the race detector, and a reproducible
build check. All must pass.
