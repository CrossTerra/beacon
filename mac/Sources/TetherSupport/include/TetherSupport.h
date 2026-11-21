#ifndef TETHER_SUPPORT_H
#define TETHER_SUPPORT_H

#include <sys/types.h>

// Lease bytes from tether to the app. The app never writes: half-closing its end asks
// for a stop, and the kernel closes it if the app dies.
#define TETHER_ACTIVE 'A'  // services started
#define TETHER_LEGACY 'L'  // the retired collector LaunchAgent is installed; collector not started
#define TETHER_BUSY 'B'    // another session holds the lock; nothing started
#define TETHER_STOPPED 'S' // every supervised process is verified gone

// Starts the helper at `helper` in its own process group with the lease on its standard input.
// Returns the app's end of the lease, or -1.
int tether_open_session(const char *helper, pid_t *pid);

// Spawns `image` as the leader of a new process group. Only standard input (`input`, or
// /dev/null when negative) and /dev/null for output are inherited.
pid_t tether_spawn(const char *image, char *const argv[], char *const env[], int input);

#endif
