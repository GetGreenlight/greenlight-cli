/* Path canonicalization + classification helpers used by interpose.c.
 * Lives in a header (like interpose_json.h) so the unit tests in
 * test_pathnorm.c can exercise them directly as a host process, without
 * pulling in the libc-interposition machinery.
 *
 * INVARIANT (issue #241): every classifier below (gl_is_system_path,
 * gl_is_agent_internal, gl_is_temp_path, gl_is_dotfile, gl_is_project_file)
 * does raw prefix/substring matching and MUST only ever be called on a
 * path that has already been through gl_canonicalize(). Classifying a
 * raw, uncanonicalized path lets a `..` traversal or a symlink spoof the
 * prefix match (e.g. "/tmp/../Users/me/.ssh/id_rsa" matches the "/tmp/"
 * prefix and is silently treated as a temp file). See gl_canonicalize's
 * doc comment and cli/CLAUDE.md.
 */

#ifndef GREENLIGHT_INTERPOSE_PATHNORM_H
#define GREENLIGHT_INTERPOSE_PATHNORM_H

#include <errno.h>
#include <limits.h>
#include <stdio.h>
#include <stdlib.h>
#include <string.h>
#include <sys/stat.h>

#ifndef GL_PATH_MAX
#define GL_PATH_MAX PATH_MAX
#endif

/* ---------- canonicalization ---------- */

/* Lexically collapses "." and ".." components in an absolute, "/"-joined
 * path string, without touching the filesystem. Safe to use ONLY on a
 * path whose components don't exist on disk (so none of them can be a
 * symlink) -- gl_canonicalize uses this solely on the non-existent
 * trailing remainder after realpath() has already resolved the existing,
 * symlink-free ancestor. Applying this to an arbitrary (possibly
 * symlink-containing) path would be wrong: "a/../b" is not always the
 * same as "b" when `a` is a symlink.
 */
static void gl_lexical_clean_abs(const char *in, char *out, size_t out_len) {
    char work[GL_PATH_MAX];
    strncpy(work, in, sizeof(work) - 1);
    work[sizeof(work) - 1] = '\0';

    char *parts[256];
    int n = 0;
    char *save = NULL;
    for (char *tok = strtok_r(work, "/", &save); tok; tok = strtok_r(NULL, "/", &save)) {
        if (strcmp(tok, ".") == 0 || tok[0] == '\0') continue;
        if (strcmp(tok, "..") == 0) {
            if (n > 0) n--;
            continue;
        }
        if (n < (int)(sizeof(parts) / sizeof(parts[0]))) parts[n++] = tok;
    }

    if (out_len == 0) return;
    out[0] = '\0';
    if (n == 0) {
        strncpy(out, "/", out_len - 1);
        out[out_len - 1] = '\0';
        return;
    }
    for (int i = 0; i < n; i++) {
        strncat(out, "/", out_len - strlen(out) - 1);
        strncat(out, parts[i], out_len - strlen(out) - 1);
    }
}

/* Canonicalizes `in` (must already be an absolute path -- callers resolve
 * relative paths against cwd/dirfd first, as interpose.c already does)
 * into `out`, resolving ".." traversal and symlinks. This is the fix for
 * issue #241: is_temp_path/is_system_path/etc. match on raw prefixes, so
 * "/tmp/../Users/me/.ssh/id_rsa" would otherwise be classified as a temp
 * file and silently allowed.
 *
 * realpath(3) requires the target to exist, which doesn't hold for
 * O_CREAT targets whose leaf is new. So this walks up from `in` to find
 * the longest EXISTING ancestor (via lstat, so a symlink leaf is itself
 * the boundary -- its target is resolved by realpath in the next step),
 * realpath()'s that ancestor (resolving any ".."/symlinks within it),
 * and lexically rejoins the stripped-off non-existent remainder (safe:
 * those components can't be symlinks since they don't exist on disk).
 *
 * Returns 0 on success. Returns -1 on failure (e.g. realpath() hits
 * ELOOP/EACCES on the existing ancestor, or `in` isn't absolute) -- on
 * failure `out` is left unspecified and callers MUST fail closed (treat
 * the operation as requiring a permission prompt), never fall back to
 * classifying the raw path.
 */
static int gl_canonicalize(const char *in, char *out, size_t out_len) {
    if (!in || in[0] != '/' || out_len == 0) return -1;
    if (strlen(in) >= GL_PATH_MAX) return -1;

    char work[GL_PATH_MAX];
    strcpy(work, in);

    /* remainder accumulates the components stripped off the tail while
       walking up to an existing ancestor, in original left-to-right order. */
    char remainder[GL_PATH_MAX] = {0};

    for (int iter = 0; iter < 4096; iter++) {
        struct stat st;
        if (lstat(work, &st) == 0) break;
        if (errno != ENOENT && errno != ENOTDIR) {
            /* Unexpected/unsafe error (EACCES, ELOOP, ENAMETOOLONG, ...) --
               fail closed rather than guessing. */
            return -1;
        }

        char *slash = strrchr(work, '/');
        if (!slash) return -1; /* unreachable: work is always absolute */

        char component[GL_PATH_MAX];
        strncpy(component, slash + 1, sizeof(component) - 1);
        component[sizeof(component) - 1] = '\0';

        if (component[0]) {
            if (remainder[0]) {
                char tmp[GL_PATH_MAX];
                int n = snprintf(tmp, sizeof(tmp), "%s/%s", component, remainder);
                if (n < 0 || (size_t)n >= sizeof(tmp)) return -1;
                strcpy(remainder, tmp);
            } else {
                strncpy(remainder, component, sizeof(remainder) - 1);
                remainder[sizeof(remainder) - 1] = '\0';
            }
        }

        if (slash == work) {
            /* work was "/component" -- next boundary is the root, which
               always exists, so lstat("/") will succeed on the next pass. */
            work[1] = '\0';
        } else {
            *slash = '\0';
        }
    }

    char resolved[GL_PATH_MAX];
    if (realpath(work, resolved) == NULL) return -1;

    if (!remainder[0]) {
        if (strlen(resolved) >= out_len) return -1;
        strcpy(out, resolved);
        return 0;
    }

    char joined[GL_PATH_MAX];
    int n = snprintf(joined, sizeof(joined), "%s/%s", resolved, remainder);
    if (n < 0 || (size_t)n >= sizeof(joined)) return -1;

    /* The remainder may itself contain ".."/"." (e.g. the caller opened
       ".../nonexistent/../etc/passwd" where "nonexistent" doesn't exist).
       Those components can't be symlinks (they don't exist), so a pure
       lexical collapse -- now anchored on the fully-resolved `resolved`
       prefix -- is correct. */
    gl_lexical_clean_abs(joined, out, out_len);
    return 0;
}

/* ---------- classification (pure -- callers pass in any global state) ---------- */

static int gl_is_system_path(const char *path) {
    return strncmp(path, "/System/", 8) == 0 ||
           strncmp(path, "/Library/", 9) == 0 ||
           strncmp(path, "/usr/", 5) == 0 ||
           strncmp(path, "/dev/", 5) == 0 ||
           strncmp(path, "/etc/", 5) == 0 ||
           strncmp(path, "/var/", 5) == 0 ||
           strncmp(path, "/sbin/", 6) == 0 ||
           strncmp(path, "/bin/", 5) == 0 ||
           strncmp(path, "/opt/", 5) == 0 ||
           strncmp(path, "/Applications/", 14) == 0 ||
           strncmp(path, "/private/", 9) == 0 ||
           strncmp(path, "/sys/", 5) == 0 ||     /* Linux sysfs */
           strncmp(path, "/proc/", 6) == 0 ||    /* Linux procfs */
           strncmp(path, "/run/", 5) == 0;       /* Linux runtime */
}

static int gl_is_agent_internal(const char *path) {
    return strstr(path, "/.claude/") != NULL ||
           strstr(path, "/.local/share/claude/") != NULL ||
           strstr(path, "/.local/state/claude/") != NULL ||
           strstr(path, "/Library/Caches/claude") != NULL ||
           strstr(path, "/Library/Application Support/Claude") != NULL ||
           strstr(path, "/.cursor/") != NULL ||
           strstr(path, "/.local/share/cursor-agent/") != NULL ||
           strstr(path, "/Library/Caches/cursor") != NULL ||
           strstr(path, "/.codex/") != NULL ||
           strstr(path, "/.copilot/") != NULL ||
           strstr(path, "/Library/Caches/copilot") != NULL ||
           strstr(path, "/.gemini/") != NULL ||
           strstr(path, "/.pi/") != NULL;
}

static int gl_is_temp_path(const char *path, const char *tmpdir_path) {
    if (strncmp(path, "/tmp/", 5) == 0 || strncmp(path, "/private/tmp/", 13) == 0)
        return 1;
    if (tmpdir_path && tmpdir_path[0] && strncmp(path, tmpdir_path, strlen(tmpdir_path)) == 0)
        return 1;
    return 0;
}

static int gl_is_dotfile(const char *path) {
    /* Check for /.<name> component (hidden dir/file) in the path after home */
    const char *p = path;
    while ((p = strstr(p, "/.")) != NULL) {
        /* Skip /.. (parent dir) and /. (current dir) */
        if (p[2] != '.' && p[2] != '/' && p[2] != '\0')
            return 1;
        p += 2;
    }
    return 0;
}

/* Returns 1 if path is in the project directory and is a user file
   (not system, not claude internal, not temp, not dotfile) */
static int gl_is_project_file(const char *path, const char *project_dir) {
    if (!project_dir || !project_dir[0]) return 0;
    size_t plen = strlen(project_dir);
    if (strncmp(path, project_dir, plen) != 0) return 0;
    if (path[plen] != '/' && path[plen] != '\0') return 0;
    if (gl_is_dotfile(path)) return 0;
    return 1;
}

#endif /* GREENLIGHT_INTERPOSE_PATHNORM_H */
