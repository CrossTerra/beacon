#ifndef BEACON_SECURITY_H
#define BEACON_SECURITY_H

#include <stdbool.h>
#include <stddef.h>
#include <stdint.h>
#include <sys/types.h>

// Checks the process at the other end of a connected Unix socket. The kernel's audit token for
// the connection identifies that exact process instance; its effective and real user ids must be
// `uid`, and its code must be valid and satisfy one of `requirements` (code requirement strings).
// Returns the index of the first requirement it satisfies, or -1. `pid` receives the peer's pid and `path`
// the executable its code reference names (empty when unknown).
int beacon_peer_match(int fd, uid_t uid, const char *const *requirements, int count, pid_t *pid,
                      char *path, size_t path_size);

// The running code's own requirement, `cdhash H"…"`, for tests that connect to themselves.
bool beacon_self_cdhash_requirement(char *out, size_t size);

// Whether the running process is `identifier` and signed with a certificate (not ad-hoc).
bool beacon_self_is(const char *identifier);

// The vault key: 32 random bytes in a generic password item. With a `tag` (the nonce of a pending vault
// file), an existing item is adopted only if it carries that tag; otherwise a new item is made with the tag
// and an access list that names only the running app's designated requirement. Any other existing item,
// untagged or tagged differently, is refused (BEACON_VAULT_KEY_EXISTS), never adopted: Beacon did not make
// it for this vault. Without a tag, only an existing item is read. Refuses before opening any keychain
// unless the running process is `identifier` signed with a certificate, so a test host or another program
// never reaches it. Returns 0, or an OSStatus, -1 or BEACON_VAULT_KEY_EXISTS.
#define BEACON_VAULT_KEY_EXISTS (-2)
int beacon_vault_key(const char *identifier, const char *service, const char *account, const char *tag, uint8_t key[32]);

// The designated requirement of the running process, or of the code at `path`, as text. Returns false for
// unsigned code or when `out` is too small.
bool beacon_self_designated_requirement(char *out, size_t size);
bool beacon_designated_requirement(const char *path, char *out, size_t size);

#endif
