#ifndef TETHER_SUPPORT_H
#define TETHER_SUPPORT_H

#include <sys/types.h>

// Lease bytes from tether to the app. The app never writes: half-closing its end asks
// for a stop, and the kernel closes it if the app dies.
#define TETHER_ACTIVE 'A'  // services started
#define TETHER_LEGACY 'L'  // the retired collector LaunchAgent is installed; collector not started
#define TETHER_BUSY 'B'    // another session holds the lock; nothing started
#define TETHER_STOPPED 'S' // every supervised process is verified gone
// The first service's state (the collector), after TETHER_ACTIVE.
#define TETHER_RUNNING 'R'  // started
#define TETHER_WAITING 'W'  // exited with a failure; restarting after a backoff
#define TETHER_EXITED 'X'   // exited and will not be restarted this session
// Without TETHER_STOPPED, a closed lease means cleanup was not confirmed: tether gives up
// after the grace period plus TETHER_GIVE_UP_MS, so Quit never waits longer than that.
#define TETHER_GIVE_UP_MS 5000

// Starts the helper at `helper` in its own process group with the lease on its standard input.
// Returns the app's end of the lease, or -1.
int tether_open_session(const char *helper, pid_t *pid);

// Spawns `image` as the leader of a new process group. It inherits only standard input
// (`input`) and standard output (`output`); either is /dev/null when negative. Standard
// error is /dev/null.
pid_t tether_spawn(const char *image, char *const argv[], char *const env[], int input, int output);

#endif
