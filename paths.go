//go:build darwin || linux

package main

import (
	"fmt"
	"os"
	"path/filepath"
)

// buildID isolates daemon state per server target. Empty = prod (default,
// preserves the existing ~/.greenlight + /tmp/greenlight-daemon.sock layout).
// Set to "dev" or "local" via -ldflags for non-prod builds so that a dev
// daemon does not collide with a prod daemon on the same host.
var buildID string

// greenlightDirName returns ".greenlight" for prod builds or
// ".greenlight-<buildID>" for dev/local builds.
func greenlightDirName() string {
	if buildID == "" {
		return ".greenlight"
	}
	return ".greenlight-" + buildID
}

// greenlightDir returns the absolute path to the per-build config dir.
func greenlightDir() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, greenlightDirName()), nil
}

// daemonSockName returns the daemon socket filename. Prod keeps the
// historical name; dev/local builds get a buildID-suffixed name so daemons
// targeting different servers can coexist on one host.
func daemonSockName() string {
	if buildID == "" {
		return "greenlight-daemon.sock"
	}
	return "greenlight-daemon-" + buildID + ".sock"
}

// setupCLIShim creates a per-session bin dir containing a `greenlight`
// symlink that points at the running binary. The caller prepends the
// returned dir to the child agent's PATH so that any `greenlight …`
// invocation by the agent — including subshells spawned via `bash -c` —
// resolves to *this* binary, not whatever else happens to sit on PATH.
//
// This keeps skill files and the system-prompt extension portable across
// prod/dev/local builds: their literal "greenlight" text works unchanged.
//
// Returns the directory path and a cleanup func. Both are empty/nil on
// failure (best-effort: a missing shim only degrades to the system
// `greenlight`, it doesn't break the session).
func setupCLIShim(relayID string) (string, func()) {
	exe, err := os.Executable()
	if err != nil {
		return "", nil
	}
	if resolved, err := filepath.EvalSymlinks(exe); err == nil {
		exe = resolved
	}
	dir := filepath.Join(os.TempDir(), fmt.Sprintf("greenlight-bin-%s", relayID))
	if err := os.MkdirAll(dir, 0755); err != nil {
		return "", nil
	}
	link := filepath.Join(dir, "greenlight")
	// Replace any stale link from a previous run with the same relay ID.
	_ = os.Remove(link)
	if err := os.Symlink(exe, link); err != nil {
		os.RemoveAll(dir)
		return "", nil
	}
	return dir, func() { os.RemoveAll(dir) }
}

// prependPATH returns a PATH value with prefix at the front. If existing is
// empty, returns prefix alone.
func prependPATH(prefix, existing string) string {
	if existing == "" {
		return prefix
	}
	return prefix + string(os.PathListSeparator) + existing
}
