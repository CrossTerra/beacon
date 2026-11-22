// Rootless harness: runs the real tether session with fixture jobs in place of the collector and watches.
#include "TetherSupport.h"
#include "supervise.h"

#include <arpa/inet.h>
#include <fcntl.h>
#include <limits.h>
#include <netinet/in.h>
#include <signal.h>
#include <stdbool.h>
#include <stdio.h>
#include <stdlib.h>
#include <string.h>
#include <sys/socket.h>
#include <sys/wait.h>
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

static int ignore_signal(pid_t pid, int sig) {
    (void)pid;
    (void)sig;
    return 0; // A process that does not stop, as far as tether can tell.
}

static void forever(void) {
    for (;;) pause();
}

// A process in a new session whose parent has already exited. It ignores TERM, so cleanup
// must find it and escalate.
static void leaked_grandchild(void) {
    if (setsid() < 0) _exit(1);
    pid_t pid = fork();
    if (pid != 0) _exit(pid < 0);
    signal(SIGTERM, SIG_IGN);
    record();
    char name[64];
    snprintf(name, sizeof(name), "leaked-%d", getpid());
    touch(name);
    forever();
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
        // Optional: "slow" makes the watch's first restart wait 30 seconds; "stubborn" drops every signal.
        // "leaky" makes the watch leave a detached grandchild and exit with a failure, once.
        bool slow = argc > 3 && strcmp(argv[3], "slow") == 0;
        bool leaky = argc > 3 && strcmp(argv[3], "leaky") == 0;
        if (argc > 3 && strcmp(argv[3], "stubborn") == 0) tether_kill = ignore_signal;
        char *idle[] = {argv[0], "idle", argv[2], NULL};
        char *flaky[] = {argv[0], leaky ? "leaky" : "flaky", argv[2], NULL};
        char *detach[] = {argv[0], "detach", argv[2], NULL};
        char log[PATH_MAX];
        snprintf(log, sizeof(log), "%s/collector.log", dir);
        const struct tether_spec specs[] = {
            {.argv = idle, .restart = true, .lifetime = true, .log = log}, // the collector's role
            {.argv = flaky, .restart = true, .backoff_ms = slow || leaky ? 30000 : 0}, // a watch
            {.argv = detach},                                  // a runner tree: never restarted
        };
        char *env[] = {"PATH=/usr/bin:/bin", NULL};
        return tether_run(STDIN_FILENO, dir, specs, 3, env, 1000, TETHER_ACTIVE);
    }
    record();
    if (strcmp(argv[1], "idle") == 0) {
        if (printf("Items: 0 send\n") < 0 || fflush(stdout) != 0) return 1;
        char byte;
        return read(STDIN_FILENO, &byte, 1) == 0 ? 0 : 1; // EOF: tether died.
    }
    if (strcmp(argv[1], "leaky") == 0) {
        char path[PATH_MAX];
        snprintf(path, sizeof(path), "%s/watch-ran", dir);
        if (access(path, F_OK) == 0) forever();
        char name[64];
        snprintf(name, sizeof(name), "watch-leader-%d", getpid());
        touch(name);
        pid_t child = fork();
        if (child == 0) leaked_grandchild();
        if (child < 0 || waitpid(child, NULL, 0) != child) return 1;
        touch("watch-ran"); // Written after the middle process exited: the grandchild is detached.
        return 1;
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
