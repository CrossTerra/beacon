// Rootless harness: runs the real tether session with fixture jobs in place of the collector and watches.
#include "TetherSupport.h"
#include "supervise.h"

#include <arpa/inet.h>
#include <fcntl.h>
#include <limits.h>
#include <netinet/in.h>
#include <signal.h>
#include <stdio.h>
#include <stdlib.h>
#include <string.h>
#include <sys/socket.h>
#include <unistd.h>

static const char *dir;

static void touch(const char *name) {
    char path[PATH_MAX];
    snprintf(path, sizeof(path), "%s/%s", dir, name);
    int fd = open(path, O_WRONLY | O_CREAT, 0600);
    if (fd >= 0) close(fd);
}

static void record(void) {
    char name[64];
    snprintf(name, sizeof(name), "pids/%d", getpid());
    touch(name);
}

static void forever(void) {
    for (;;) pause();
}

// A loopback listener in a new session, whose parent has already exited. It ignores TERM,
// so cleanup must find it and escalate.
static void detached_grandchild(void) {
    if (setsid() < 0) _exit(1);
    pid_t pid = fork();
    if (pid != 0) _exit(pid < 0);
    signal(SIGTERM, SIG_IGN);
    record();
    struct sockaddr_in addr = {.sin_family = AF_INET, .sin_addr.s_addr = htonl(INADDR_LOOPBACK)};
    socklen_t size = sizeof(addr);
    int fd = socket(AF_INET, SOCK_STREAM, 0);
    if (fd < 0 || bind(fd, (struct sockaddr *)&addr, size) != 0 || listen(fd, 4) != 0 ||
        getsockname(fd, (struct sockaddr *)&addr, &size) != 0) _exit(1);
    char tmp[PATH_MAX], path[PATH_MAX];
    snprintf(tmp, sizeof(tmp), "%s/port.tmp", dir);
    snprintf(path, sizeof(path), "%s/port", dir);
    FILE *f = fopen(tmp, "w");
    if (!f || fprintf(f, "%u\n", ntohs(addr.sin_port)) < 0 || fclose(f) != 0 || rename(tmp, path) != 0) _exit(1);
    forever();
}

int main(int argc, char **argv) {
    if (argc < 3) return 2;
    dir = argv[2];
    if (strcmp(argv[1], "session") == 0) {
        char *idle[] = {argv[0], "idle", argv[2], NULL};
        char *flaky[] = {argv[0], "flaky", argv[2], NULL};
        char *detach[] = {argv[0], "detach", argv[2], NULL};
        const struct tether_spec specs[] = {
            {.argv = idle, .restart = true, .lifetime = true}, // the collector's role
            {.argv = flaky, .restart = true},                  // a watch
            {.argv = detach},                                  // a runner tree: never restarted
        };
        char *env[] = {"PATH=/usr/bin:/bin", NULL};
        return tether_run(STDIN_FILENO, dir, specs, 3, env, 1000, TETHER_ACTIVE);
    }
    record();
    if (strcmp(argv[1], "idle") == 0) {
        char byte;
        return read(STDIN_FILENO, &byte, 1) == 0 ? 0 : 1; // EOF: tether died.
    }
    if (strcmp(argv[1], "flaky") == 0) {
        char path[PATH_MAX];
        snprintf(path, sizeof(path), "%s/watch-ran", dir);
        if (access(path, F_OK) != 0) {
            touch("watch-ran");
            return 1; // The first run fails, so a restart is scheduled after a 1 second backoff.
        }
        touch("watch-restarted");
        forever();
    }
    if (strcmp(argv[1], "detach") == 0) {
        pid_t in_group = fork();
        if (in_group == 0) {
            signal(SIGTERM, SIG_IGN);
            record();
            forever();
        }
        if (fork() == 0) detached_grandchild();
        forever();
    }
    return 2;
}
