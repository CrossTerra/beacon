# beacon

Beacon is a small agent that runs on your own computer. It reads the services you work in (GitHub, Jira, Gmail,
Google Calendar, Google Drive, Slack), decides locally which changes are actionable, and sends only those to a
[Signal](#signal) event store. Agents and people then work from Signal instead of each polling every service.

Beacon is the trust boundary between your accounts and a shared store. Your source credentials never leave the
machine. Nothing leaves it unless a rule you wrote allows it, and every byte that does leave is kept in a local
log you can read.

**Status:** design. There is no code yet. The design and the work plan are in [`plans/`](plans/README.md), and
the trust model is in [`docs/trust-model.md`](docs/trust-model.md).

## Why a local agent

A connector that runs on a server needs a copy of your credentials and sees everything those credentials can
see. It then has to filter on someone else's machine. Beacon turns that around:

- **Your credentials stay with you.** Beacon uses your own OAuth tokens and API tokens, stored in the operating
  system's keychain. The store never receives them.
- **You decide what leaves.** A readable rules file says which sources, projects, repositories, channels and
  senders may send, and which fields of each item may go with them. The default is to send nothing.
- **You can check what left.** Beacon writes the exact request body of every send to a local, append-only
  outbox. `beacon outbox` shows it.
- **You can check Beacon itself.** The code is open, builds reproducibly, and talks only to the source APIs and
  the store you configure. `beacon egress` lists those hosts.

## How it works

```
sources ──► collectors ──► candidate items (local only)
                               │
                               ▼
                       rules file (yours)  ──►  never: dropped, logged locally
                               │
                     optional local classifier (may only narrow)
                               │
                               ▼
                       field projection per source
                               │
                 ┌─────────────┴─────────────┐
                 ▼                           ▼
          hold: review queue          send: outbox ──► Signal
          (you approve)                 (exact bytes kept)
```

1. **Collectors** pull changes from each source with your credentials and keep a cursor per source.
2. **Rules** decide each item's tier: `send`, `hold` (wait for your approval), or `never`. Rules are plain text,
   reviewed like code, and the first match wins.
3. **A classifier** (optional, local by default) can answer judgment questions such as "is this a request to
   me?". It can move an item from `send` to `hold` or `never`. It can never send something the rules did not
   allow.
4. **Projection** keeps only the fields a rule names. A rule may allow full message bodies for a sender or a
   channel. Without that, an email leaves as its id, sender, subject, date and link.
5. **The outbox** records each request before it is sent, and retries resend the same bytes.

## Signal

Signal is an append-only event store for people and agents that share attention. Beacon writes to its HTTP API
(`POST /api/events`) with a key bound to a Beacon identity for one person. Events are immutable there, which is
why Beacon filters before it sends and never after.

## License

[Apache License 2.0](LICENSE).
