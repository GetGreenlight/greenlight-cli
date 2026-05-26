//go:build darwin || linux

package main

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"
)

// TestHasOtherSessions_SiblingsInSameDaemon exercises the daemon-mode
// regression where every session in one daemon shares the daemon's PID.
// The old implementation skipped any PID file whose pid matched the current
// process, so it could not see sibling sessions and let skill cleanup wipe
// state that the surviving session still needed (issue #25).
func TestHasOtherSessions_SiblingsInSameDaemon(t *testing.T) {
	t.Setenv("TMPDIR", t.TempDir())
	tmp := os.TempDir()

	agent := "claude"
	cwd := tmp

	// Simulate a sibling session whose PID file records *our* PID — the
	// daemon shares one PID across every session it owns. The caller's own
	// PID file has already been removed by cleanup() before this runs.
	siblingPid := os.Getpid()
	siblingFile := filepath.Join(tmp, "greenlight-connect-sibling-relay.pid")
	if err := os.WriteFile(siblingFile, []byte(fmt.Sprintf("%d %s %s", siblingPid, agent, cwd)), 0644); err != nil {
		t.Fatalf("write sibling pid file: %v", err)
	}

	if !hasOtherSessions(agent, cwd) {
		t.Fatalf("hasOtherSessions should detect a sibling session sharing this daemon's PID")
	}

	// Mismatched cwd must not match.
	if hasOtherSessions(agent, filepath.Join(tmp, "different")) {
		t.Errorf("hasOtherSessions should ignore sessions in unrelated cwd")
	}

	// A stale PID file pointing at a long-dead PID should be cleaned up
	// and not count as a live sibling.
	stale := filepath.Join(tmp, "greenlight-connect-stale-relay.pid")
	if err := os.WriteFile(stale, []byte(fmt.Sprintf("%d %s %s", 0x7fffffff, agent, cwd)), 0644); err != nil {
		t.Fatalf("write stale pid file: %v", err)
	}
	// Remove the real sibling so only the stale file remains.
	os.Remove(siblingFile)
	if hasOtherSessions(agent, cwd) {
		t.Errorf("stale PID file should not count as live sibling")
	}
	if _, err := os.Stat(stale); !os.IsNotExist(err) {
		t.Errorf("stale PID file should have been removed, got err=%v", err)
	}
}
