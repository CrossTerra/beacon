#include "TetherSupport.h"
#include "supervise.h"

#include <errno.h>
#include <libgen.h>
#include <limits.h>
#include <mach-o/dyld.h>
#include <pwd.h>
#include <stdint.h>
#include <stdio.h>
#include <stdlib.h>
#include <string.h>
#include <sys/resource.h>
#include <sys/stat.h>
#include <unistd.h>

// The collector is the bundled binary beside this helper, with its default config. Signal
// watches join this list when they exist (plan part 4); runners arrive with agent runs.
static int session(void) {
    uid_t uid = getuid();
    struct passwd *pw = getpwuid(uid);
    if (uid == 0 || geteuid() != uid || !pw || !pw->pw_dir || pw->pw_dir[0] != '/') return 1;

    char self[PATH_MAX], resolved[PATH_MAX], collector[PATH_MAX], home[PATH_MAX + 8];
    char support[PATH_MAX], state[PATH_MAX], legacy[PATH_MAX];
    uint32_t size = sizeof(self);
    if (_NSGetExecutablePath(self, &size) != 0 || !realpath(self, resolved) ||
        snprintf(collector, sizeof(collector), "%s/beacon", dirname(resolved)) >= (int)sizeof(collector) ||
        snprintf(home, sizeof(home), "HOME=%s", pw->pw_dir) >= (int)sizeof(home) ||
        snprintf(support, sizeof(support), "%s/Library/Application Support", pw->pw_dir) >= (int)sizeof(support) ||
        snprintf(state, sizeof(state), "%s/beacon", support) >= (int)sizeof(state) ||
        snprintf(legacy, sizeof(legacy), "%s/Library/LaunchAgents/dev.beacon.agent.plist", pw->pw_dir) >=
            (int)sizeof(legacy)) return 1;
    if ((mkdir(support, 0700) != 0 && errno != EEXIST) || (mkdir(state, 0700) != 0 && errno != EEXIST) ||
        chdir(pw->pw_dir) != 0) return 1;

    // Two collectors would contend for one outbox. Leave the retired job to be removed first.
    struct stat st;
    bool legacy_installed = lstat(legacy, &st) == 0 || errno != ENOENT;
    char *collector_argv[] = {collector, "run", "--summary", NULL};
    const struct tether_spec specs[] = {{.argv = collector_argv, .restart = true, .lifetime = true}};
    char *env[] = {home, "PATH=/usr/bin:/bin", "LANG=C", "BEACON_TETHERED=1", NULL};
    return tether_run(STDIN_FILENO, state, specs, legacy_installed ? 0 : 1, env, TETHER_GRACE_MS,
                      legacy_installed ? TETHER_LEGACY : TETHER_ACTIVE);
}

int main(int argc, char **argv) {
    umask(077);
    const struct rlimit no_core = {0, 0};
    if (setrlimit(RLIMIT_CORE, &no_core) != 0) return 1;
    if (argc == 2 && strcmp(argv[1], "session") == 0) return session();
    return 2;
}
