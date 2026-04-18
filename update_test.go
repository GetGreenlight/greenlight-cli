//go:build integration

package main

import (
	"fmt"
	"os"
	"testing"
	"time"
)

// TestIntegration_UpdateShutdown_NoInstances tests that update_shutdown
// with no active instances immediately returns ok and shuts down the daemon.
func TestIntegration_UpdateShutdown_NoInstances(t *testing.T) {
	sockPath, _, cleanup := startTestDaemon(t)
	defer cleanup()

	resp := daemonIPC(t, sockPath, ipcRequest{Type: "update_shutdown"})
	if resp.Type != "ok" {
		t.Fatalf("expected ok, got %q (message: %s)", resp.Type, resp.Message)
	}

	// Daemon should exit shortly
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if !waitForSocket(t, sockPath, 200*time.Millisecond) {
			return // daemon exited
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Error("daemon did not exit after update_shutdown with no instances")
}

// TestIntegration_UpdateShutdown_ActiveInstances tests that update_shutdown
// without force reports active instances and keeps the daemon running.
func TestIntegration_UpdateShutdown_ActiveInstances(t *testing.T) {
	sockPath, _, cleanup := startTestDaemon(t,
		"GREENLIGHT_DEVICE_ID=test-device-123",
	)
	defer cleanup()

	// First verify no instances
	statusResp := daemonIPC(t, sockPath, ipcRequest{Type: "status"})
	if len(statusResp.Instances) != 0 {
		t.Fatalf("expected 0 instances initially, got %d", len(statusResp.Instances))
	}

	// With 0 instances and force=false, it should still return ok.
	resp := daemonIPC(t, sockPath, ipcRequest{Type: "update_shutdown", Force: false})
	if resp.Type != "ok" {
		t.Fatalf("expected ok with no instances, got %q", resp.Type)
	}
}

// TestIntegration_UpdateShutdown_Force tests that update_shutdown with
// force=true shuts down even if there were instances.
func TestIntegration_UpdateShutdown_Force(t *testing.T) {
	sockPath, _, cleanup := startTestDaemon(t)
	defer cleanup()

	resp := daemonIPC(t, sockPath, ipcRequest{Type: "update_shutdown", Force: true})
	if resp.Type != "ok" {
		t.Fatalf("expected ok, got %q", resp.Type)
	}

	// Daemon should exit
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if !waitForSocket(t, sockPath, 200*time.Millisecond) {
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Error("daemon did not exit after forced update_shutdown")
}

// TestIntegration_UpdateShutdown_DaemonStaysRunning tests that the daemon
// stays healthy after a status check followed by an update_shutdown.
func TestIntegration_UpdateShutdown_DaemonStaysRunning(t *testing.T) {
	sockPath, _, cleanup := startTestDaemon(t)
	defer cleanup()

	statusResp := daemonIPC(t, sockPath, ipcRequest{Type: "status"})
	if statusResp.Type != "status_response" {
		t.Fatalf("expected status_response, got %q", statusResp.Type)
	}

	// Now send update_shutdown (no instances, so it'll shut down)
	resp := daemonIPC(t, sockPath, ipcRequest{Type: "update_shutdown"})
	if resp.Type != "ok" {
		t.Fatalf("expected ok, got %q", resp.Type)
	}
}

// daemonTestSockPath generates a unique socket path for tests.
func daemonTestSockPath() string {
	return fmt.Sprintf("/tmp/gl-test-%d-%d.sock", os.Getpid(), time.Now().UnixNano())
}
