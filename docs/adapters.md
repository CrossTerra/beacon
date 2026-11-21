# Write an ingestion adapter

Start by agreeing with the person on which account and objects to read, which changes are useful, and what can
appear in Signal. Default deny applies to a new source. As part of the design, list the provider permissions,
origins, request methods and paths, data read, changes emitted, fields, state fields and local files. Review
[the trust model](trust-model.md) before implementing it.

## Collect into items

1. Implement [`source.Source`](../internal/source/source.go) with a stable name, a fixed field list, a fixed
   state field list, `Hosts` and `Collect`. Start with
   [the fixture source](../internal/source/fixture.go) and an existing service source such as
   [GitHub](../internal/github/github.go). Return items and an opaque next cursor. Let the runner decide
   and record each item before it commits the cursor. Collectors can run at the same time. Keep per-source
   collection state out of the shared send state.
2. Define a stable subject for the native object and a key of the form `<source>:<native id>:<version>`
   for each change. Use a provider change ID, revision or timestamp from the change itself. Re-reading the same
   change must yield the same key. A snapshot has `observed_at` and the version also used at the end of its key.
   Use `source.ActNative` for an observation of an act. The ID is opaque, carries no content or `@`, and stays in
   the plain envelope. Use explicit subject references and a short change title that carries no object title
   or message text. Treat the object title, state, references, body and other actors as opt-in fields.
3. Establish `own` from the authenticated account and the provider's record of who acted. Establish
   `addressed` from a provider assignment, request or mention. Keep both evidence paths testable. The
   projection records the person's own identity in `by` when configured, and another actor only when a rule
   lists `actor`.
4. With no cursor, read bounded open state. Respect pagination, API limits and cancellation. Test an
   interrupted pass and a retry so partial or failed reads cannot silently skip changes. Use the shared
   [rate-limit client](../internal/limited/limited.go) where it fits. Leave first-run acceptance to the runner
   and CLI. Add the kind, validated configuration and interval in [`internal/config/config.go`](../internal/config/config.go)
   and wire the source in [`cmd/beacon/sources.go`](../cmd/beacon/sources.go).

## Keep the trust boundary

- Give the adapter a lazy credential callback returning `keychain.Secret`. Keep input parsing, secret bytes
  and storage in [`internal/keychain`](../internal/keychain). Use the package's header helpers only for a
  request already checked against the provider origin. Accept the narrowest usable read scopes. Where the grant
  is broader than the collector, describe the difference. Keep secrets out of config, errors, logs and fixtures.
- Declare every connected host, including token exchanges and setup calls, in the listing used by
  [`internal/egress`](../internal/egress/egress.go). Implement `Hosts` and use the guarded HTTP client passed
  through `SetHTTP` in [`cmd/beacon/app.go`](../cmd/beacon/app.go). For reference see
  [the Google host declarations](../internal/google/api.go) and [Slack setup](../cmd/beacon/sources.go).
  Validate the HTTPS origin, method, paths, query and any provider-supplied next link before attaching a secret.
  Reuse redirect refusal and the transport that ignores environment proxies. Links copied into items are data.
- Read as little as the API allows. Add `SetBodies` for an API that can omit text and wire the flag to rules
  that can request it. For a personal source, gate a body read on sealed rules that list `body`. If the API
  returns text with metadata, document that local read and ensure text leaves only through projection.
  Keep attachments and arbitrary file contents outside collection. See [Google](../internal/google/gmail.go)
  and [Slack](../internal/slack/slack.go).
- For a personal subject, namespace it with the person's handle. Give the source a `PersonalStem` and add any
  new stem to the fixed list in [`internal/source/source.go`](../internal/source/source.go). Check rule
  validation for entirely personal and mixed sources in [`internal/rules/check.go`](../internal/rules/check.go)
  and [`cmd/beacon/app.go`](../cmd/beacon/app.go). Pending and rejected entries must keep their privacy check
  after a source is disabled. Keep bodies sealed on personal subjects and hold sealed items on shared subjects.
- Return items to [the runner](../internal/runner/runner.go). Keep decisions, projection, sealing, outbox writes
  and Signal sends in the shared pipeline. Check that `seal.owner` and the subject floor can only raise, and
  that a new field can leave only when a rule lists it. A code change that changes a trust property also changes
  [the trust-model document](trust-model.md).

## Test and review

Use synthetic API responses with `httptest`, `keychain.NewMemory`, the
[`egresstest` guard](../internal/egress/egresstest/egresstest.go) and
[`signaltest`](../internal/signal/signaltest/signaltest.go). Add adapter tests for open state, each change type,
own versus other acts, a stable key across repeat reads, pagination, limits, unavailable auth and a cursor on
failure. Assert that requests outside the provider scope, cross-origin pagination and redirects receive no
credential. Check the hosts listed by `beacon egress` against the hosts seen by the test transport.

Exercise the adapter through the CLI or runner with fixture rules. Check that unmatched, held and dropped items
produce no event and that a send contains exactly the listed fields and expected envelope. Test any personal
body, unknown restriction and retry paths the source adds. Check that logs carry only allowed attributes, even on
provider errors. Document the configuration, provider grant and revocation, collection limits, fields, state,
subjects and change markers in [Sources](sources.md) and [Configuration](configuration.md). Run
[`scripts/gates.sh`](development.md#gates) for code changes. Justify any dependency in the PR.

Keep the adapter in the person's fork while testing. If it is useful to other installations, show the person the
exact generic diff, trust and egress changes, fixture evidence and upstream target. Send an upstream PR only
after their explicit yes. Keep private rules, credentials, settings, logs and provider responses outside the
contribution.
