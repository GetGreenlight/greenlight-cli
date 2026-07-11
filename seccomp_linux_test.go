//go:build linux

package main

import (
	"os"
	"path/filepath"
	"testing"
)

// The test process's own pid is a valid target for seccompCanonicalize:
// /proc/<pid>/root is a magic symlink to "/" for a non-chrooted process,
// exactly like the traced agent process in the non-namespaced case.
func selfPID(t *testing.T) uint32 {
	t.Helper()
	return uint32(os.Getpid())
}

func TestSeccompCanonicalize_NoTraversal(t *testing.T) {
	tmp := t.TempDir()
	real, err := filepath.EvalSymlinks(tmp)
	if err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(real, "target.txt")
	if err := os.WriteFile(target, []byte("x"), 0644); err != nil {
		t.Fatal(err)
	}

	got, ok := seccompCanonicalize(selfPID(t), target)
	if !ok {
		t.Fatalf("seccompCanonicalize(%q) failed, want success", target)
	}
	if got != target {
		t.Errorf("seccompCanonicalize(%q) = %q, want %q", target, got, target)
	}
}

// This is the read-exfiltration case from issue #241: a ".." traversal
// through a directory that looks like a trusted prefix, reaching an
// existing file outside it.
func TestSeccompCanonicalize_DotDotTraversalToExistingFile(t *testing.T) {
	tmp := t.TempDir()
	real, err := filepath.EvalSymlinks(tmp)
	if err != nil {
		t.Fatal(err)
	}
	sub := filepath.Join(real, "sub")
	if err := os.Mkdir(sub, 0755); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(real, "target.txt")
	if err := os.WriteFile(target, []byte("secret"), 0644); err != nil {
		t.Fatal(err)
	}

	spoofed := filepath.Join(sub, "..", "target.txt")
	got, ok := seccompCanonicalize(selfPID(t), spoofed)
	if !ok {
		t.Fatalf("seccompCanonicalize(%q) failed, want success", spoofed)
	}
	if got != target {
		t.Errorf("seccompCanonicalize(%q) = %q, want %q", spoofed, got, target)
	}
}

// The write/O_CREAT case: the leaf doesn't exist yet, only its parent does.
func TestSeccompCanonicalize_OCreatNonexistentLeaf(t *testing.T) {
	tmp := t.TempDir()
	real, err := filepath.EvalSymlinks(tmp)
	if err != nil {
		t.Fatal(err)
	}
	sub := filepath.Join(real, "sub")
	if err := os.Mkdir(sub, 0755); err != nil {
		t.Fatal(err)
	}

	spoofed := filepath.Join(sub, "..", "newfile.txt")
	want := filepath.Join(real, "newfile.txt")
	got, ok := seccompCanonicalize(selfPID(t), spoofed)
	if !ok {
		t.Fatalf("seccompCanonicalize(%q) failed, want success", spoofed)
	}
	if got != want {
		t.Errorf("seccompCanonicalize(%q) = %q, want %q", spoofed, got, want)
	}
}

func TestSeccompCanonicalize_NestedNonexistentTail(t *testing.T) {
	tmp := t.TempDir()
	real, err := filepath.EvalSymlinks(tmp)
	if err != nil {
		t.Fatal(err)
	}
	want := filepath.Join(real, "a", "b", "c.txt")
	got, ok := seccompCanonicalize(selfPID(t), want)
	if !ok {
		t.Fatalf("seccompCanonicalize(%q) failed, want success", want)
	}
	if got != want {
		t.Errorf("seccompCanonicalize(%q) = %q, want %q", want, got, want)
	}
}

// AC3: the symlink confused-deputy case — a symlink under a trusted-looking
// path pointing at a file outside it must classify against the resolved
// target, not the symlink's own path.
func TestSeccompCanonicalize_SymlinkConfusedDeputy(t *testing.T) {
	tmp := t.TempDir()
	real, err := filepath.EvalSymlinks(tmp)
	if err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(real, "target.txt")
	if err := os.WriteFile(target, []byte("secret"), 0644); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(real, "link_to_target")
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}

	got, ok := seccompCanonicalize(selfPID(t), link)
	if !ok {
		t.Fatalf("seccompCanonicalize(%q) failed, want success", link)
	}
	if got != target {
		t.Errorf("seccompCanonicalize(%q) = %q, want %q", link, got, target)
	}
}

func TestSeccompCanonicalize_SymlinkLoopFailsClosed(t *testing.T) {
	tmp := t.TempDir()
	real, err := filepath.EvalSymlinks(tmp)
	if err != nil {
		t.Fatal(err)
	}
	a := filepath.Join(real, "loop_a")
	b := filepath.Join(real, "loop_b")
	if err := os.Symlink(b, a); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(a, b); err != nil {
		t.Fatal(err)
	}

	if _, ok := seccompCanonicalize(selfPID(t), a); ok {
		t.Errorf("seccompCanonicalize(%q) succeeded, want fail-closed on symlink loop", a)
	}
}

func TestSeccompCanonicalize_RejectsRelativeAndEmpty(t *testing.T) {
	if _, ok := seccompCanonicalize(selfPID(t), "relative/path"); ok {
		t.Error("seccompCanonicalize(relative) succeeded, want failure")
	}
	if _, ok := seccompCanonicalize(selfPID(t), ""); ok {
		t.Error("seccompCanonicalize(\"\") succeeded, want failure")
	}
}

// End-to-end regression: this is the exact exploit shape from issue #241
// — "/tmp/../<real path>" matches the naive "/tmp/" prefix check before
// canonicalization, but must not after. Uses the real system /tmp and an
// existing file that ships on essentially every Linux system, since the
// bug is specifically about the literal "/tmp/" prefix seccompIsTempPath
// checks for.
func TestSeccompCanonicalize_DefeatsTempPathSpoof(t *testing.T) {
	const realTarget = "/etc/hostname"
	if _, err := os.Stat(realTarget); err != nil {
		t.Skipf("skipping: %s not present on this system", realTarget)
	}

	spoofed := "/tmp/../etc/hostname"

	// Sanity: the raw path (pre-canonicalization) does spoof the naive
	// prefix classifier — this is the bug.
	if !seccompIsTempPath(spoofed) {
		t.Fatalf("sanity check failed: expected raw spoofed path %q to match the naive temp-prefix check", spoofed)
	}

	canon, ok := seccompCanonicalize(selfPID(t), spoofed)
	if !ok {
		t.Fatalf("seccompCanonicalize(%q) failed, want success", spoofed)
	}
	if canon != realTarget {
		t.Errorf("seccompCanonicalize(%q) = %q, want %q", spoofed, canon, realTarget)
	}
	if seccompIsTempPath(canon) {
		t.Errorf("canonicalized path %q still classified as temp — traversal defeated the fix", canon)
	}
}

func TestSeccompIsTempPath(t *testing.T) {
	if !seccompIsTempPath("/tmp/foo.txt") {
		t.Error("/tmp/foo.txt should be temp")
	}
	if !seccompIsTempPath("/private/tmp/foo.txt") {
		t.Error("/private/tmp/foo.txt should be temp")
	}
	if seccompIsTempPath("/Users/me/notes.txt") {
		t.Error("home dir path should not be temp")
	}
}

func TestSeccompIsProjectFile_BoundaryCheck(t *testing.T) {
	orig := seccompProjectDir
	defer func() { seccompProjectDir = orig }()

	seccompProjectDir = "/Users/me/proj"
	if !seccompIsProjectFile("/Users/me/proj/file.txt") {
		t.Error("file inside project dir should match")
	}
	if seccompIsProjectFile("/Users/me/proj2/file.txt") {
		t.Error("sibling dir with project dir as a string prefix must not match (boundary check)")
	}
}

// seccompCanonicalizeUnderRoot's own tests (including the mount-namespace/
// chroot absolute-symlink-scoping case) live in pathcanon_test.go, a
// build-tag-free file, so they actually execute on darwin as well as
// linux.
