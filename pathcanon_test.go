package main

import (
	"os"
	"path/filepath"
	"testing"
)

// --- seccompCanonicalizeUnderRoot: mount-namespace/chroot scoping ---
//
// These tests use an ordinary temp dir as a stand-in for /proc/<pid>/root,
// since constructing a real mount namespace in a unit test is impractical.
// seccompCanonicalizeUnderRoot has no /proc dependency, so this file
// deliberately carries no //go:build tag -- these tests run (and are
// verified) on every platform, including the darwin dev machine.

// The bug this guards against: naively resolving a symlink via
// filepath.EvalSymlinks(filepath.Join(root, path)) works for a *relative*
// symlink target, but an *absolute* target (e.g. "/etc/passwd") would be
// re-interpreted against the real host filesystem instead of staying
// scoped under `root` -- silently breaking out of the intended namespace
// isolation. seccompCanonicalizeUnderRoot must treat an absolute symlink
// target as rooted at `root`, not at the caller's own "/".
func TestSeccompCanonicalizeUnderRoot_AbsoluteSymlinkStaysScopedToRoot(t *testing.T) {
	root := t.TempDir()

	realDir := filepath.Join(root, "real")
	if err := os.Mkdir(realDir, 0755); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(realDir, "target.txt")
	if err := os.WriteFile(target, []byte("secret"), 0644); err != nil {
		t.Fatal(err)
	}

	// A symlink whose target is an ABSOLUTE path *within the simulated
	// root's own namespace* -- "/real/target.txt", not root+"/real/...".
	link := filepath.Join(root, "link")
	if err := os.Symlink("/real/target.txt", link); err != nil {
		t.Fatal(err)
	}

	got, ok := seccompCanonicalizeUnderRoot(root, "/link")
	if !ok {
		t.Fatalf("seccompCanonicalizeUnderRoot(%q, /link) failed, want success", root)
	}
	want := "/real/target.txt"
	if got != want {
		t.Errorf("seccompCanonicalizeUnderRoot(%q, /link) = %q, want %q (scoped under root, not the host filesystem)", root, got, want)
	}
}

// A multi-hop chain mixing relative and absolute symlink targets must
// still resolve entirely within `root`.
func TestSeccompCanonicalizeUnderRoot_MixedSymlinkChain(t *testing.T) {
	root := t.TempDir()

	if err := os.MkdirAll(filepath.Join(root, "a", "b"), 0755); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(root, "a", "b", "target.txt")
	if err := os.WriteFile(target, []byte("x"), 0644); err != nil {
		t.Fatal(err)
	}

	// hop2 (relative) -> a/b/target.txt
	hop2 := filepath.Join(root, "a", "hop2")
	if err := os.Symlink("b/target.txt", hop2); err != nil {
		t.Fatal(err)
	}
	// hop1 (absolute, within root) -> /a/hop2
	hop1 := filepath.Join(root, "hop1")
	if err := os.Symlink("/a/hop2", hop1); err != nil {
		t.Fatal(err)
	}

	got, ok := seccompCanonicalizeUnderRoot(root, "/hop1")
	if !ok {
		t.Fatalf("seccompCanonicalizeUnderRoot(%q, /hop1) failed, want success", root)
	}
	if want := "/a/b/target.txt"; got != want {
		t.Errorf("seccompCanonicalizeUnderRoot(%q, /hop1) = %q, want %q", root, got, want)
	}
}

// O_CREAT semantics still hold when routed through the root-scoped resolver.
func TestSeccompCanonicalizeUnderRoot_NonexistentLeaf(t *testing.T) {
	root := t.TempDir()
	if err := os.Mkdir(filepath.Join(root, "sub"), 0755); err != nil {
		t.Fatal(err)
	}

	got, ok := seccompCanonicalizeUnderRoot(root, "/sub/../newfile.txt")
	if !ok {
		t.Fatalf("seccompCanonicalizeUnderRoot(%q) failed, want success", root)
	}
	if want := "/newfile.txt"; got != want {
		t.Errorf("seccompCanonicalizeUnderRoot(%q) = %q, want %q", root, got, want)
	}
}

func TestSeccompCanonicalizeUnderRoot_SymlinkLoopFailsClosed(t *testing.T) {
	root := t.TempDir()
	a := filepath.Join(root, "loop_a")
	b := filepath.Join(root, "loop_b")
	// Absolute-within-root targets, matching the shape that previously
	// escaped root scoping.
	if err := os.Symlink("/loop_b", a); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("/loop_a", b); err != nil {
		t.Fatal(err)
	}

	if _, ok := seccompCanonicalizeUnderRoot(root, "/loop_a"); ok {
		t.Error("seccompCanonicalizeUnderRoot(loop) succeeded, want fail-closed on symlink cycle")
	}
}
