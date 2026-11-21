#include "TetherSupport.h"

#include <fcntl.h>
#include <signal.h>
#include <spawn.h>
#include <sys/socket.h>
#include <unistd.h>

pid_t tether_spawn(const char *image, char *const argv[], char *const env[], int input) {
    posix_spawn_file_actions_t files;
    posix_spawnattr_t attrs;
    if (posix_spawn_file_actions_init(&files) != 0) return -1;
    pid_t pid = -1;
    if (posix_spawnattr_init(&attrs) == 0) {
        sigset_t clear, defaults;
        sigemptyset(&clear);
        sigfillset(&defaults);
        short flags = POSIX_SPAWN_CLOEXEC_DEFAULT | POSIX_SPAWN_SETPGROUP |
                      POSIX_SPAWN_SETSIGDEF | POSIX_SPAWN_SETSIGMASK;
        int err = input < 0
            ? posix_spawn_file_actions_addopen(&files, STDIN_FILENO, "/dev/null", O_RDONLY, 0)
            : posix_spawn_file_actions_adddup2(&files, input, STDIN_FILENO);
        if (err == 0 &&
            posix_spawn_file_actions_addopen(&files, STDOUT_FILENO, "/dev/null", O_WRONLY, 0) == 0 &&
            posix_spawn_file_actions_addopen(&files, STDERR_FILENO, "/dev/null", O_WRONLY, 0) == 0 &&
            posix_spawnattr_setsigmask(&attrs, &clear) == 0 &&
            posix_spawnattr_setsigdefault(&attrs, &defaults) == 0 &&
            posix_spawnattr_setpgroup(&attrs, 0) == 0 &&
            posix_spawnattr_setflags(&attrs, flags) == 0 &&
            posix_spawn(&pid, image, &files, &attrs, argv, env) != 0) {
            pid = -1;
        }
        posix_spawnattr_destroy(&attrs);
    }
    posix_spawn_file_actions_destroy(&files);
    return pid;
}

int tether_open_session(const char *helper, pid_t *pid) {
    if (getuid() == 0 || getuid() != geteuid()) return -1;
    int pair[2];
    if (socketpair(AF_UNIX, SOCK_STREAM, 0, pair) != 0) return -1;
    *pid = -1;
    if (fcntl(pair[0], F_SETFD, FD_CLOEXEC) == 0 && fcntl(pair[1], F_SETFD, FD_CLOEXEC) == 0) {
        char *argv[] = {(char *)helper, "session", NULL};
        char *env[] = {"PATH=/usr/bin:/bin", "LANG=C", NULL};
        *pid = tether_spawn(helper, argv, env, pair[1]);
    }
    close(pair[1]);
    if (*pid > 0) return pair[0];
    close(pair[0]);
    return -1;
}
