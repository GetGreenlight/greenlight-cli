//go:build darwin || linux

package main

import (
	"testing"
	"time"
)

// TestMarkSessionAwaitingUser_NoSession verifies the handoff signal is a no-op
// outside a session: with GREENLIGHT_SESSION_ID unset it must not dial the daemon
// at all (so it returns immediately even when the socket path is bogus).
func TestMarkSessionAwaitingUser_NoSession(t *testing.T) {
	t.Setenv("GREENLIGHT_SESSION_ID", "")
	t.Setenv("GREENLIGHT_DAEMON_SOCK", "/nonexistent/greenlight-await-test.sock")

	done := make(chan struct{})
	go func() {
		markSessionAwaitingUser()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("markSessionAwaitingUser blocked when no session was set")
	}
}

// TestMarkSessionAwaitingUser_DaemonDown verifies the signal is non-fatal when
// the daemon is unreachable: the stage move already happened, so a dial failure
// must be swallowed (logged), never panic or exit.
func TestMarkSessionAwaitingUser_DaemonDown(t *testing.T) {
	t.Setenv("GREENLIGHT_SESSION_ID", "relay-xyz")
	t.Setenv("GREENLIGHT_DAEMON_SOCK", "/nonexistent/greenlight-await-test.sock")

	done := make(chan struct{})
	go func() {
		markSessionAwaitingUser() // must return rather than os.Exit / panic
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(6 * time.Second):
		t.Fatal("markSessionAwaitingUser blocked with the daemon down")
	}
}
