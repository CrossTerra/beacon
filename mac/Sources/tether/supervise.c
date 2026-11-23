#include "supervise.h"

#include "TetherSupport.h"
#include "procs.h"

#include <errno.h>
#include <fcntl.h>
#include <poll.h>
#include <signal.h>
#include <stdarg.h>
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
#define ENTRY_SIZE (sizeof(TETHER_MARKER) + TOKEN_SIZE + TETHER_ID_MAX + 8)
#define LAUNCH_MAX (1 << 20) // The largest launch file: its argv and environment.
#define TETHER_SEND_MS 1000  // The longest a line to the app may block.
#define RUNNERS_MAX 256      // Runs alive at once in one session.

struct tracked {
    pid_t pid;
    uint64_t start_us;
    size_t job;
};

// A run the app asked for: a job that is never restarted, with the strings its spec points into.
struct runner {
    struct tether_spec spec;
    char id[TETHER_ID_MAX + 1];
    char *launch; // The launch file's bytes; argv and env point into it.
    char **argv;
    bool exit_reported;
};

struct job {
    const struct tether_spec *spec;
    struct runner *runner; // NULL for a fixed service.
    bool collector;        // The first fixed service: the only job whose state is sent as a bare byte.
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
    int lease;
    bool closed; // The app closed its end, or the lease failed: the session is ending.
    int dir;     // The state directory.
    const char *token;
    struct job *jobs;
    size_t count;
    char *const *env;
    unsigned grace_ms;
    struct tracked *tracked;
    size_t ntracked, cap;
    char command[TETHER_ID_MAX + 8];
    size_t ncommand;
    bool discarding; // The current command line is too long; drop it.
};

static volatile sig_atomic_t stopping;

int (*tether_kill)(pid_t, int) = kill;

static void stop(int sig) {
    (void)sig;
    stopping = 1;
}

static uint64_t now_ms(void) {
    return clock_gettime_nsec_np(CLOCK_MONOTONIC_RAW) / 1000000;
}

static bool live(const struct session *s) {
    return !stopping && !s->closed;
}

static void send_byte(int lease, char byte) {
    (void)send(lease, &byte, 1, MSG_DONTWAIT);
}

static void send_line(int lease, const char *format, ...) __attribute__((format(printf, 2, 3)));
// A whole line, or nothing more once the lease fails: the lease blocks for at most TETHER_SEND_MS
// (SO_SNDTIMEO), so an app that stops reading cannot hold tether, and a partial write is completed.
static void send_line(int lease, const char *format, ...) {
    char line[128];
    va_list args;
    va_start(args, format);
    int n = vsnprintf(line, sizeof(line), format, args);
    va_end(args);
    if (n <= 0 || n >= (int)sizeof(line)) return;
    size_t sent = 0;
    while (sent < (size_t)n) {
        ssize_t w = send(lease, line + sent, (size_t)n - sent, 0);
        if (w < 0 && errno == EINTR) continue;
        if (w <= 0) return;
        sent += (size_t)w;
    }
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
        if (sig) tether_kill(s->tracked[i].pid, sig);
    }
    if (sig && s->jobs[j].pid > 0) tether_kill(-s->jobs[j].pid, sig);
    return count;
}

// The collector's state, by kind and not by index: with the retired LaunchAgent installed there is no fixed
// service, and the first run takes job 0. A run reports only through its "#… <id>" lines.
static void report(struct session *s, size_t j, char state) {
    if (s->jobs[j].collector && !s->jobs[j].runner && live(s)) send_byte(s->lease, state);
}

// A run's exit: its code, or 128 plus the signal that ended it. Sent once.
static void report_exit(struct session *s, struct job *job, int code, int status) {
    if (!job->runner || job->runner->exit_reported) return;
    job->runner->exit_reported = true;
    int value = code == CLD_EXITED ? status : 128 + status;
    send_line(s->lease, "%c" "exited %s %d\n", TETHER_LINE, job->runner->id, value);
}

static void retry_later(struct session *s, size_t j, uint64_t now) {
    struct job *job = &s->jobs[j];
    if (now - job->started_at > 60000) job->failures = 0;
    // By default 1, 2, 4, 8, 16, then 30 seconds. The collector also has its own pass backoff.
    uint64_t delay = (uint64_t)(job->spec->backoff_ms ? job->spec->backoff_ms : 1000u) << job->failures;
    if (job->failures < 5) job->failures++;
    job->retry_at = now + (delay < 30000 ? delay : 30000);
    report(s, j, TETHER_WAITING);
}

static void start(struct session *s, size_t j, uint64_t now) {
    struct job *job = &s->jobs[j];
    size_t envc = 0;
    while (s->env[envc]) envc++;
    char **env = calloc(envc + 2, sizeof(*env));
    int pipefd[2] = {-1, -1};
    int log = job->spec->log
        ? open(job->spec->log, O_WRONLY | O_APPEND | O_CREAT | O_NOFOLLOW | O_CLOEXEC, 0600)
        : -1;
    if (env && (!job->spec->log || log >= 0) && (!job->spec->lifetime ||
                (pipe(pipefd) == 0 && fcntl(pipefd[0], F_SETFD, FD_CLOEXEC) == 0 &&
                 fcntl(pipefd[1], F_SETFD, FD_CLOEXEC) == 0))) {
        memcpy(env, s->env, envc * sizeof(*env));
        env[envc] = job->entry;
        job->pid = tether_spawn(job->spec->argv[0], job->spec->argv, env, pipefd[0], log);
    } else {
        job->pid = -1;
    }
    free(env);
    if (log >= 0) close(log);
    if (pipefd[0] >= 0) close(pipefd[0]);
    job->started_at = now;
    if (job->pid > 0) {
        job->lifetime = pipefd[1];
        report(s, j, TETHER_RUNNING);
        return;
    }
    job->pid = 0;
    if (pipefd[1] >= 0) close(pipefd[1]);
    if (job->spec->restart) {
        retry_later(s, j, now);
    } else {
        job->retired = true;
        report(s, j, TETHER_EXITED);
    }
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
            report_exit(s, job, info.si_code, info.si_status);
            // Decide on the next step, after a refresh has adopted the job's marked processes:
            // a detached descendant may have no tracked parent.
            return;
        }
    }
    if (job->cleaning) {
        if (members(s, j, 0) == 0) {
            bool reaped = job->pid > 0;
            int status = 0;
            if (reaped && waitpid(job->pid, &status, WNOHANG) != job->pid) return;
            // A run stopped before its exit was observed reports it from the reaped status.
            if (reaped && WIFEXITED(status)) report_exit(s, job, CLD_EXITED, WEXITSTATUS(status));
            if (reaped && WIFSIGNALED(status)) report_exit(s, job, CLD_KILLED, WTERMSIG(status));
            job->pid = 0;
            job->cleaning = false;
            if (job->lifetime >= 0) close(job->lifetime);
            job->lifetime = -1;
            if (!reaped) return; // A sweep at shutdown of a job with no leader.
            if (job->runner) send_line(s->lease, "%c" "gone %s\n", TETHER_LINE, job->runner->id);
            if (!job->spec->restart || job->clean_exit) {
                job->retired = true;
                report(s, j, TETHER_EXITED);
            } else {
                retry_later(s, j, now);
            }
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

static bool valid_id(const char *id) {
    size_t n = strlen(id);
    return n >= 1 && n <= TETHER_ID_MAX && strspn(id, "abcdefghijklmnopqrstuvwxyz0123456789_") == n;
}

static struct job *runner_job(struct session *s, const char *id) {
    for (size_t j = 0; j < s->count; j++)
        if (s->jobs[j].runner && strcmp(s->jobs[j].runner->id, id) == 0) return &s->jobs[j];
    return NULL;
}

static void free_runner(struct runner *r) {
    if (!r) return;
    free(r->argv);
    free(r->launch);
    free(r);
}

// Reads <state>/runs/<id>/launch: NUL-terminated fields, the working directory, its identity as
// "<device>:<inode>" (pinned when the app approved it), the argument count, the arguments (the
// first an absolute image path), then the environment. Returns the runner with its argv set, its
// environment in `env` (NULL-terminated, the job's marker still to be added), or NULL with errno set.
static struct runner *read_launch(int run, const char *id, char ***env, const char **cwd, dev_t *dev, ino_t *ino) {
    int fd = openat(run, "launch", O_RDONLY | O_NOFOLLOW | O_CLOEXEC);
    struct stat st;
    if (fd < 0) return NULL;
    if (fstat(fd, &st) != 0 || !S_ISREG(st.st_mode) || st.st_size < 4 || st.st_size > LAUNCH_MAX) {
        close(fd);
        errno = EINVAL;
        return NULL;
    }
    struct runner *r = calloc(1, sizeof(*r));
    size_t size = (size_t)st.st_size;
    char *buf = r ? malloc(size) : NULL;
    ssize_t got = buf ? read(fd, buf, size) : -1;
    close(fd);
    if (!buf || got != (ssize_t)size || buf[size - 1] != '\0') {
        free(buf);
        free(r);
        errno = EINVAL;
        return NULL;
    }
    r->launch = buf;
    size_t nfields = 0;
    for (size_t i = 0; i < size; i++) nfields += buf[i] == '\0';
    char **fields = calloc(nfields + 1, sizeof(*fields));
    if (!fields) {
        free_runner(r);
        return NULL;
    }
    for (size_t i = 0, k = 0; i < size; i += strlen(buf + i) + 1) fields[k++] = buf + i;
    char *end = NULL, *colon = NULL, *rest = NULL;
    unsigned long long device = nfields >= 4 ? strtoull(fields[1], &colon, 10) : 0;
    unsigned long long inode = colon && *colon == ':' ? strtoull(colon + 1, &rest, 10) : 0;
    long argc = nfields >= 4 ? strtol(fields[2], &end, 10) : 0;
    if (nfields < 4 || fields[0][0] != '/' || !rest || *rest || colon == fields[1] || !end || *end || argc < 1 ||
        argc > 64 || (size_t)argc + 3 > nfields || fields[3][0] != '/') {
        free(fields);
        free_runner(r);
        errno = EINVAL;
        return NULL;
    }
    *cwd = fields[0];
    *dev = (dev_t)device;
    *ino = (ino_t)inode;
    r->argv = calloc((size_t)argc + 1, sizeof(*r->argv));
    *env = calloc(nfields - 3 - (size_t)argc + 2, sizeof(**env));
    if (!r->argv || !*env) {
        // The caller frees env too: leave it nothing to free twice.
        free(*env);
        *env = NULL;
        free(fields);
        free_runner(r);
        return NULL;
    }
    memcpy(r->argv, fields + 3, (size_t)argc * sizeof(*r->argv));
    size_t envc = 0;
    for (size_t k = 3 + (size_t)argc; k < nfields; k++)
        if (strchr(fields[k], '=') && strncmp(fields[k], TETHER_MARKER, sizeof(TETHER_MARKER) - 1) != 0)
            (*env)[envc++] = fields[k];
    free(fields);
    snprintf(r->id, sizeof(r->id), "%s", id);
    r->spec = (struct tether_spec){.argv = r->argv};
    return r;
}

static size_t live_runners(const struct session *s) {
    size_t n = 0;
    for (size_t j = 0; j < s->count; j++) n += s->jobs[j].runner && !s->jobs[j].retired;
    return n;
}

// A job slot for a new run: a run that ended gives its slot back (nothing is tracked for it any more: it
// retired only once its last process was gone), so a long session does not grow without bound.
static struct job *runner_slot(struct session *s) {
    for (size_t j = 0; j < s->count; j++) {
        struct job *job = &s->jobs[j];
        // A run that ended, or a slot a failed launch left (no spec). A fixed service always has a spec.
        if ((job->runner || !job->spec) && job->retired && job->pid == 0 && !job->cleaning) {
            free_runner(job->runner);
            job->runner = NULL;
            return job;
        }
    }
    struct job *grown = realloc(s->jobs, (s->count + 1) * sizeof(*s->jobs));
    if (!grown) return NULL;
    s->jobs = grown;
    s->jobs[s->count] = (struct job){.lifetime = -1};
    s->count++;
    return &s->jobs[s->count - 1];
}

// Records that the run started, with its pid, once posix_spawn succeeded: the app reads it back after a
// crash to tell a run that started from one that never did.
static void mark_started(int run, pid_t pid) {
    int fd = openat(run, "started", O_WRONLY | O_CREAT | O_EXCL | O_NOFOLLOW | O_CLOEXEC, 0600);
    if (fd < 0) return;
    char text[32];
    int n = snprintf(text, sizeof(text), "%d\n", pid);
    if (n > 0) (void)!write(fd, text, (size_t)n);
    (void)fsync(fd);
    close(fd);
}

// Starts the run `id` as a new job: its own process group, never restarted, in the working directory
// the app pinned (opened once, compared by device and inode, and entered by descriptor, so a directory
// swapped in at that path is refused with ESTALE), with standard input from the run's prompt file and
// its output and errors in files beside it, created afresh.
static void launch(struct session *s, const char *id) {
    if (live_runners(s) >= RUNNERS_MAX || runner_job(s, id)) {
        send_line(s->lease, "%c" "failed %s %d\n", TETHER_LINE, id, runner_job(s, id) ? EEXIST : EAGAIN);
        return;
    }
    int runs = openat(s->dir, "runs", O_RDONLY | O_DIRECTORY | O_NOFOLLOW | O_CLOEXEC);
    int run = runs >= 0 ? openat(runs, id, O_RDONLY | O_DIRECTORY | O_NOFOLLOW | O_CLOEXEC) : -1;
    int error = errno;
    if (runs >= 0) close(runs);
    char **env = NULL;
    const char *cwd = NULL;
    dev_t dev = 0;
    ino_t ino = 0;
    struct runner *r = run >= 0 ? read_launch(run, id, &env, &cwd, &dev, &ino) : NULL;
    if (!r) error = errno;
    int dir = -1;
    if (r) {
        struct stat st;
        dir = open(cwd, O_RDONLY | O_DIRECTORY | O_CLOEXEC);
        if (dir < 0) {
            error = errno;
        } else if (fstat(dir, &st) != 0 || st.st_dev != dev || st.st_ino != ino) {
            error = ESTALE;
            close(dir);
            dir = -1;
        }
    }
    int in = -1, out = -1, err = -1;
    pid_t pid = -1;
    struct job *job = dir >= 0 ? runner_slot(s) : NULL;
    if (job) {
        *job = (struct job){.spec = &r->spec, .runner = r, .lifetime = -1};
        snprintf(job->entry, sizeof(job->entry), TETHER_MARKER "%s:r%s", s->token, id);
        size_t envc = 0;
        while (env[envc]) envc++;
        env[envc] = job->entry;
        in = openat(run, "prompt", O_RDONLY | O_NOFOLLOW | O_CLOEXEC);
        out = openat(run, "output", O_WRONLY | O_CREAT | O_EXCL | O_NOFOLLOW | O_CLOEXEC, 0600);
        err = openat(run, "errors", O_WRONLY | O_CREAT | O_EXCL | O_NOFOLLOW | O_CLOEXEC, 0600);
        int suspended = 0;
        pid = in >= 0 && out >= 0 && err >= 0 ? tether_spawn_held(r->argv[0], r->argv, env, in, out, err, dir, &suspended) : -1;
        error = errno;
        if (pid > 0) {
            job->pid = pid;
            job->started_at = now_ms();
            mark_started(run, pid);
            // Before it can run: the app records the group (the leader's pid) before anything the run does.
            send_line(s->lease, "%c" "started %s %d\n", TETHER_LINE, id, pid);
            if (suspended && tether_resume(pid) != 0) {
                tether_kill(pid, SIGKILL);
                job->cleaning = true;
                job->term_sent = true;
                job->kill_at = now_ms();
            }
        } else {
            // The slot stays free for the next run.
            *job = (struct job){.retired = true, .lifetime = -1};
        }
    } else if (dir >= 0) {
        error = ENOMEM;
    }
    if (in >= 0) close(in);
    if (out >= 0) close(out);
    if (err >= 0) close(err);
    if (dir >= 0) close(dir);
    if (run >= 0) close(run);
    free(env);
    if (pid > 0) {
        // `started` was sent before the process was resumed.
    } else {
        free_runner(r);
        send_line(s->lease, "%c" "failed %s %d\n", TETHER_LINE, id, error);
    }
}

// Stops a run as a cleanup stops any job: TERM to its group and tracked processes, KILL after the
// grace period. Its exit and its end are reported as they happen.
static void stop_run(struct session *s, const char *id) {
    struct job *job = runner_job(s, id);
    if (job && job->pid > 0 && !job->cleaning) {
        job->cleaning = true;
        job->term_sent = false;
    }
}

static void command(struct session *s, char *line) {
    if (strlen(line) < 3 || line[1] != ' ' || !valid_id(line + 2)) return;
    if (line[0] == 'L') launch(s, line + 2);
    if (line[0] == 'K') stop_run(s, line + 2);
}

// Reads what the app wrote: runner commands, one per line. EOF or an error ends the session.
static void read_commands(struct session *s) {
    char buf[512];
    for (;;) {
        ssize_t n = recv(s->lease, buf, sizeof(buf), MSG_DONTWAIT);
        if (n < 0 && errno == EINTR) continue;
        if (n < 0 && (errno == EAGAIN || errno == EWOULDBLOCK)) return;
        if (n <= 0) {
            s->closed = true;
            return;
        }
        for (ssize_t i = 0; i < n; i++) {
            if (buf[i] == '\n') {
                s->command[s->ncommand] = '\0';
                if (!s->discarding) command(s, s->command);
                s->ncommand = 0;
                s->discarding = false;
            } else if (s->ncommand + 1 < sizeof(s->command)) {
                s->command[s->ncommand++] = buf[i];
            } else {
                s->discarding = true;
            }
        }
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
// Returns false if some remain TETHER_GIVE_UP_MS after the grace period.
static bool sweep(const char *prefix, unsigned grace_ms) {
    bool term_sent = false;
    uint64_t kill_at = 0, give_up = now_ms() + grace_ms + TETHER_GIVE_UP_MS;
    for (;;) {
        if (now_ms() >= give_up) return false;
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
            if (match[k]) tether_kill(procs[k].pid, sig);
        free(match);
        free(procs);
        if (!count) return true;
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
    const struct timeval send_timeout = {.tv_sec = TETHER_SEND_MS / 1000, .tv_usec = (TETHER_SEND_MS % 1000) * 1000};
    (void)setsockopt(lease, SOL_SOCKET, SO_SNDTIMEO, &send_timeout, sizeof(send_timeout));
    int lock = openat(dir, "tether.lock", O_RDWR | O_CREAT | O_NOFOLLOW | O_CLOEXEC, 0600);
    if (lock < 0) return 1;
    if (flock(lock, LOCK_EX | LOCK_NB) != 0) {
        send_byte(lease, TETHER_BUSY);
        return 3;
    }

    char token[TOKEN_SIZE], prefix[ENTRY_SIZE];
    if (read_token(dir, token)) {
        snprintf(prefix, sizeof(prefix), TETHER_MARKER "%s:", token);
        if (!sweep(prefix, grace_ms)) return 4;
    }
    uint8_t random[(TOKEN_SIZE - 1) / 2];
    arc4random_buf(random, sizeof(random));
    for (size_t i = 0; i < sizeof(random); i++) snprintf(token + 2 * i, 3, "%02x", random[i]);
    if (!write_token(dir, token)) return 1;

    struct job *jobs = calloc(count ? count : 1, sizeof(*jobs));
    if (!jobs) return 1;
    for (size_t j = 0; j < count; j++) {
        jobs[j] = (struct job){.spec = &specs[j], .lifetime = -1, .collector = j == 0};
        snprintf(jobs[j].entry, sizeof(jobs[j].entry), TETHER_MARKER "%s:%zu", token, j);
    }
    struct session s = {.lease = lease, .dir = dir, .token = token, .jobs = jobs, .count = count, .env = env,
                        .grace_ms = grace_ms};
    if (live(&s)) send_byte(lease, ready);

    struct pollfd fd = {.fd = lease, .events = POLLIN};
    while (live(&s)) {
        read_commands(&s);
        if (!live(&s)) break;
        if (refresh(&s))
            for (size_t j = 0; j < s.count; j++) step(&s, j, live(&s), now_ms());
        poll(&fd, 1, 100);
    }
    s.closed = true;
    // No restart from here. Stop every job at once so their grace periods overlap. Jobs without a
    // leader (in backoff, or retired) are swept too: their marked processes are adopted on the next
    // refresh, and the session file is kept unless every job is confirmed empty after that.
    for (size_t j = 0; j < s.count; j++) {
        if (!s.jobs[j].cleaning) {
            s.jobs[j].cleaning = true;
            s.jobs[j].term_sent = false;
        }
    }
    uint64_t give_up = now_ms() + grace_ms + TETHER_GIVE_UP_MS;
    bool verified = false;
    while (now_ms() < give_up) {
        bool done = true;
        if (refresh(&s)) {
            for (size_t j = 0; j < s.count; j++) {
                step(&s, j, false, now_ms());
                if (s.jobs[j].pid > 0 || s.jobs[j].cleaning) done = false;
            }
        } else {
            done = false;
        }
        if (done) {
            verified = true;
            break;
        }
        poll(NULL, 0, 100);
    }
    if (verified) {
        unlinkat(dir, "tether.session", 0);
        fsync(dir);
        send_byte(lease, TETHER_STOPPED);
    }
    for (size_t j = 0; j < s.count; j++) {
        if (s.jobs[j].lifetime >= 0) close(s.jobs[j].lifetime);
        free_runner(s.jobs[j].runner);
    }
    free(s.tracked);
    free(s.jobs);
    close(lock);
    close(dir);
    return verified ? 0 : 4;
}
