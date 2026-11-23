#ifndef TETHER_SUPPORT_H
#define TETHER_SUPPORT_H

#include <sys/types.h>

// Lease bytes from tether to the app. Half-closing the app's end asks for a stop, and the
// kernel closes it if the app dies. The app writes only runner commands, one per line:
//   "L <id>\n" prepares the run described in <state>/runs/<id>/, "A <id>\n" acknowledges that Beacon
//   recorded its process group, and "K <id>\n" stops it.
// tether answers each run with lines that start with TETHER_LINE:
//   "#prepared <id> <pid>" (suspended; the pid is also the group id), "#started <id> <pid>" (resumed
//   after acknowledgement), "#failed <id> <errno>" (never resumed; ESTALE for a changed checkout,
//   ETIMEDOUT for a missing acknowledgement),
//   "#exited <id> <status>" (the exit code, or 128 + the signal), and
//   "#gone <id>" once every process of the run is gone.
#define TETHER_ACTIVE 'A'  // services started
#define TETHER_LEGACY 'L'  // the retired collector LaunchAgent is installed; collector not started
#define TETHER_BUSY 'B'    // another session holds the lock; nothing started
#define TETHER_STOPPED 'S' // every supervised process is verified gone
// The collector's state, after TETHER_ACTIVE. Never sent for a run, nor at all after TETHER_LEGACY.
#define TETHER_RUNNING 'R'  // started
#define TETHER_WAITING 'W'  // exited with a failure; restarting after a backoff
#define TETHER_EXITED 'X'   // exited and will not be restarted this session
// Without TETHER_STOPPED, a closed lease means cleanup was not confirmed: tether gives up
// after the grace period plus TETHER_GIVE_UP_MS, so Quit never waits longer than that.
#define TETHER_GIVE_UP_MS 5000
#define TETHER_LINE '#'
// A runner id: lowercase letters, digits and '_', 1 to TETHER_ID_MAX characters.
#define TETHER_ID_MAX 40

// Starts the helper at `helper` in its own process group with the lease on its standard input.
// Returns the app's end of the lease, or -1.
int tether_open_session(const char *helper, pid_t *pid);

// Spawns `image` as the leader of a new process group. It inherits only standard input
// (`input`) and standard output (`output`); either is /dev/null when negative. Standard
// error is /dev/null.
pid_t tether_spawn(const char *image, char *const argv[], char *const env[], int input, int output);

// As tether_spawn, with standard error on `error` (/dev/null when negative) and, when `cwd` is
// not negative, started in the directory that descriptor names.
pid_t tether_spawn_at(const char *image, char *const argv[], char *const env[], int input, int output, int error,
                      int cwd);

// As tether_spawn_at, but the new group leader cannot run until tether_resume. No unsuspended fallback.
pid_t tether_spawn_suspended(const char *image, char *const argv[], char *const env[], int input, int output,
                             int error, int cwd);
// Resumes a process started suspended. 0 on success.
int tether_resume(pid_t pid);

// The launch file stores a directory's device and inode as unsigned decimals of their bits.
// Darwin's dev_t is signed; a negative device is that bit pattern, not a different volume.
// 1 when `device` and `inode` are exactly those bits.
int tether_same_identity(dev_t dev, ino_t ino, unsigned long long device, unsigned long long inode);

#endif
