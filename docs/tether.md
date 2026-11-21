# Tether: services that end with the app

`Beacon.app` starts its services through `tether`, a small helper in `Beacon.app/Contents/Helpers`. Nothing is
installed separately. There is no LaunchDaemon, no administrator step and no other account. Everything runs as
the person in their login session.

## What it runs

| Service | Command | Restart |
| --- | --- | --- |
| Collector | `Contents/Helpers/beacon run --summary`, with the default config (`~/Library/Application Support/beacon/config.yaml`) | After a non-zero exit, with backoff |
| Signal watches | None yet. They join the same fixed list in plan part 4 | After a non-zero exit, with backoff |
| Runners | None yet. They arrive with agent runs | Never |

The commands are compiled into `tether`. The app passes no command, path, credential or setting to it. Each
service gets a cleared environment: `HOME`, a system `PATH`, the C locale, `BEACON_TETHERED=1` and
`BEACON_SUPERVISOR`, a random marker for this session and service. Standard output and standard error go to
`/dev/null`. The collector still records `pass.json` and `coverage.json`. `beacon status --coverage` shows them.

If `~/Library/LaunchAgents/dev.beacon.agent.plist` exists, `tether` does not start the collector, because two
collectors would contend for one outbox. The menu says so. Run `beacon service uninstall` to remove the retired
job, then relaunch Beacon.

## How it stops

The app holds one end of a socket pair. `tether` holds the other on its standard input. The app never writes to
it. **Quit Beacon** closes the app's write half and waits for `tether` to confirm cleanup. If the app crashes or
is force-quit, the kernel closes its end. Either way, `tether` stops:

1. It starts nothing more. A pending restart is cancelled.
2. It sends SIGTERM to every service's process group and every process it tracks for that service.
3. After 15 seconds, it sends SIGKILL to whatever remains.
4. It confirms with the kernel's process table that no tracked process is left, reaps each service and sends a
   confirmation to the app.

Each service starts as the leader of a new process group. `tether` observes a leader's exit without reaping it.
The zombie keeps the group ID reserved, so the group cannot be confused with an unrelated one. It is reaped only
once its whole tree is gone. A restart of the collector or a watch waits for that cleanup too.

A descendant can leave its process group with `setsid`. `tether` still tracks it in three ways:

- every 100 ms, it adds each new process whose parent it already tracks;
- while stopping, it adds any process with the service's `BEACON_SUPERVISOR` marker;
- every process in the service's process group is a member.

If `tether` itself dies, the collector's standard input pipe reaches EOF and the collector exits. Other services
can be left running. The next session finds their marker from the session file
`~/Library/Application Support/beacon/tether.session` and stops them and their tracked descendants before it starts
anything. A lock (`tether.lock` in the same directory) allows one session at a time. A second app gets
**Another Beacon session is running** and starts nothing.

Quit also expires every approval that was not durably started. Recovery applies the same rule to approvals
already recorded. The journal connects that rule to stored proposals in a later pull request.

## Limits

- A descendant can still escape. It must leave the process group, its parent must exit within 100 ms, and it must
  either clear `BEACON_SUPERVISOR` or run an Apple platform binary. macOS hides the environment of platform
  binaries from other processes. `tether` does not run untrusted code yet. Runners need this reviewed before
  they are added.
- Signals go to process IDs from the process table. A process that exits between the read and the signal could,
  in principle, have its ID reused by another of the person's processes.
- Every service runs as the person, with the person's file access and Keychain prompts. `tether` supervises
  processes; it does not isolate them.
- Logout and shutdown end the session's processes through macOS, whether or not `tether` confirms cleanup.

## Tests

`scripts/test-tether.sh` builds the session code with fixture services and runs it as the current user. A
collector stand-in, a watch that fails once and restarts after a one-second backoff, and a runner tree run under
the session. The runner tree has a child in its group that ignores SIGTERM, and a detached grandchild in a new
session that ignores SIGTERM and holds a loopback listener. The tests check that:

- Quit after the restart, Quit with the restart pending, and a killed app stop every process and close the
  listener, and nothing restarts during shutdown;
- a killed `tether` leaves processes that the next session stops before it reports ready;
- a second session is refused while the first runs;
- the real Go collector exits 0 on SIGTERM, and exits when its lifetime pipe closes.

CI runs these on macOS. They install nothing, use no Keychain and contact nothing outside loopback.
