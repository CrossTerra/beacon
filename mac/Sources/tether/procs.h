#ifndef TETHER_PROCS_H
#define TETHER_PROCS_H

#include <stdbool.h>
#include <stdint.h>
#include <sys/types.h>

struct tether_proc {
    pid_t pid, ppid, pgid;
    uint64_t start_us;
};

// Lists this user's live processes, excluding zombies and the caller. Returns the count, or -1.
// The caller frees *out.
int tether_list(struct tether_proc **out);

// Whether `pid` has the environment entry `entry`, or one that starts with it when `prefix`.
// Darwin hides the environment of Apple platform binaries, so false is not proof.
bool tether_env_has(pid_t pid, const char *entry, bool prefix);

#endif
