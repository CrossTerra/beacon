# Trust model

Beacon sits between accounts that belong to one person and a store that many people and agents read. This file
says what Beacon protects, from whom, and how. Code that changes any of these properties changes this file in the
same pull request.

## What crosses the boundary

| Leaves the machine | Never leaves the machine |
| --- | --- |
| Events a rule sent: the projected fields only | Source credentials (OAuth refresh tokens, API tokens) |
| The Signal key's bearer header, to the configured Signal host | Items the rules dropped or that are waiting for review |
| Requests to the source APIs, made with your credentials | The rules file, the review queue, the outbox, classifier input and output |

## Properties

1. **Default deny.** An item with no matching `send` or `hold` rule is dropped. Adding a source sends nothing
   until a rule names it.
2. **Rules decide; the classifier only narrows.** The classifier sees items the rules already allowed. It can
   hold or drop them. It cannot send an item the rules denied, and it cannot add fields.
3. **Projection is explicit.** Each source has a fixed list of fields. A rule lists the fields it sends. Message
   bodies, attachments and file contents leave only when a rule lists them. Attachments and file contents are
   out of scope for the first release.
4. **Local classifier by default.** The classifier runs on the machine. Using a hosted model is an explicit
   setting, shown by `beacon egress`, because it sends item content to that model's vendor before the filter
   decides.
5. **The outbox is the record.** Every request body is appended to the outbox before it is sent. Retries resend
   those exact bytes, which is also what Signal requires of a retried key.
6. **Fixed egress.** Beacon connects only to the source APIs, the configured Signal host and, if configured, a
   hosted classifier. `beacon egress` prints the list. There is no telemetry, no update check and no crash
   reporting.
7. **Credentials stay in the keychain.** Beacon reads them when it needs them and never writes them to disk,
   logs or the outbox.
8. **Reproducible and signed.** Release builds are reproducible from a tagged commit. Releases are signed, macOS
   builds are notarized, and each release lists its dependencies (SBOM).

## Known limits

- **Signal cannot delete.** Events are immutable and redaction is not built. A rule that sends too much cannot be
  undone after the send. New rules should start as `hold` until their output has been checked.
- **Signal has no per-event visibility yet.** Until it does, any key that can read the store can read every event
  Beacon sends, including full bodies a rule allowed. Rules that send bodies from personal sources (direct mail,
  direct messages, private files) should wait for visibility, or be limited to content the whole organization may
  read.
- **Beacon trusts the machine it runs on.** Anyone with your user account can read the outbox, the queue and the
  keychain items Beacon uses.
- **Beacon trusts the Signal host it is pointed at.** It checks TLS. It does not protect what the store does with
  data after it arrives.
