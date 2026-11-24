# Development and gates

Use Go **1.24.3** from [go.dev/dl](https://go.dev/dl/). There is no Xcode project, signing identity or cgo
requirement. The gates in `scripts/gates.sh` run formatting, vet, staticcheck, race tests, the installer fixtures,
ShellCheck when available and a reproducible build. CI runs the gates on macOS and Linux and builds the actual
install artifact in a temporary prefix without starting a user service. Linux CI exercises the review pages with
headless Chrome and a virtual authenticator. `scripts/repro.sh REF` builds the given Git commit twice from archives.

Use only temporary fixture environments:

- `internal/approval`, `internal/approvalserve`, `internal/review` and `internal/actsocket` cover Signal's
  committed hash/record fixtures, recovery without replay, same-user admission, browser sessions and assertions.
- `internal/actionexec`, `internal/passkey`, `internal/webauthn` and `internal/vault` cover fake providers,
  exact-write and read-back checks, key enrollment, scope segregation and the KeePassXC format.
- `internal/supervisor` and the subprocess tests in `cmd/beacon` cover sweep, process groups, cleanup and lifetime
  pipe failure. `scripts/test-install.sh` uses fake user managers and temporary home directories.
- `internal/cutover` uses fixture SQLite data, including committed WAL, and a private socket. The `vault` CLI
  tests inject the old credential reader. Tests do not open the login Keychain or migrate a live app.

Do not run a real install, `vault import-keychain`, `migrate-swift` or any command against a person's live state.
For the operator's cutover and smoke tests see [installing Beacon](install.md).
