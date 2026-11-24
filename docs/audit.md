# Audit a Beacon build before installing

Review the exact Git commit the person will run. Record the fork URL, full commit ID, proposed configuration,
blocking findings and evidence with file paths. Do not use the person's real credentials or service for tests.

1. **Build and placement.** [`scripts/install.sh`](../scripts/install.sh) requires Go 1.24.3, refuses changes
   to tracked files, builds the Git archive and checks the resulting commit. Review both unit definitions,
   paths, stop and rollback behavior. No signing, automatic fetch or installation as root.
2. **Read and send boundaries.** [`beacon egress`](commands.md#beacon-egress) lists the provider and Signal
   hosts. Review [`internal/egress`](../internal/egress), the enabled sources, rules and projected fields;
   `hold` and `never` do not send. An approved action goes only to its bound origin, account and destination.
3. **Approval and recovery.** Follow [`internal/actsocket`](../internal/actsocket),
   [`internal/review`](../internal/review), [`internal/webauthn`](../internal/webauthn) and
   [`internal/approval`](../internal/approval). A proposal is not authority. Verify the canonical origin,
   session, assertion, PRF unwrap, approved hash, durable start, and read-back-only recovery. Check the
   exact-byte outbox and sequence numbers against Signal's fixture contract. A Swift archive is not imported.
4. **Credentials and process lifetime.** Read [`internal/vault`](../internal/vault),
   [`internal/keychain`](../internal/keychain) and [`internal/supervisor`](../internal/supervisor).
   Exporting a key file is an explicit user operation. There is no runtime Keychain access. The one-time
   collector import never prints secret values or deletes old items. Check the child environment and read/write
   separation. Same-user malware is not isolated; see [known limits](trust-model.md#known-limits).

Runtime dependencies added for the Go service: `golang.org/x/sys` for peer credentials, process tables and
`F_FULLFSYNC`; `fxamacker/cbor/v2` for WebAuthn; `godbus/dbus/v5` for Linux Secret Service and notifications;
and `tobischo/gokeepasslib/v3` for KDBX. The latter is pinned at v3.6.1 with the narrow local in-memory-key-file
backport described in `third_party/gokeepasslib/`. Its Argon2 dependencies are `golang.org/x/crypto` and
`tobischo/argon2` (including Argon2d). The independent `go-webauthn/webauthn` and Chrome driver are test-only.
There is no dependency for the served pages. Offline migration uses macOS's `/usr/bin/sqlite3`. Details and
operator checks are in [installation and cutover](install.md).
