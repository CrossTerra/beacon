# Trust model

Beacon sits between accounts that belong to one person and a store that many people and agents read. This file
says what Beacon protects, from whom, and how. Code that changes any of these properties changes this file in the
same pull request.

## What crosses the boundary

| Leaves the machine | Never leaves the machine |
| --- | --- |
| Events a rule sent: the projected fields and state fields only | Source credentials (OAuth refresh tokens, API tokens) |
| The Signal key's bearer header, to the configured Signal host | Items the rules dropped or that are waiting for review |
| Requests to the source APIs, made with your credentials, including on a dry run | The rules file, the review queue, the outbox, the cursors, the seed file, classifier input and output |
| Lease claims: your Signal handle, a random instance id and the lease period | Rule names, and why an item matched |
| Your own identity in a source, as `by`, on your own acts | Who else caused a change, unless a rule lists `actor` |

## Properties

1. **Default deny.** An item with no matching `send` or `hold` rule is dropped. Adding a source sends nothing
   until a rule names it. A rule condition with no value is an error, not "match anything".
2. **Rules decide; the classifier only narrows.** The classifier sees items the rules already allowed. It can
   hold or drop them. It cannot send an item the rules denied, and it cannot add fields. (No classifier exists
   yet; it arrives in Phase 6.)
3. **Projection is explicit.** Each source has a fixed list of fields, and an item with any other field is refused.
   A rule lists the fields it sends. Message bodies, attachments and file contents leave only when a rule lists
   them. Attachments and file contents are out of scope for the first release. A state snapshot carries only the
   state fields a rule lists (DEC-28). Who caused a change leaves only when a rule lists `actor`; your own identity
   leaves as `by` on your own acts (DEC-27).
4. **Local classifier by default.** The classifier runs on the machine. Using a hosted model is an explicit
   setting, shown by `beacon egress`, because it sends item content to that model's vendor before the filter
   decides.
5. **The outbox is the record.** Every request body, lease claims included, is appended to the outbox and synced
   to disk before it is sent. Retries resend those exact bytes, which is also what Signal requires of a retried
   key. A body Signal refused is recorded and never rewritten.
6. **Fixed egress.** Beacon connects only to the source APIs, the configured Signal host and, if configured, a
   hosted classifier. `beacon egress` prints the list, and every HTTP request goes through a transport that
   refuses any other host before connecting. Proxy settings from the environment are ignored and redirects are not
   followed. There is no telemetry, no update check and no crash reporting.
7. **Credentials stay in the keychain.** Only the keychain package reads them, when they are needed. They are
   never written to disk, logs or the outbox; a credential prints as `[redacted]` however it is formatted. The
   Signal URL must be `https`, except to a loopback address.
8. **Logs carry no content.** Log lines have fixed event names and carry counts, source names, outcomes, statuses,
   Signal error codes and fixed reason codes: no item content, no rule matches, no keys, no bodies, no tokens, and no
   error text, which can name any of those. `beacon run` prints a fixed message for each reason.
9. **One sender per person.** Only the instance holding the lease sends (DEC-25), and no write outlasts the lease.
   Another instance collects and keeps its bodies pending.
10. **Reproducible and signed.** Builds are reproducible from a commit (checked by `scripts/repro.sh` on every pull
    request). Signed releases, notarized macOS builds and an SBOM per release arrive in Phase 7.

## Known limits

- **Signal cannot delete.** Events are immutable and redaction is not built. A rule that sends too much cannot be
  undone after the send. New rules should start as `hold` until their output has been checked.
- **Signal has no per-event visibility yet.** Until it does, any key that can read the store can read every event
  Beacon sends, including full bodies a rule allowed. Rules that send bodies from personal sources (direct mail,
  direct messages, private files) should wait for visibility, or be limited to content the whole organization may
  read.
- **Other people are attributed in metadata, not `by`.** Signal accepts `by` only for identities an actor already
  holds, so Beacon records other people in `metadata.actor` (DEC-27). Signal treats it as plain data: it does not
  link it to an actor or let a search by actor find it.
- **Asks are tracked locally.** Beacon knows which asks are open from its own outbox. If the person closes an ask in
  Signal directly, Beacon's next own act on that subject still names it in `closes`, Signal refuses the event with
  `409 conflict`, and the event is recorded as rejected instead of sent.
- **Signal does not keep state snapshots yet.** Beacon sends the state fields a rule lists (DEC-28). Signal `main`
  accepts them and discards them until its plan 12.01; they still leave the machine, so list state fields with the
  same care as other fields.
- **The hold queue holds content at rest.** Held items are stored in full in the state directory (`0600`) until
  reviewed.
- **The lease follows the local clock.** A handover takes up to two lease periods, and machines whose clocks differ
  by more than a period may disagree on the current period.
- **Beacon trusts the machine it runs on.** Anyone with your user account can read the outbox, the queue and the
  keychain items Beacon uses.
- **Beacon trusts the Signal host it is pointed at.** It checks TLS. It does not protect what the store does with
  data after it arrives.
