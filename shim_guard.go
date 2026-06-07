//go:build darwin || linux

package main

import (
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// The shim re-exec guard prevents a second permission prompt when a command
// shim execs the real binary. When the daemon approves a rewritten shim command
// (e.g. it showed `greenlight run … -- gh issue list` on the phone), it records
// the inner command the shim will re-exec. The shim then runs the real
// `/abs/path/gh issue list`; if that exec is itself intercepted (unsigned dev
// builds on macOS, where the interpose lib loads into the greenlight binary),
// the handler auto-allows it once instead of prompting again.
//
// This is safe against a hostile agent: an entry exists only *after* the
// command was approved through normal gating, so the agent can't pre-create one
// to bypass a prompt. Consumption is single-use, TTL-bounded, and only fires
// for an absolute-path re-exec — never for the agent-facing bare command, which
// is always gated normally.
var (
	shimGuardMu sync.Mutex
	shimGuard   = map[string]time.Time{} // re-exec key → expiry
)

// shimGuardTTL bounds how long a recorded re-exec stays auto-allowable. The
// shim re-execs within milliseconds of approval; this is generous slack.
const shimGuardTTL = 10 * time.Second

// normalizeShimKey reduces a command to "<basename> <args…>" with collapsed
// whitespace, so the recorded form ("gh issue list") matches the shim's
// absolute-path re-exec ("/opt/homebrew/bin/gh issue list").
func normalizeShimKey(cmd string) string {
	fields := strings.Fields(cmd)
	if len(fields) == 0 {
		return ""
	}
	fields[0] = filepath.Base(fields[0])
	return strings.Join(fields, " ")
}

// rememberShimReexec records the re-exec keys of a just-approved shim command,
// sweeping any expired entries first.
func rememberShimReexec(keys []string) {
	if len(keys) == 0 {
		return
	}
	now := time.Now()
	exp := now.Add(shimGuardTTL)
	shimGuardMu.Lock()
	defer shimGuardMu.Unlock()
	for k, e := range shimGuard {
		if e.Before(now) {
			delete(shimGuard, k)
		}
	}
	for _, k := range keys {
		if k != "" {
			shimGuard[k] = exp
		}
	}
}

// consumeShimReexec reports whether cmd is a shim's re-exec of a real binary
// that was just approved, consuming the entry. Only an absolute-path leading
// token qualifies, so the agent-facing bare command is never auto-allowed here.
func consumeShimReexec(cmd string) bool {
	fields := strings.Fields(cmd)
	if len(fields) == 0 || !strings.Contains(fields[0], "/") {
		return false
	}
	key := normalizeShimKey(cmd)
	if key == "" {
		return false
	}
	shimGuardMu.Lock()
	defer shimGuardMu.Unlock()
	exp, ok := shimGuard[key]
	if !ok {
		return false
	}
	delete(shimGuard, key)
	return time.Now().Before(exp)
}
