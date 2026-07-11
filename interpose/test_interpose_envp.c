/* Unit tests for gl_env_key_contains_path and gl_reinject_envp_impl.
 *
 * Build:    cc -Wall -Wextra -O2 -o test_interpose_envp test_interpose_envp.c
 * Run:      ./test_interpose_envp     (exit 0 = all pass)
 * Or:       make test-unit-envp
 *
 * Regression coverage for issue #239: gl_reinject_envp used to back off
 * (return NULL, meaning "envp is fine as-is") whenever GL_ENV_KEY was
 * merely *present* in envp, regardless of its value. A parent process
 * setting DYLD_INSERT_LIBRARIES/LD_PRELOAD to an empty string or an
 * unrelated decoy path would silently defeat interposition for the child.
 * These tests assert that the returned envp always contains gl_lib_path
 * as one of GL_ENV_KEY's colon-separated entries whenever GL_ENV_KEY was
 * present with any value, and that the untouched-when-already-correct and
 * key-absent fast paths still behave as before.
 */

#include <stdio.h>
#include <stdlib.h>
#include <string.h>

#include "interpose_envp.h"

static int failures = 0;
static int total = 0;

#define CHECK(cond, fmt, ...) do {                                     \
    total++;                                                           \
    if (!(cond)) {                                                     \
        failures++;                                                    \
        fprintf(stderr, "FAIL %s:%d: " fmt "\n",                       \
                __func__, __LINE__, ##__VA_ARGS__);                    \
    }                                                                  \
} while (0)

#define GL_LIB_PATH "/opt/greenlight/libgreenlight-darwin-arm64.dylib"

/* Find the GL_ENV_KEY entry in a returned envp; NULL if absent. */
static const char *find_env_key(char *const envp[]) {
    if (!envp) return NULL;
    size_t key_len = strlen(GL_ENV_KEY);
    for (int i = 0; envp[i]; i++)
        if (strncmp(envp[i], GL_ENV_KEY "=", key_len + 1) == 0)
            return envp[i];
    return NULL;
}

static void free_envp(char *const *envp) {
    /* Entries point into caller-owned strings or a static buffer — only
       the array itself is heap-allocated. */
    free((void *)envp);
}

/* ---------- gl_env_key_contains_path ---------- */

static void test_contains_exact_single(void) {
    CHECK(gl_env_key_contains_path(GL_LIB_PATH, GL_LIB_PATH) == 1, "exact single value should match");
}

static void test_contains_empty_value(void) {
    CHECK(gl_env_key_contains_path("", GL_LIB_PATH) == 0, "empty value should not match");
}

static void test_contains_decoy_only(void) {
    CHECK(gl_env_key_contains_path("/tmp/decoy.dylib", GL_LIB_PATH) == 0, "decoy-only value should not match");
}

static void test_contains_decoy_then_ours(void) {
    char value[256];
    snprintf(value, sizeof(value), "/tmp/decoy.dylib:%s", GL_LIB_PATH);
    CHECK(gl_env_key_contains_path(value, GL_LIB_PATH) == 1, "should find our path in non-first position");
}

static void test_contains_prefix_not_match(void) {
    /* Substring-only match must not count — exact token compare only. */
    char value[256];
    snprintf(value, sizeof(value), "%s.decoy", GL_LIB_PATH);
    CHECK(gl_env_key_contains_path(value, GL_LIB_PATH) == 0, "prefix-only match should not count as contained");
}

/* ---------- gl_reinject_envp_impl: key absent ---------- */

static void test_key_absent_appends(void) {
    char *envp[] = { (char *)"PATH=/usr/bin", NULL };
    char *const *out = gl_reinject_envp_impl("/usr/local/bin/node", envp, GL_LIB_PATH);
    CHECK(out != NULL, "expected a rewritten envp when key is absent");
    if (out) {
        const char *entry = find_env_key(out);
        CHECK(entry != NULL, "expected GL_ENV_KEY entry to be appended");
        if (entry) CHECK(gl_env_key_contains_path(entry + strlen(GL_ENV_KEY) + 1, GL_LIB_PATH) == 1,
                          "appended entry should contain gl_lib_path: %s", entry);
        free_envp(out);
    }
}

/* ---------- gl_reinject_envp_impl: key present, already correct ---------- */

static void test_key_present_already_correct_sole_value(void) {
    char entry[256];
    snprintf(entry, sizeof(entry), "%s=%s", GL_ENV_KEY, GL_LIB_PATH);
    char *envp[] = { entry, NULL };
    char *const *out = gl_reinject_envp_impl("/usr/local/bin/node", envp, GL_LIB_PATH);
    CHECK(out == NULL, "fast path: already-correct sole value should return NULL (no rewrite)");
}

static void test_key_present_already_correct_in_list(void) {
    char entry[256];
    snprintf(entry, sizeof(entry), "%s=/tmp/other.dylib:%s", GL_ENV_KEY, GL_LIB_PATH);
    char *envp[] = { entry, NULL };
    char *const *out = gl_reinject_envp_impl("/usr/local/bin/node", envp, GL_LIB_PATH);
    CHECK(out == NULL, "fast path: our path present anywhere in the list should return NULL");
}

/* ---------- gl_reinject_envp_impl: key present, wrong/empty (the bug) ---------- */

static void test_key_present_empty_value_rewrites(void) {
    /* Acceptance criterion 1 & 5: KEY= (empty). */
    char entry[64];
    snprintf(entry, sizeof(entry), "%s=", GL_ENV_KEY);
    char *envp[] = { entry, NULL };
    char *const *out = gl_reinject_envp_impl("/usr/local/bin/node", envp, GL_LIB_PATH);
    CHECK(out != NULL, "empty value must be rewritten, not left as-is");
    if (out) {
        const char *e = find_env_key(out);
        CHECK(e != NULL, "expected GL_ENV_KEY entry present");
        if (e) CHECK(gl_env_key_contains_path(e + strlen(GL_ENV_KEY) + 1, GL_LIB_PATH) == 1,
                     "rewritten entry should contain gl_lib_path: %s", e);
        free_envp(out);
    }
}

static void test_key_present_decoy_only_rewrites(void) {
    /* Acceptance criterion 1 & 5: KEY=/tmp/decoy.dylib. */
    char entry[64];
    snprintf(entry, sizeof(entry), "%s=/tmp/decoy.dylib", GL_ENV_KEY);
    char *envp[] = { entry, NULL };
    char *const *out = gl_reinject_envp_impl("/usr/local/bin/node", envp, GL_LIB_PATH);
    CHECK(out != NULL, "decoy-only value must be rewritten");
    if (out) {
        const char *e = find_env_key(out);
        CHECK(e != NULL, "expected GL_ENV_KEY entry present");
        const char *val = e ? e + strlen(GL_ENV_KEY) + 1 : NULL;
        CHECK(val && gl_env_key_contains_path(val, GL_LIB_PATH) == 1,
              "rewritten entry should contain gl_lib_path: %s", e ? e : "(null)");
        CHECK(val && strstr(val, "/tmp/decoy.dylib") != NULL,
              "decoy should be preserved alongside our path: %s", e ? e : "(null)");
        free_envp(out);
    }
}

static void test_key_present_decoy_list_nonfirst_rewrites(void) {
    /* Acceptance criterion 5: KEY=/tmp/decoy.dylib:<gl_lib_path> is already
       correct (our path is present) — must NOT rewrite. This exercises the
       "non-first position" case from a different angle than the fast-path
       test above by using a longer decoy chain. */
    char entry[256];
    snprintf(entry, sizeof(entry), "%s=/tmp/decoy1.dylib:/tmp/decoy2.dylib:%s", GL_ENV_KEY, GL_LIB_PATH);
    char *envp[] = { entry, NULL };
    char *const *out = gl_reinject_envp_impl("/usr/local/bin/node", envp, GL_LIB_PATH);
    CHECK(out == NULL, "our path present in a multi-entry list should return NULL");
}

static void test_key_present_wrong_path_rewrites(void) {
    char entry[64];
    snprintf(entry, sizeof(entry), "%s=/tmp/decoy1.dylib:/tmp/decoy2.dylib", GL_ENV_KEY);
    char *envp[] = { entry, NULL };
    char *const *out = gl_reinject_envp_impl("/usr/local/bin/node", envp, GL_LIB_PATH);
    CHECK(out != NULL, "multi-entry list missing our path must be rewritten");
    if (out) {
        const char *e = find_env_key(out);
        const char *val = e ? e + strlen(GL_ENV_KEY) + 1 : NULL;
        CHECK(val && gl_env_key_contains_path(val, GL_LIB_PATH) == 1,
              "rewritten entry should contain gl_lib_path: %s", e ? e : "(null)");
        free_envp(out);
    }
}

static void test_key_present_wrong_no_double_entry(void) {
    /* The array must never contain two GL_ENV_KEY= entries after a rewrite
       — first/last-wins semantics vary by libc/dyld. */
    char entry[64];
    snprintf(entry, sizeof(entry), "%s=/tmp/decoy.dylib", GL_ENV_KEY);
    char *envp[] = { (char *)"PATH=/usr/bin", entry, (char *)"HOME=/root", NULL };
    char *const *out = gl_reinject_envp_impl("/usr/local/bin/node", envp, GL_LIB_PATH);
    CHECK(out != NULL, "expected a rewrite");
    if (out) {
        int key_count = 0;
        size_t key_len = strlen(GL_ENV_KEY);
        for (int i = 0; out[i]; i++)
            if (strncmp(out[i], GL_ENV_KEY "=", key_len + 1) == 0) key_count++;
        CHECK(key_count == 1, "expected exactly one GL_ENV_KEY entry, got %d", key_count);
        free_envp(out);
    }
}

#ifdef __APPLE__
/* ---------- gl_reinject_envp_impl: SIP-protected target ---------- */

static void test_sip_protected_strips_any_value(void) {
    char entry[64];
    snprintf(entry, sizeof(entry), "%s=%s", GL_ENV_KEY, GL_LIB_PATH);
    char *envp[] = { entry, NULL };
    /* Even a *correct* value must be stripped entirely for a SIP target —
       macOS 15+ SIGABRTs if the var is present at all. */
    char *const *out = gl_reinject_envp_impl("/usr/bin/true", envp, GL_LIB_PATH);
    CHECK(out != NULL, "expected a stripped envp for a SIP-protected target");
    if (out) {
        CHECK(find_env_key(out) == NULL, "GL_ENV_KEY must be fully absent for SIP targets");
        free_envp(out);
    }
}

static void test_sip_protected_no_key_no_copy(void) {
    char *envp[] = { (char *)"PATH=/usr/bin", NULL };
    char *const *out = gl_reinject_envp_impl("/usr/bin/true", envp, GL_LIB_PATH);
    CHECK(out == NULL, "no key present for SIP target should return NULL (nothing to strip)");
}
#endif

/* ---------- runner ---------- */

int main(void) {
    test_contains_exact_single();
    test_contains_empty_value();
    test_contains_decoy_only();
    test_contains_decoy_then_ours();
    test_contains_prefix_not_match();

    test_key_absent_appends();

    test_key_present_already_correct_sole_value();
    test_key_present_already_correct_in_list();

    test_key_present_empty_value_rewrites();
    test_key_present_decoy_only_rewrites();
    test_key_present_decoy_list_nonfirst_rewrites();
    test_key_present_wrong_path_rewrites();
    test_key_present_wrong_no_double_entry();

#ifdef __APPLE__
    test_sip_protected_strips_any_value();
    test_sip_protected_no_key_no_copy();
#endif

    if (failures == 0) {
        printf("OK: %d/%d checks passed\n", total, total);
        return 0;
    } else {
        fprintf(stderr, "FAIL: %d/%d checks failed\n", failures, total);
        return 1;
    }
}
