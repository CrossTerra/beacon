#include "procs.h"

#include <stdlib.h>
#include <string.h>
#include <sys/proc.h>
#include <sys/sysctl.h>
#include <unistd.h>

int tether_list(struct tether_proc **out) {
    *out = NULL;
    int mib[] = {CTL_KERN, KERN_PROC, KERN_PROC_UID, (int)getuid()};
    for (int attempt = 0; attempt < 4; attempt++) {
        size_t size = 0;
        if (sysctl(mib, 4, NULL, &size, NULL, 0) != 0) return -1;
        size += 64 * sizeof(struct kinfo_proc);
        struct kinfo_proc *kinfo = malloc(size);
        if (!kinfo) return -1;
        if (sysctl(mib, 4, kinfo, &size, NULL, 0) != 0) {
            free(kinfo);
            continue; // The table grew between the two calls.
        }
        size_t n = size / sizeof(*kinfo);
        struct tether_proc *procs = calloc(n ? n : 1, sizeof(*procs));
        if (!procs) {
            free(kinfo);
            return -1;
        }
        int count = 0;
        for (size_t i = 0; i < n; i++) {
            const struct kinfo_proc *k = &kinfo[i];
            if (k->kp_proc.p_stat == SZOMB || k->kp_proc.p_pid == getpid()) continue;
            procs[count++] = (struct tether_proc){
                .pid = k->kp_proc.p_pid,
                .ppid = k->kp_eproc.e_ppid,
                .pgid = k->kp_eproc.e_pgid,
                .start_us = (uint64_t)k->kp_proc.p_starttime.tv_sec * 1000000u +
                            (uint64_t)k->kp_proc.p_starttime.tv_usec,
            };
        }
        free(kinfo);
        *out = procs;
        return count;
    }
    return -1;
}

bool tether_env_has(pid_t pid, const char *entry, bool prefix) {
    int mib[] = {CTL_KERN, KERN_PROCARGS2, pid};
    size_t size = 0;
    if (sysctl(mib, 3, NULL, &size, NULL, 0) != 0 || size < sizeof(int)) return false;
    char *buf = malloc(size);
    if (!buf) return false;
    bool found = false;
    if (sysctl(mib, 3, buf, &size, NULL, 0) == 0 && size >= sizeof(int)) {
        // argc, the executable path, NUL padding, argc arguments, then the environment.
        int argc;
        memcpy(&argc, buf, sizeof(argc));
        const char *p = buf + sizeof(argc), *end = buf + size;
        p += strnlen(p, (size_t)(end - p));
        while (p < end && *p == '\0') p++;
        for (int i = 0; i < argc && p < end; i++) p += strnlen(p, (size_t)(end - p)) + 1;
        size_t length = strlen(entry);
        while (!found && p < end && *p) {
            size_t n = strnlen(p, (size_t)(end - p));
            found = prefix ? n >= length && memcmp(p, entry, length) == 0 : n == length && memcmp(p, entry, n) == 0;
            p += n + 1;
        }
    }
    free(buf);
    return found;
}
