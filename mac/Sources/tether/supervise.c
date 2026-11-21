#include "supervise.h"

#include "TetherSupport.h"
#include "procs.h"

#include <errno.h>
#include <fcntl.h>
#include <poll.h>
#include <signal.h>
#include <stdint.h>
#include <stdio.h>
#include <stdlib.h>
#include <string.h>
#include <sys/file.h>
#include <sys/socket.h>
#include <sys/stat.h>
#include <sys/wait.h>
#include <time.h>
#include <unistd.h>

#define TOKEN_SIZE 33
#define ENTRY_SIZE (sizeof(TETHER_MARKER) + TOKEN_SIZE + 24)

struct tracked {
    pid_t pid;
    uint64_t start_us;
    size_t job;
};

struct job {
    const struct tether_spec *spec;
    char entry[ENTRY_SIZE]; // TETHER_MARKER "<session>:<job>"
    // The group leader. It is reaped only after its whole tree is gone, so while pid > 0
    // the kernel cannot reuse pid as another group's id.
    pid_t pid;
    bool cleaning, term_sent, retired, clean_exit;
    int lifetime;
    unsigned failures;
    uint64_t started_at, retry_at, kill_at;
};

struct session {
    struct job *jobs;
    size_t count;
    char *const *env;
    unsigned grace_ms;
    struct tracked *tracked;
    size_t ntracked, cap;
};

static volatile sig_atomic_t stopping;

static void stop(int sig) {
    (void)sig;
    stopping = 1;
}

static uint64_t now_ms(void) {
    return clock_gettime_nsec_np(CLOCK_MONOTONIC_RAW) / 1000000;
}

// The app never writes. Any readable byte, EOF or error ends the session for good.
static bool live(int lease) {
    struct pollfd fd = {.fd = lease, .events = POLLIN};
    return !stopping && poll(&fd, 1, 0) == 0;
}

static void send_byte(int lease, char byte) {
    (void)send(lease, &byte, 1, MSG_DONTWAIT);
}

static struct tracked *find(struct session *s, pid_t pid) {
    for (size_t i = 0; i < s->ntracked; i++)
        if (s->tracked[i].pid == pid) return &s->tracked[i];
    return NULL;
}

static void add(struct session *s, const struct tether_proc *p, size_t job) {
    if (s->ntracked == s->cap) {
        size_t cap = s->cap ? 2 * s->cap : 64;
        struct tracked *grown = realloc(s->tracked, cap * sizeof(*grown));
        if (!grown) return; // The environment marker and the group still find it.
        s->tracked = grown;
        s->cap = cap;
    }
    s->tracked[s->ntracked++] = (struct tracked){.pid = p->pid, .start_us = p->start_us, .job = job};
}

// Keeps the tracked processes that still exist and adds new members of a job: its leader,
// anything in its process group and, transitively, any child of a tracked process. A child
// that calls setsid stays tracked through its parent if seen before that parent exits.
static void track(struct session *s, const struct tether_proc *procs, int n) {
    size_t kept = 0;
    for (size_t i = 0; i < s->ntracked; i++) {
        for (int k = 0; k < n; k++) {
            if (procs[k].pid == s->tracked[i].pid && procs[k].start_us == s->tracked[i].start_us) {
                s->tracked[kept++] = s->tracked[i];
                break;
            }
        }
    }
    s->ntracked = kept;
    bool added;
    do {
        added = false;
        for (int k = 0; k < n; k++) {
            const struct tether_proc *p = &procs[k];
            if (find(s, p->pid)) continue;
            size_t job = s->count;
            for (size_t j = 0; j < s->count && job == s->count; j++)
                if (s->jobs[j].pid > 0 && (p->pid == s->jobs[j].pid || p->pgid == s->jobs[j].pid)) job = j;
            const struct tracked *parent = job == s->count ? find(s, p->ppid) : NULL;
            if (parent && p->start_us >= parent->start_us) job = parent->job;
            if (job < s->count) {
                add(s, p, job);
                added = true;
            }
        }
    } while (added);
}

// While a job is being cleaned, also adopt any process that carries its marker: a
// detached descendant whose parent exited before it was seen.
static void adopt_marked(struct session *s, const struct tether_proc *procs, int n) {
    for (int k = 0; k < n; k++) {
        if (find(s, procs[k].pid)) continue;
        for (size_t j = 0; j < s->count; j++) {
            if (s->jobs[j].cleaning && tether_env_has(procs[k].pid, s->jobs[j].entry, false)) {
                add(s, &procs[k], j);
                break;
            }
        }
    }
}

// Counts the job's live processes and, with a signal, sends it to each and to the group.
static size_t members(struct session *s, size_t j, int sig) {
    size_t count = 0;
    for (size_t i = 0; i < s->ntracked; i++) {
        if (s->tracked[i].job != j) continue;
        count++;
        if (sig) kill(s->tracked[i].pid, sig);
    }
    if (sig && s->jobs[j].pid > 0) killpg(s->jobs[j].pid, sig);
    return count;
}

static void retry_later(struct job *job, uint64_t now) {
    if (now - job->started_at > 60000) job->failures = 0;
    // 1, 2, 4, 8, 16, then 30 seconds. The collector also has its own pass backoff.
    job->retry_at = now + (job->failures < 5 ? (1000u << job->failures++) : 30000u);
}

static void start(struct session *s, size_t j, uint64_t now) {
    struct job *job = &s->jobs[j];
    size_t envc = 0;
    while (s->env[envc]) envc++;
    char **env = calloc(envc + 2, sizeof(*env));
    int pipefd[2] = {-1, -1};
    if (env && (!job->spec->lifetime ||
                (pipe(pipefd) == 0 && fcntl(pipefd[0], F_SETFD, FD_CLOEXEC) == 0 &&
                 fcntl(pipefd[1], F_SETFD, FD_CLOEXEC) == 0))) {
        memcpy(env, s->env, envc * sizeof(*env));
        env[envc] = job->entry;
        job->pid = tether_spawn(job->spec->argv[0], job->spec->argv, env, pipefd[0]);
    } else {
        job->pid = -1;
    }
    free(env);
    if (pipefd[0] >= 0) close(pipefd[0]);
    job->started_at = now;
    if (job->pid > 0) {
        job->lifetime = pipefd[1];
        return;
    }
    job->pid = 0;
    if (pipefd[1] >= 0) close(pipefd[1]);
    if (job->spec->restart) retry_later(job, now);
    else job->retired = true;
}

// One step of a job. Restarts happen only while `running`, so shutdown starts nothing.
static void step(struct session *s, size_t j, bool running, uint64_t now) {
    struct job *job = &s->jobs[j];
    if (job->pid > 0 && !job->cleaning) {
        siginfo_t info = {0};
        // Observe the exit without reaping: the zombie keeps the group id reserved.
        if (waitid(P_PID, job->pid, &info, WEXITED | WNOHANG | WNOWAIT) == 0 && info.si_pid == job->pid) {
            job->cleaning = true;
            job->term_sent = false;
            job->clean_exit = info.si_code == CLD_EXITED && info.si_status == 0;
        }
    }
    if (job->cleaning) {
        if (members(s, j, 0) == 0) {
            if (waitpid(job->pid, NULL, WNOHANG) != job->pid) return;
            job->pid = 0;
            job->cleaning = false;
            if (job->lifetime >= 0) close(job->lifetime);
            job->lifetime = -1;
            if (!job->spec->restart || job->clean_exit) job->retired = true;
            else retry_later(job, now);
        } else if (!job->term_sent) {
            members(s, j, SIGTERM);
            job->term_sent = true;
            job->kill_at = now + s->grace_ms;
        } else if (now >= job->kill_at) {
            members(s, j, SIGKILL);
        }
    } else if (running && job->pid == 0 && !job->retired && now >= job->retry_at) {
        start(s, j, now);
    }
}

static bool refresh(struct session *s) {
    struct tether_proc *procs;
    int n = tether_list(&procs);
    if (n < 0) return false;
    track(s, procs, n);
    for (size_t j = 0; j < s->count; j++) {
        if (s->jobs[j].cleaning) {
            adopt_marked(s, procs, n);
            break;
        }
    }
    free(procs);
    return true;
}

// Stops what a session that died left behind: every process with its marker, and their descendants.
static void sweep(const char *prefix, unsigned grace_ms) {
    bool term_sent = false;
    uint64_t kill_at = 0;
    for (;;) {
        struct tether_proc *procs;
        int n = tether_list(&procs);
        if (n < 0) {
            poll(NULL, 0, 100);
            continue;
        }
        bool *match = calloc((size_t)n + 1, sizeof(*match));
        if (!match) {
            free(procs);
            poll(NULL, 0, 100);
            continue;
        }
        size_t count = 0;
        for (int k = 0; k < n; k++)
            if ((match[k] = tether_env_has(procs[k].pid, prefix, true))) count++;
        bool added;
        do {
            added = false;
            for (int k = 0; k < n; k++) {
                for (int p = 0; !match[k] && p < n; p++) {
                    if (match[p] && procs[k].ppid == procs[p].pid && procs[k].start_us >= procs[p].start_us) {
                        match[k] = added = true;
                        count++;
                    }
                }
            }
        } while (added);
        int sig = 0;
        if (count && !term_sent) {
            sig = SIGTERM;
            term_sent = true;
            kill_at = now_ms() + grace_ms;
        } else if (count && now_ms() >= kill_at) {
            sig = SIGKILL;
        }
        for (int k = 0; sig && k < n; k++)
            if (match[k]) kill(procs[k].pid, sig);
        free(match);
        free(procs);
        if (!count) return;
        poll(NULL, 0, 100);
    }
}

static bool read_token(int dir, char token[TOKEN_SIZE]) {
    int fd = openat(dir, "tether.session", O_RDONLY | O_NOFOLLOW | O_CLOEXEC);
    if (fd < 0) return false;
    ssize_t n = read(fd, token, TOKEN_SIZE - 1);
    close(fd);
    if (n != TOKEN_SIZE - 1) return false;
    token[TOKEN_SIZE - 1] = '\0';
    return strspn(token, "0123456789abcdef") == TOKEN_SIZE - 1;
}

static bool write_token(int dir, const char *token) {
    int fd = openat(dir, "tether.session.tmp", O_WRONLY | O_CREAT | O_TRUNC | O_NOFOLLOW | O_CLOEXEC, 0600);
    if (fd < 0) return false;
    bool ok = write(fd, token, TOKEN_SIZE - 1) == TOKEN_SIZE - 1 && fsync(fd) == 0;
    close(fd);
    return ok && renameat(dir, "tether.session.tmp", dir, "tether.session") == 0 && fsync(dir) == 0;
}

int tether_run(int lease, const char *state_dir, const struct tether_spec *specs, size_t count,
               char *const *env, unsigned grace_ms, char ready) {
    struct sigaction action = {.sa_handler = stop};
    sigemptyset(&action.sa_mask);
    if (signal(SIGPIPE, SIG_IGN) == SIG_ERR || sigaction(SIGTERM, &action, NULL) != 0 ||
        sigaction(SIGINT, &action, NULL) != 0) return 1;
    int dir = open(state_dir, O_RDONLY | O_DIRECTORY | O_NOFOLLOW | O_CLOEXEC);
    if (dir < 0) return 1;
    int lock = openat(dir, "tether.lock", O_RDWR | O_CREAT | O_NOFOLLOW | O_CLOEXEC, 0600);
    if (lock < 0) return 1;
    if (flock(lock, LOCK_EX | LOCK_NB) != 0) {
        send_byte(lease, TETHER_BUSY);
        return 3;
    }

    char token[TOKEN_SIZE], prefix[ENTRY_SIZE];
    if (read_token(dir, token)) {
        snprintf(prefix, sizeof(prefix), TETHER_MARKER "%s:", token);
        sweep(prefix, grace_ms);
    }
    uint8_t random[(TOKEN_SIZE - 1) / 2];
    arc4random_buf(random, sizeof(random));
    for (size_t i = 0; i < sizeof(random); i++) snprintf(token + 2 * i, 3, "%02x", random[i]);
    if (!write_token(dir, token)) return 1;

    struct job *jobs = calloc(count ? count : 1, sizeof(*jobs));
    if (!jobs) return 1;
    for (size_t j = 0; j < count; j++) {
        jobs[j] = (struct job){.spec = &specs[j], .lifetime = -1};
        snprintf(jobs[j].entry, sizeof(jobs[j].entry), TETHER_MARKER "%s:%zu", token, j);
    }
    struct session s = {.jobs = jobs, .count = count, .env = env, .grace_ms = grace_ms};
    if (live(lease)) send_byte(lease, ready);

    struct pollfd fd = {.fd = lease, .events = POLLIN};
    while (live(lease)) {
        if (refresh(&s))
            for (size_t j = 0; j < count; j++) step(&s, j, live(lease), now_ms());
        poll(&fd, 1, 100);
    }
    // No restart from here. Stop every job at once so their grace periods overlap.
    for (size_t j = 0; j < count; j++) {
        if (jobs[j].pid > 0 && !jobs[j].cleaning) {
            jobs[j].cleaning = true;
            jobs[j].term_sent = false;
        }
    }
    for (;;) {
        bool done = true;
        if (refresh(&s)) {
            for (size_t j = 0; j < count; j++) {
                step(&s, j, false, now_ms());
                if (jobs[j].pid > 0) done = false;
            }
        } else {
            done = false;
        }
        if (done) break;
        poll(NULL, 0, 100);
    }
    unlinkat(dir, "tether.session", 0);
    fsync(dir);
    send_byte(lease, TETHER_STOPPED);
    free(s.tracked);
    free(jobs);
    close(lock);
    close(dir);
    return 0;
}
