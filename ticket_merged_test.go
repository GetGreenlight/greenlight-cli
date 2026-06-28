//go:build darwin || linux

package main

import (
	"testing"
	"time"
)

// TestSignalTicketMerged_NoID verifies the merge signal is a no-op without a
// resolved ticket id (e.g. a `--pr`-only merge): it must return immediately and
// never dial the daemon, even with a bogus socket path.
func TestSignalTicketMerged_NoID(t *testing.T) {
	t.Setenv("GREENLIGHT_DAEMON_SOCK", "/nonexistent/greenlight-merged-test.sock")

	done := make(chan struct{})
	go func() {
		signalTicketMerged("proj", "/tmp", "")
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("signalTicketMerged blocked with no ticket id")
	}
}

// TestSignalTicketMerged_UnresolvableTarget verifies the signal is non-fatal when
// the tag target can't be resolved (cwd empty → no_repo): it must return without
// dialing rather than fail the (already-completed) merge.
func TestSignalTicketMerged_UnresolvableTarget(t *testing.T) {
	t.Setenv("GREENLIGHT_DAEMON_SOCK", "/nonexistent/greenlight-merged-test.sock")

	done := make(chan struct{})
	go func() {
		signalTicketMerged("proj", "", "42") // empty cwd → resolveTagTarget returns no_repo
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("signalTicketMerged blocked on an unresolvable target")
	}
}
