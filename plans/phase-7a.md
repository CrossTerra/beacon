# Phase 7a — Per-user launchd agent

Status: complete. What was built is described in [`docs/`](../docs/); see [Outcome](#outcome). Phase 7 (signed releases, notarization, SBOM) stays pending.

## Goal

Beacon runs continuously as a per-user launchd agent on macOS. One person, one GUI session, the binary they installed, the keychain they already use. Not root. Not a daemon. No credential in the job definition.

## Scope

- `beacon service install|uninstall|status|logs`.
- A plist at `~/Library/LaunchAgents/dev.beacon.agent.plist` that runs the installed binary with `run` (the poll loop, not `--once`), `RunAtLoad`, and `KeepAlive` only when the process did not exit cleanly, with an explicit throttle.
- Standard output and standard error in files under the state directory, rotated by size.
- A minimal environment: the plist sets no variables, so it cannot carry a secret. Keys are read from the keychain at send time, as they are for a terminal `beacon run`.
- Refuse to install, and refuse to `run`, when the effective uid is 0. The plist is a LaunchAgent, never a LaunchDaemon, and it sets no `UserName`.
- Refuse to install a binary that is not code-signed with a stable identity. An ad-hoc signature (`go build`) is not stable: each rebuild is a new program to the keychain. `scripts/build-local.sh` is the local path to a stable identity.
- `status` reports whether the job is loaded and running, the last send pass's time and outcome from the state directory, and the lease holder as this instance knows it. It does not contact Signal.
- `uninstall` stops the job and removes the plist. It does not delete the state directory.
- `beacon run` in its poll loop exits 0 on SIGTERM after the in-flight send finishes, and does not start another send. A clean exit is what keeps launchd from restarting a stop.

## Out of scope

Signed releases, notarization, the SBOM, the menu bar, Linux service managers, installing or loading the agent on a developer's machine as part of this change.

## Design

### The job

Label `dev.beacon.agent`. Domain `gui/<uid>`, so the agent is in the graphical login session and can read the login keychain. `launchctl bootstrap` / `bootout` / `print`. Not `load` / `unload`.

`ProgramArguments` is the absolute, symlink-resolved binary, then `run`, `--config`, and the absolute config path. No `--once`. No `EnvironmentVariables`. No `UserName`. `Umask` is 077 (decimal 63), so files the process creates are not readable by other users. `ProcessType` is `Background`.

`KeepAlive` is `SuccessfulExit` false: launchd restarts a non-zero exit (a crash, a panic, a failed start) and does not restart exit 0. `ThrottleInterval` is 10 seconds, the launchd default made explicit, so a crash loop does not spin. `ExitTimeOut` is 180 seconds: long enough for one send, including one `Retry-After` of up to two minutes, and not infinity. A send still waiting after that can be killed; the outbox already recovers that case. `RunAtLoad` is true.

Standard output is `<state>/service.stdout.log`. Standard error is `<state>/service.stderr.log`. The same text a terminal run prints. The JSON log stays on standard error and still has no item content, no keys, no bodies, no tokens. The state directory is `0700` and the files `0600`.

launchd holds those file descriptors open and writes with append. Rotation is copy-and-truncate at one mebibyte, keeping one generation (`*.1`), done at the start of a pass and between passes, and only when standard output or standard error is that file. A rename would leave launchd's descriptor on the old inode. A file too large to copy is truncated without a copy rather than read into memory.

### Signing

`install` runs `codesign --verify --strict` and reads `codesign -dvv`. A stable identity has an `Authority` and is not ad-hoc (`Signature=adhoc` or the ad-hoc flag). Developer ID and the local "Beacon Local Signing" identity both qualify. Anything else is refused, with a fixed message that names `scripts/build-local.sh`, and nothing is written or bootstrapped.

The binary defaults to this executable, or `--binary PATH`. The path in the plist is absolute and symlink-resolved, and it is the path that was checked.

### Root

`beacon service` and `beacon run` refuse euid 0 before they read the keychain or touch launchd. A plist copied into `/Library/LaunchDaemons` therefore exits instead of running as root. Install writes only under the user's `~/Library/LaunchAgents`.

### Last pass

A send pass writes `pass.json` in the state directory (`0600`, atomic, synced): the time, an outcome (`ok`, `stopped`, or a fixed reason code), and the lease (`held`, `other`, `not_taken`). When this instance holds the lease, `holder` is the instance id from the `instance` file. The claim's `metadata.run` is that id plus `:lease`. Another holder is not named: Beacon does not read Signal's log, and a 409 does not carry the other instance's id. Status prints "another instance".

A dry run writes nothing, including this file. Status does not echo the file: an outcome, lease, or holder outside the fixed set is `unreadable` or `none`.

### SIGTERM

The poll loop treats SIGTERM as a stop, not as a cancelled send. The in-flight write keeps its context, including that write's retries, and is recorded. The loop does not start another event send, and it does not start another pass. It exits 0. SIGINT still cancels, as it does today. A lease renewal already inside `Ensure` finishes (at most the current period and the next); no further event send starts after it. Between passes, SIGTERM exits immediately.

### Commands

`install` refuses a second time without `--force`. `--force` bootouts, rewrites, and bootstraps. If bootstrap fails, the plist is removed so the next login does not start a half-installed agent. `uninstall` bootouts even when the plist is already gone, then removes the plist, and leaves state. `logs` prints the JSON log (standard error); `--stdout` prints the other file. Neither follows. `status` and `logs` need the config only to find the state directory.

On a system that is not macOS, install, uninstall, and status say the agent is a launchd job and do nothing else. Tests inject the launcher and the signature check, so they never call `launchctl` or `codesign`, and they never write `~/Library/LaunchAgents`.

## Tests

- Golden plist: the arguments, `RunAtLoad`, `KeepAlive`, the throttle, the log paths under the state directory, no `--once`, no `EnvironmentVariables`, no `UserName`.
- A path with `&` or `<` is escaped. A path with a control character is rejected.
- Install of an ad-hoc or unsigned binary does not write a plist and does not bootstrap. A stable identity does. A planted environment value is not in the plist. euid 0 does not install and does not call launchctl.
- `launchctl print` fixtures parse to loaded / running / not loaded.
- Status output from a fixture plus `pass.json`, including a holder that is not a fixed code and is not printed.
- Log rotation under an append-mode descriptor.
- SIGTERM during a send: that send is recorded, the next is not started, the process exits 0.

## Exit gate

`scripts/gates.sh` passes. The plist golden test matches. Status parsing matches the fixtures. A dry run still leaves the state directory unchanged. Nothing in this change is installed into the user's LaunchAgents.

## Outcome

Built as scoped. Differences from the text above:

- The poll loop runs in dry-run as well as send mode, unless `--once` or `--first`. A dry-run loop still writes
  nothing, including `pass.json`. The command documentation already said the loop was not limited to send mode; the
  code had been one pass. The agent would otherwise start and exit under the default `mode: dry-run`.
- `KeepAlive` is `SuccessfulExit` false, not the `Crashed` key. A Go panic exits non-zero rather than with a crash
  signal, and a failed start should come back. Exit 0, which is how SIGTERM leaves, is not restarted.
- `ExitTimeOut` is 180 seconds so launchd does not kill the process before the current send can finish. A send still
  waiting after that can be killed; the outbox recovers it.
- `Umask` is 077. The plist sets `ProcessType` to `Background`.
- Status does not name another instance's id. Beacon does not read Signal's log, and a 409 does not carry it.
- A persistent failure waits one poll interval, doubling up to 30 minutes, before the non-zero exit, and records the
  reason in `pass.json`. The service log does not include bodies unless `--bodies` is set. Install pins the
  certificate leaf hash; a mismatch is refused and shown by `status`. `--force` waits until the old job is unloaded.
