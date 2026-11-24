# Go service supervision

`beacon serve` starts a reaper from the same binary (`serve --reaper`, an internal mode with inherited
descriptors). This is the Go replacement for the app's [tether](tether.md), not an installer or a switch to
the Go service. The isolated review preview takes the lock and sweeps. It can launch separately approved
[agent runs](go-approvals.md#agent-runs-and-checkouts), but starts no live collector or watch and reads
no collector config or credentials. The signed app still uses C tether.

## Startup, stop and recovery

1. `serve` takes `instance.lock` in its private state directory and shares that locked file descriptor with
   the reaper. The reaper is the sole parent of supervised processes. It holds the lock through cleanup even
   if `serve` has been killed, so a replacement cannot race it.
2. Under the lock, the reaper sweeps a previous `supervisor.session` before the service replays its journal
   or binds a listener. The file is owner-only (`0600`) and contains a random session ID bound to the device
   and inode of that state directory and of `instance.lock`, not PIDs. A copy of the directory has a different
   lock, so it is not a session this process may sweep: startup stops rather than signaling the original jobs.
   An unreadable, malformed, unbound or unresolved session prevents startup. The sweep runs to its own TERM
   and KILL deadlines even if the recovering service dies. It replaces the session only after two fresh
   censuses find nothing, and keeps the marker if any run expires during the drain. The reaper checks that
   the service is still alive before publishing a new session. The new session ID is synced and published
   before any jobs start. Neither this sweep nor journal replay restarts a prior approval.
3. Each job is its own process group, with no inherited service environment. It receives the job's explicit
   environment plus `BEACON_SUPERVISOR=<session>:<job>`. A fixed job's standard input is a pipe whose only
   writer is the reaper, as C tether does for the collector. The collector command sets `BEACON_TETHERED=1`
   and exits on stdin EOF, the same guard as `beacon run`. That writer closes if the reaper dies, so a
   tethered collector exits on EOF instead of surviving the reaper or exiting at once on `/dev/null`.
   `Command.Stdin` is not used for a fixed job. A failed collector or watch restarts after its entire tree
   is gone, with 1, 2, 4, 8, 16, then at most 30 seconds between attempts. Clean exits and runs do not
   restart. A leader that exits stays unreaped until a later census has seen the tree gone, reserving the
   group ID. The census that first sees the leader gone does not release the group: a child forked as the
   leader exited may not be visible yet.
4. `SIGTERM` or Ctrl-C makes `serve` close the lifetime pipe's sole writer. If `serve` dies, the kernel
   closes it. EOF cancels pending starts and restarts. The reaper concurrently sends TERM to jobs' groups
   and tracked processes, sends KILL after 15 seconds, and gives up after five more seconds if it cannot
   confirm cleanup. A run that has already reached that give-up is reported immediately; its session marker
   stays. Other jobs are drained only until the deadlines they already have. Drain does not grant another
   full grace. Zombies are already dead, but the reaper still waits for and reaps its own leaders.
   `serve` waits at most 23 seconds for the reaper, and reports an unconfirmed stop rather than hanging.
5. Only confirmed cleanup removes the session file. If the reaper itself is killed, `serve` stops serving
   and exits with an error; it cannot claim the jobs are gone. The next start does the marker sweep before
   serving. A sweep uses process identities discovered in the current process table, never a saved numeric PID.

The reaper checks the same user's process table every 100 ms while jobs are active. Group members and
descendants of tracked processes are tracked by PID and start time, even after they detach. Cleanup also
checks for the session's exact marker, including processes that detached and became orphaned before a scan.
Linux reads `/proc/PID/stat` and `/proc/PID/environ`, and the process UID from `/proc/PID/status`, so a
non-dumpable process of this user is not dropped because `stat` is root-owned. A missing table entry is not
leader death: exit is confirmed with `waitid` `WNOWAIT`, which does not reap. An already-tracked process
that disappears from the table stays pending until death or PID reuse is confirmed; otherwise cleanup fails
at its deadline. `waitid` only sees the reaper's own children. Any other tracked process, such as a detached
descendant or an orphan found by the startup sweep, is reaped by its own parent or init: on Linux it is
confirmed gone when `kill(pid, 0)` reports `ESRCH`, or when the PID's `/proc/PID/stat` shows another start
time. A PID that still exists but cannot be read stays pending. The reaper does not reap a leader until a previous census confirmed the exit. macOS uses `kern.proc` and
`KERN_PROCARGS2`, without cgo. Only a matching marker is retained from those environment queries. There is
no new network egress.

The reaper has a separate process group so launchd's cleanup of the service group does not kill it first.
On Linux, the planned systemd user unit uses `KillMode=control-group` and `TimeoutStopSec=30`. The cgroup
also catches processes that escaped group and marker tracking. The reaper does not treat systemd's group-wide
TERM as a clean exit: it still waits for the lifetime pipe. Part 5 writes and installs both user service
definitions; this part changes neither the installer nor the currently installed app.

## Run handoff

Trusted service code uses `Supervisor.PrepareRun` with an approved run ID and explicit command. The reaper
installs the process group and its ownership before returning `Run.PGID()`. A trusted bootstrap waits for a
permit on a private pipe: it has not loaded the target executable or received its environment. The handler
durably records the registered group and claims the permit before release. The bootstrap's readiness,
permit write and exec-status read share the request's deadline; a late start does not grant a new timeout.
A failed exec writes an error on a close-on-exec status pipe. Silent EOF alone could be bootstrap death.
macOS requires a kernel exec notification; Linux corroborates EOF with a non-exiting child or a normal
exit status the bootstrap could not return. An ambiguous death or timeout after the permit is unconfirmed.
The reaper syncs a receipt before acknowledging an approved run's exec. Shutdown, `Run.Stop()` or five seconds
without release cancels a prepared launch without executing the target. The leader's exit is reported without
reaping it; `Run.Wait` confirms the whole tree is gone or returns a cleanup/launch error. There is no retry.

When supplied, the service passes the checkout descriptor from freeze to the reaper over a private Unix
socket pair. The bootstrap enters by descriptor and verifies the approved path still names it just before
exec. `PWD` is the approved path; `OLDPWD` is not inherited. The approved prompt is sent to the child
through an anonymous pipe; there is no prompt file to reopen. Run outputs are created exclusively; fixed
job logs append. The handler, not the reaper, owns approval and construction of the command and environment.
Do not pass Beacon credentials to a child.

The proposal socket resolves the kernel peer PID once, at connect time. Group membership or tracked ancestry
can identify a run; an environment marker alone cannot. An empty run ID means the peer was read and is not
in a supervised group. An unreadable, vanished or zombie peer is rejected, not treated as that empty result. If the
reaper is unavailable, the socket rejects the connection rather than letting the peer fall back to the
unattributed identity.

## Limits and tests

Supervision is not isolation. Without systemd's cgroup, a descendant can escape if it leaves its group and
ancestry tracking misses it, and it removes the marker or the kernel hides its environment (as macOS does
for platform binaries). A PID can still be reused between the final identity check and a signal. If the
reaper is killed, its in-memory ancestry is lost; recovery needs the marker, a still-marked ancestor or a
still-marked group leader. The Linux cgroup also ends descendants when the unit is stopped.

`go test -race ./...` in the macOS and Linux CI matrix includes the supervision fixtures: a summary-only
collector, a restarting watch, and runner trees with a TERM-ignoring child and a detached grandchild holding
a loopback listener. They cover service stop, SIGKILL of the service or the reaper, the next start's sweep,
pending restarts, the run handshake, socket attribution and bounded failure. They install nothing, run no
real job, and use no Keychain.
