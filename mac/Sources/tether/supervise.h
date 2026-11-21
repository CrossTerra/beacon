#ifndef TETHER_SUPERVISE_H
#define TETHER_SUPERVISE_H

#include <stdbool.h>
#include <stddef.h>
#include <sys/types.h>

#define TETHER_GRACE_MS 15000
#define TETHER_MARKER "BEACON_SUPERVISOR="

struct tether_spec {
    char *const *argv; // A fixed command. argv[0] is the absolute image path.
    bool restart;      // Restart with backoff after a non-zero exit, while the lease is live.
    bool lifetime;     // Standard input is a pipe that reaches EOF if tether itself dies.
    const char *log;   // Standard output is appended here (created 0600), or discarded when NULL.
    unsigned backoff_ms; // The first restart delay, doubling up to 30 seconds. 0 means 1 second.
};

// Runs one supervised session under the lock in `state_dir`. It stops every process left
// by an earlier session that died, starts `specs`, sends `ready` on `lease`, and supervises
// until the lease reads EOF or an error, or until SIGTERM or SIGINT. Then it stops every
// process in each job's group and every tracked descendant, verifies that none remain,
// and sends TETHER_STOPPED. Each job's processes carry TETHER_MARKER "<session>:<job>".
// The first job's state changes are reported with TETHER_RUNNING, TETHER_WAITING and TETHER_EXITED.
// If processes remain TETHER_GIVE_UP_MS after the grace period, at startup or shutdown, it
// returns 4 without TETHER_STOPPED and keeps the session file, so the next session sweeps again.
int tether_run(int lease, const char *state_dir, const struct tether_spec *specs, size_t count,
               char *const *env, unsigned grace_ms, char ready);

// Signals go through this hook, so tests can model a process that does not stop.
extern int (*tether_kill)(pid_t, int);

#endif
