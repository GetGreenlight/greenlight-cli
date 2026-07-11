/* Unit tests for gl_canonicalize and the gl_is_* path classifiers
 * (issue #241).
 *
 * Build:    cc -Wall -Wextra -O2 -o test_pathnorm test_pathnorm.c
 * Run:      ./test_pathnorm     (exit 0 = all pass)
 * Or:       make test-pathnorm
 *
 * These are the first unit tests for the classifier functions -- until
 * now they were only covered indirectly by the integration/black-box
 * tests. The core regression under test: is_temp_path/is_system_path/
 * is_agent_internal/is_dotfile do raw prefix/substring matching, so a
 * ".." traversal or symlink through a trusted prefix (e.g.
 * "/tmp/../Users/me/.ssh/id_rsa") must be resolved by gl_canonicalize
 * BEFORE classification, or the classifiers are trivially spoofed.
 */

#include <stdio.h>
#include <stdlib.h>
#include <string.h>
#include <unistd.h>
#include <sys/stat.h>

#include "interpose_pathnorm.h"

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

#define CHECK_STR_EQ(actual, expected) do {                            \
    total++;                                                           \
    if (strcmp((actual), (expected)) != 0) {                           \
        failures++;                                                    \
        fprintf(stderr, "FAIL %s:%d: expected \"%s\" got \"%s\"\n",    \
                __func__, __LINE__, (expected), (actual));             \
    }                                                                  \
} while (0)

/* ---------- gl_is_system_path ---------- */

static void test_is_system_path(void) {
    CHECK(gl_is_system_path("/etc/passwd"), "etc should be system");
    CHECK(gl_is_system_path("/private/etc/passwd"), "private should be system");
    CHECK(gl_is_system_path("/usr/bin/ls"), "usr should be system");
    CHECK(!gl_is_system_path("/Users/me/notes.txt"), "home dir should not be system");
    CHECK(!gl_is_system_path("/Users/me/etc/foo"), "path containing 'etc' component elsewhere should not match /etc/ prefix");
}

/* ---------- gl_is_agent_internal ---------- */

static void test_is_agent_internal(void) {
    CHECK(gl_is_agent_internal("/Users/me/.claude/plugins/foo.md"), "should match .claude/");
    CHECK(!gl_is_agent_internal("/Users/me/project/notes.txt"), "regular project file should not match");
}

/* ---------- gl_is_temp_path ---------- */

static void test_is_temp_path(void) {
    CHECK(gl_is_temp_path("/tmp/foo.txt", ""), "/tmp/ should be temp");
    CHECK(gl_is_temp_path("/private/tmp/foo.txt", ""), "/private/tmp/ should be temp");
    CHECK(gl_is_temp_path("/custom/tmpdir/foo.txt", "/custom/tmpdir"), "custom TMPDIR should be temp when configured");
    CHECK(!gl_is_temp_path("/custom/tmpdir/foo.txt", ""), "custom TMPDIR should not match when unset");
    CHECK(!gl_is_temp_path("/Users/me/.ssh/id_rsa", ""), "home dir should not be temp");
}

/* ---------- gl_is_dotfile ---------- */

static void test_is_dotfile(void) {
    CHECK(gl_is_dotfile("/Users/me/.ssh/id_rsa"), "path through a dotdir should be a dotfile");
    CHECK(gl_is_dotfile("/Users/me/.env"), "dotfile leaf should be a dotfile");
    CHECK(!gl_is_dotfile("/Users/me/notes.txt"), "plain file should not be a dotfile");
    CHECK(!gl_is_dotfile("/Users/me/../etc/passwd"), "a bare '..' component must not itself be classified as a dotfile");
}

/* ---------- gl_is_project_file ---------- */

static void test_is_project_file(void) {
    CHECK(gl_is_project_file("/Users/me/proj/file.txt", "/Users/me/proj"), "file inside project dir");
    CHECK(gl_is_project_file("/Users/me/proj", "/Users/me/proj"), "bare project dir itself");
    CHECK(!gl_is_project_file("/Users/me/proj2/file.txt", "/Users/me/proj"),
          "sibling dir with project dir as a string prefix must not match (boundary check)");
    CHECK(!gl_is_project_file("/Users/me/other/file.txt", "/Users/me/proj"), "unrelated dir should not match");
    CHECK(!gl_is_project_file("/Users/me/proj/.git/config", "/Users/me/proj"), "dotfile inside project should not match");
    CHECK(!gl_is_project_file("/Users/me/proj/file.txt", ""), "unset project dir should never match");
}

/* ---------- gl_canonicalize ---------- */

/* Test fixture: a scratch directory tree under the real system tmp dir,
   used to exercise realpath()/symlink resolution against actual inodes. */
static char fixture_root[GL_PATH_MAX];

static void fixture_setup(void) {
    /* Deliberately NOT under /tmp: gl_is_temp_path hardcodes "/tmp/" and
       "/private/tmp/" as always-temp, which would make the traversal
       regression test below trivially pass/fail on that unrelated check
       instead of exercising the "custom tmpdir" configuration path. */
    char tmpl[] = "/var/tmp/gl_pathnorm_test.XXXXXX";
    char *d = mkdtemp(tmpl);
    if (!d) {
        fprintf(stderr, "mkdtemp failed\n");
        exit(1);
    }
    /* Resolve to the real path up front (macOS: /tmp -> /private/tmp) so
       test expectations don't have to special-case the symlink. */
    if (!realpath(d, fixture_root)) {
        fprintf(stderr, "realpath(mkdtemp result) failed\n");
        exit(1);
    }

    char path[GL_PATH_MAX];
    snprintf(path, sizeof(path), "%s/sub", fixture_root);
    mkdir(path, 0700);
    snprintf(path, sizeof(path), "%s/sub/deeper", fixture_root);
    mkdir(path, 0700);

    snprintf(path, sizeof(path), "%s/target.txt", fixture_root);
    FILE *f = fopen(path, "w");
    if (f) { fputs("secret", f); fclose(f); }

    snprintf(path, sizeof(path), "%s/link_to_target", fixture_root);
    char dest[GL_PATH_MAX];
    snprintf(dest, sizeof(dest), "%s/target.txt", fixture_root);
    symlink(dest, path);

    /* Symlink loop for the ELOOP failure case. */
    char loop_a[GL_PATH_MAX], loop_b[GL_PATH_MAX];
    snprintf(loop_a, sizeof(loop_a), "%s/loop_a", fixture_root);
    snprintf(loop_b, sizeof(loop_b), "%s/loop_b", fixture_root);
    symlink(loop_b, loop_a);
    symlink(loop_a, loop_b);
}

static void test_canonicalize_no_traversal(void) {
    char path[GL_PATH_MAX], out[GL_PATH_MAX];
    snprintf(path, sizeof(path), "%s/target.txt", fixture_root);
    int rc = gl_canonicalize(path, out, sizeof(out));
    CHECK(rc == 0, "canonicalize of a plain existing path should succeed");
    CHECK_STR_EQ(out, path);
}

static void test_canonicalize_dotdot_traversal_existing_target(void) {
    /* This is the read-exfiltration case from the issue: "/tmp/../X"
       reaching an existing file outside the temp prefix. Here we
       generalize it to fixture_root/sub/../target.txt to avoid depending
       on real files outside the sandbox. */
    char path[GL_PATH_MAX], out[GL_PATH_MAX], expected[GL_PATH_MAX];
    snprintf(path, sizeof(path), "%s/sub/../target.txt", fixture_root);
    snprintf(expected, sizeof(expected), "%s/target.txt", fixture_root);
    int rc = gl_canonicalize(path, out, sizeof(out));
    CHECK(rc == 0, "canonicalize should succeed");
    CHECK_STR_EQ(out, expected);
}

static void test_canonicalize_dotdot_traversal_deep(void) {
    char path[GL_PATH_MAX], out[GL_PATH_MAX], expected[GL_PATH_MAX];
    snprintf(path, sizeof(path), "%s/sub/deeper/../../target.txt", fixture_root);
    snprintf(expected, sizeof(expected), "%s/target.txt", fixture_root);
    int rc = gl_canonicalize(path, out, sizeof(out));
    CHECK(rc == 0, "canonicalize should succeed");
    CHECK_STR_EQ(out, expected);
}

static void test_canonicalize_ocreat_nonexistent_leaf(void) {
    /* The write case: target.txt doesn't exist yet (O_CREAT), but its
       parent does -- gl_canonicalize must still resolve the traversal
       instead of bailing because the full path doesn't stat. */
    char path[GL_PATH_MAX], out[GL_PATH_MAX], expected[GL_PATH_MAX];
    snprintf(path, sizeof(path), "%s/sub/../newfile.txt", fixture_root);
    snprintf(expected, sizeof(expected), "%s/newfile.txt", fixture_root);
    int rc = gl_canonicalize(path, out, sizeof(out));
    CHECK(rc == 0, "canonicalize of an O_CREAT target should succeed");
    CHECK_STR_EQ(out, expected);
}

static void test_canonicalize_ocreat_nested_nonexistent(void) {
    /* Multiple non-existent trailing components. */
    char path[GL_PATH_MAX], out[GL_PATH_MAX], expected[GL_PATH_MAX];
    snprintf(path, sizeof(path), "%s/sub/newdir/newfile.txt", fixture_root);
    snprintf(expected, sizeof(expected), "%s/sub/newdir/newfile.txt", fixture_root);
    int rc = gl_canonicalize(path, out, sizeof(out));
    CHECK(rc == 0, "canonicalize with a multi-component nonexistent tail should succeed");
    CHECK_STR_EQ(out, expected);
}

static void test_canonicalize_symlink_confused_deputy(void) {
    /* AC3: open("/tmp/link", O_WRONLY) where link -> a file outside the
       trusted prefix must classify against the resolved target, not the
       symlink's own path. */
    char path[GL_PATH_MAX], out[GL_PATH_MAX], expected[GL_PATH_MAX];
    snprintf(path, sizeof(path), "%s/link_to_target", fixture_root);
    snprintf(expected, sizeof(expected), "%s/target.txt", fixture_root);
    int rc = gl_canonicalize(path, out, sizeof(out));
    CHECK(rc == 0, "canonicalize through a symlink should succeed");
    CHECK_STR_EQ(out, expected);
}

static void test_canonicalize_symlink_loop_fails_closed(void) {
    char path[GL_PATH_MAX], out[GL_PATH_MAX];
    snprintf(path, sizeof(path), "%s/loop_a", fixture_root);
    int rc = gl_canonicalize(path, out, sizeof(out));
    CHECK(rc == -1, "a symlink loop must fail closed (ELOOP), not silently allow");
}

static void test_canonicalize_rejects_relative_input(void) {
    char out[GL_PATH_MAX];
    int rc = gl_canonicalize("relative/path.txt", out, sizeof(out));
    CHECK(rc == -1, "a non-absolute input must be rejected");
}

static void test_canonicalize_rejects_null(void) {
    char out[GL_PATH_MAX];
    CHECK(gl_canonicalize(NULL, out, sizeof(out)) == -1, "NULL input must fail closed");
}

/* End-to-end regression: the exact exploit shape from the issue, using
   the fixture in place of a real home directory. Confirms that after
   canonicalization, a traversal through a "temp-looking" prefix is
   classified by where it REALLY points, not the spoofed prefix. */
static void test_regression_traversal_defeats_temp_classification(void) {
    char spoofed[GL_PATH_MAX];
    snprintf(spoofed, sizeof(spoofed), "%s/sub/../target.txt", fixture_root);

    /* Pretend fixture_root/sub looks like a trusted temp dir. Before the
       fix, classifying the raw spoofed path against that "temp" prefix
       would incorrectly say "temp". */
    char fake_tmpdir[GL_PATH_MAX];
    snprintf(fake_tmpdir, sizeof(fake_tmpdir), "%s/sub", fixture_root);
    CHECK(gl_is_temp_path(spoofed, fake_tmpdir),
          "sanity: raw spoofed path does match the naive prefix check (this is the bug)");

    char canon[GL_PATH_MAX];
    int rc = gl_canonicalize(spoofed, canon, sizeof(canon));
    CHECK(rc == 0, "canonicalize should succeed");
    CHECK(!gl_is_temp_path(canon, fake_tmpdir),
          "after canonicalization the traversal must NOT be classified as temp");
}

int main(void) {
    fixture_setup();

    test_is_system_path();
    test_is_agent_internal();
    test_is_temp_path();
    test_is_dotfile();
    test_is_project_file();

    test_canonicalize_no_traversal();
    test_canonicalize_dotdot_traversal_existing_target();
    test_canonicalize_dotdot_traversal_deep();
    test_canonicalize_ocreat_nonexistent_leaf();
    test_canonicalize_ocreat_nested_nonexistent();
    test_canonicalize_symlink_confused_deputy();
    test_canonicalize_symlink_loop_fails_closed();
    test_canonicalize_rejects_relative_input();
    test_canonicalize_rejects_null();
    test_regression_traversal_defeats_temp_classification();

    if (failures) {
        fprintf(stderr, "\n%d/%d checks FAILED\n", failures, total);
        return 1;
    }
    printf("%d/%d checks passed\n", total, total);
    return 0;
}
