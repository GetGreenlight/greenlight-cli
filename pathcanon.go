package main

import (
	"os"
	"path/filepath"
	"strings"
)

// maxSeccompSymlinkFollows bounds symlink-chain resolution (matches Linux's
// own MAXSYMLINKS), so a symlink cycle fails closed instead of looping.
const maxSeccompSymlinkFollows = 40

// seccompCanonicalizeUnderRoot resolves ".." traversal and symlinks in the
// absolute path `p`, with every filesystem check and every symlink target
// scoped under `root` -- an absolute symlink target like "/etc/passwd" is
// interpreted as <root>/etc/passwd, NOT the caller's own real /etc/passwd.
// This is essential when root is /proc/<pid>/root for a process in a
// different mount namespace/chroot (issue #241): naively calling
// filepath.EvalSymlinks(filepath.Join(root, p)) resolves an absolute
// symlink target against the CALLER's real filesystem root, silently
// breaking out of the namespace scoping it was supposed to provide.
//
// Lives in this build-tag-free file (rather than seccomp_linux.go, its only
// caller) purely so it and its tests compile and run on every platform --
// the function itself has no /proc dependency, only its caller
// (seccomp_linux.go's seccompCanonicalize) does. This was previously
// entangled in a //go:build linux file where the new tests covering it
// silently never executed on the darwin dev machine that wrote them.
//
// Walks `p` component by component, resolving symlinks as it goes (rather
// than realpath()-then-rejoin) so a symlink can appear anywhere, including
// inside the non-existent tail for an O_CREAT target's parent directory.
// Once a component doesn't exist, nothing under it can be a symlink
// either, so the remaining components -- including any "."/".." among
// them -- are appended by the same per-component logic without further
// filesystem checks.
func seccompCanonicalizeUnderRoot(root, p string) (string, bool) {
	pending := splitAbsPath(p)
	var resolved []string
	inTail := false // true once we're past the longest existing ancestor
	follows := 0

	for len(pending) > 0 {
		part := pending[0]
		pending = pending[1:]

		switch part {
		case "", ".":
			continue
		case "..":
			if len(resolved) > 0 {
				resolved = resolved[:len(resolved)-1]
			}
			continue
		}

		if inTail {
			resolved = append(resolved, part)
			continue
		}

		candidate := "/" + strings.Join(append(append([]string{}, resolved...), part), "/")
		fi, err := os.Lstat(filepath.Join(root, candidate))
		if err != nil {
			if os.IsNotExist(err) {
				inTail = true
				resolved = append(resolved, part)
				continue
			}
			// EACCES, ENAMETOOLONG, a pid whose /proc/<pid>/root vanished
			// mid-check, etc -- fail closed rather than guess.
			return "", false
		}

		if fi.Mode()&os.ModeSymlink == 0 {
			resolved = append(resolved, part)
			continue
		}

		follows++
		if follows > maxSeccompSymlinkFollows {
			return "", false // symlink cycle -- fail closed, mirrors ELOOP
		}
		target, err := os.Readlink(filepath.Join(root, candidate))
		if err != nil {
			return "", false
		}
		if filepath.IsAbs(target) {
			resolved = nil // absolute target is rooted at `root`, not "/"
		}
		pending = append(splitAbsPath(target), pending...)
	}

	if len(resolved) == 0 {
		return "/", true
	}
	return "/" + strings.Join(resolved, "/"), true
}

// splitAbsPath splits a path into its non-empty components. Leading/
// trailing/duplicate slashes collapse to nothing, matching how the caller's
// per-component loop already treats "" and "." as no-ops -- so this needs
// no separate filepath.Clean pass.
func splitAbsPath(p string) []string {
	var parts []string
	for _, part := range strings.Split(p, "/") {
		if part != "" {
			parts = append(parts, part)
		}
	}
	return parts
}
