//go:build darwin || linux

package main

import (
	"bytes"
	"log"
	"strings"
	"testing"
)

// captureLog redirects the standard logger to a buffer for the duration of fn
// and returns everything written to it. Restores log's prior output after.
func captureLog(t *testing.T, fn func()) string {
	t.Helper()
	var buf bytes.Buffer
	prev := log.Writer()
	log.SetOutput(&buf)
	defer log.SetOutput(prev)
	fn()
	return buf.String()
}

// TestRunHookCore_NoSockLogsReason covers issue #294's "no socket" bail path:
// with GREENLIGHT_DEVICE_ID set but GREENLIGHT_INTERPOSE_SOCK absent, runHookCore
// must log a reason instead of exiting silently.
func TestRunHookCore_NoSockLogsReason(t *testing.T) {
	t.Setenv("GREENLIGHT_DEVICE_ID", "test-device")
	t.Setenv("GREENLIGHT_INTERPOSE_SOCK", "")

	out := captureLog(t, func() {
		runHookCore(strings.NewReader(`{"tool_name":"AskUserQuestion","tool_input":{}}`), &bytes.Buffer{})
	})

	if !strings.Contains(out, "reason=no_sock_env") {
		t.Errorf("expected log to contain reason=no_sock_env, got: %q", out)
	}
}

// TestRunHookCore_DecodeFailedLogsReason covers the "decode failed" bail path:
// malformed stdin JSON must log a reason rather than exiting silently.
func TestRunHookCore_DecodeFailedLogsReason(t *testing.T) {
	t.Setenv("GREENLIGHT_DEVICE_ID", "test-device")
	t.Setenv("GREENLIGHT_INTERPOSE_SOCK", "/tmp/does-not-matter.sock")

	out := captureLog(t, func() {
		runHookCore(strings.NewReader("not json"), &bytes.Buffer{})
	})

	if !strings.Contains(out, "reason=decode_stdin_failed") {
		t.Errorf("expected log to contain reason=decode_stdin_failed, got: %q", out)
	}
}

// TestRunHookCore_NoDeviceIDLogsReason covers the "no device id" bail path.
func TestRunHookCore_NoDeviceIDLogsReason(t *testing.T) {
	t.Setenv("GREENLIGHT_DEVICE_ID", "")

	out := captureLog(t, func() {
		runHookCore(strings.NewReader(`{"tool_name":"AskUserQuestion","tool_input":{}}`), &bytes.Buffer{})
	})

	if !strings.Contains(out, "reason=no_device_id") {
		t.Errorf("expected log to contain reason=no_device_id, got: %q", out)
	}
}

// TestRunHookCore_DialFailedLogsReason covers the "dial failed" bail path: a
// socket path that exists as a string but has nothing listening.
func TestRunHookCore_DialFailedLogsReason(t *testing.T) {
	t.Setenv("GREENLIGHT_DEVICE_ID", "test-device")
	t.Setenv("GREENLIGHT_INTERPOSE_SOCK", "/tmp/greenlight-hook-test-nonexistent.sock")

	out := captureLog(t, func() {
		runHookCore(strings.NewReader(`{"tool_name":"AskUserQuestion","tool_input":{}}`), &bytes.Buffer{})
	})

	if !strings.Contains(out, "reason=dial_failed") {
		t.Errorf("expected log to contain reason=dial_failed, got: %q", out)
	}
}

// TestBuildExportEnvs_GreenlightLogMatchesDaemonLogPath pins issue #294's
// acceptance criterion 1: the GREENLIGHT_LOG value threaded to the agent's
// (and from there the hook subprocess's) env must be the exact same path
// runDaemonForeground redirects its own logging to — not an independent
// recomputation of main()'s pre-override default.
func TestBuildExportEnvs_GreenlightLogMatchesDaemonLogPath(t *testing.T) {
	prev := daemonLogPath
	defer func() { daemonLogPath = prev }()

	daemonLogPath = "/tmp/fake-greenlight-home/daemon.log"
	envs := buildExportEnvs("dev-id", "relay-id", "proj", "/bridge", "claude", "")

	if got := envs["GREENLIGHT_LOG"]; got != daemonLogPath {
		t.Errorf("GREENLIGHT_LOG = %q, want %q (daemonLogPath)", got, daemonLogPath)
	}
}
