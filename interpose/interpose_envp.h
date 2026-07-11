/* Pure DYLD_INSERT_LIBRARIES/LD_PRELOAD re-injection helpers used by
   interpose.c. Lives in a header (mirroring interpose_json.h) so the unit
   tests in test_interpose_envp.c can exercise gl_reinject_envp_impl
   directly without pulling in the rest of the library (which depends on
   libc interposition machinery that doesn't make sense in a host-process
   test). */

#ifndef GREENLIGHT_INTERPOSE_ENVP_H
#define GREENLIGHT_INTERPOSE_ENVP_H

#include <stdlib.h>
#include <string.h>
#include <stdio.h>

#ifndef GL_ENV_KEY
#ifdef __APPLE__
#define GL_ENV_KEY "DYLD_INSERT_LIBRARIES"
#else
#define GL_ENV_KEY "LD_PRELOAD"
#endif
#endif

#ifdef __APPLE__
/* SIP-protected executable prefixes. On macOS 15+, posix_spawn/execve of a
   binary under any of these paths with DYLD_INSERT_LIBRARIES in envp causes
   the child to SIGABRT (Sonoma silently stripped the var; Sequoia aborts).
   We must not reinject the var when targeting these. */
static int gl_is_sip_protected_exec(const char *path) {
    if (!path || path[0] != '/') return 0;
    return strncmp(path, "/System/",      8)  == 0 ||
           strncmp(path, "/bin/",         5)  == 0 ||
           strncmp(path, "/sbin/",        6)  == 0 ||
           strncmp(path, "/usr/bin/",     9)  == 0 ||
           strncmp(path, "/usr/sbin/",    10) == 0 ||
           strncmp(path, "/usr/libexec/", 13) == 0;
}
#endif

/* Check whether `value` (the current GL_ENV_KEY value from a child's envp)
   already contains `lib_path` as one of its colon-separated entries (also
   space-separated on Linux, matching ld.so's LD_PRELOAD convention). Exact
   string match per entry. */
static int gl_env_key_contains_path(const char *value, const char *lib_path) {
    if (!value || !lib_path || !lib_path[0]) return 0;
    size_t lib_len = strlen(lib_path);
    const char *p = value;
    while (*p) {
#ifdef __APPLE__
        while (*p == ':') p++;
#else
        while (*p == ':' || *p == ' ') p++;
#endif
        if (!*p) break;
        const char *tok_start = p;
#ifdef __APPLE__
        while (*p && *p != ':') p++;
#else
        while (*p && *p != ':' && *p != ' ') p++;
#endif
        size_t tok_len = (size_t)(p - tok_start);
        if (tok_len == lib_len && strncmp(tok_start, lib_path, lib_len) == 0)
            return 1;
    }
    return 0;
}

/* Build a modified envp for an exec. The caller must free the returned array
   (but not the strings — they point into the original envp or a static
   buffer). Returns NULL when the original envp can be used as-is.

   On macOS targeting a SIP-protected binary: actively STRIP GL_ENV_KEY from
   envp. macOS 15+ aborts the child with SIGABRT if the var is present, even
   though dyld would strip it; the abort happens before dyld runs. If the var
   isn't present, no copy is needed.

   Otherwise: if GL_ENV_KEY is missing, return a new envp with it appended.
   If present, its value is parsed as a colon/space-separated list — if
   gl_lib_path is already one of the entries, return NULL (fast path); if
   not, the entry is rewritten with gl_lib_path prepended so our library is
   guaranteed loaded regardless of what the child set. `gl_lib_path` is
   passed explicitly (rather than read from global state) so this is
   independently testable. */
static char *const *gl_reinject_envp_impl(const char *path, char *const envp[], const char *gl_lib_path) {
    if (!envp) return NULL;
    const size_t key_len = strlen(GL_ENV_KEY);

#ifdef __APPLE__
    if (gl_is_sip_protected_exec(path)) {
        /* Strip GL_ENV_KEY from envp if present. */
        int count = 0, found = -1;
        for (int i = 0; envp[i]; i++) {
            if (found < 0 && strncmp(envp[i], GL_ENV_KEY "=", key_len + 1) == 0)
                found = i;
            count++;
        }
        if (found < 0) return NULL; /* nothing to strip */
        char **new_envp = malloc(count * sizeof(char *)); /* count entries + NULL = count (one removed) */
        if (!new_envp) return NULL;
        int j = 0;
        for (int i = 0; i < count; i++) {
            if (i == found) continue;
            new_envp[j++] = envp[i];
        }
        new_envp[j] = NULL;
        return (char *const *)new_envp;
    }
#else
    (void)path;
#endif

    if (!gl_lib_path || !gl_lib_path[0]) return NULL;
    int count = 0, found = -1;
    for (int i = 0; envp[i]; i++) {
        if (found < 0 && strncmp(envp[i], GL_ENV_KEY "=", key_len + 1) == 0)
            found = i;
        count++;
    }

    /* Build "KEY=value" string in a static buffer (one per thread is fine —
       posix_spawn/execve aren't called concurrently on the same thread).
       Sized for gl_lib_path (<=4096) prepended to a same-size existing
       value plus separators, with slack for the key/'='/':'. */
    static _Thread_local char env_entry[2 * 4096 + sizeof(GL_ENV_KEY) + 8];

    if (found >= 0) {
        const char *existing = envp[found] + key_len + 1;
        if (gl_env_key_contains_path(existing, gl_lib_path))
            return NULL; /* already present — fast path */

        /* Trim leading whitespace so we don't leave a dangling separator
           when the existing value is empty/whitespace-only. */
        while (*existing == ' ' || *existing == '\t') existing++;
        if (*existing)
            snprintf(env_entry, sizeof(env_entry), "%s=%s:%s", GL_ENV_KEY, gl_lib_path, existing);
        else
            snprintf(env_entry, sizeof(env_entry), "%s=%s", GL_ENV_KEY, gl_lib_path);

        /* Replace in place — do not append a second GL_ENV_KEY entry, whose
           handling (first-wins vs last-wins) varies by libc/dyld. */
        char **new_envp = malloc((count + 1) * sizeof(char *));
        if (!new_envp) return NULL;
        for (int i = 0; i < count; i++)
            new_envp[i] = (i == found) ? env_entry : envp[i];
        new_envp[count] = NULL;
        return (char *const *)new_envp;
    }

    snprintf(env_entry, sizeof(env_entry), "%s=%s", GL_ENV_KEY, gl_lib_path);

    /* Allocate new envp: original entries + our entry + NULL */
    char **new_envp = malloc((count + 2) * sizeof(char *));
    if (!new_envp) return NULL;

    for (int i = 0; i < count; i++)
        new_envp[i] = envp[i];
    new_envp[count] = env_entry;
    new_envp[count + 1] = NULL;
    return (char *const *)new_envp;
}

#endif /* GREENLIGHT_INTERPOSE_ENVP_H */
