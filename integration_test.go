//go:build integration

package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"greenlight/internal/mockserver"
)

// Paths set by TestMain
var (
	greenlightBin string // path to compiled greenlight binary
	mockClaudeBin string // path to mock claude binary
)

// The mock server lives in internal/mockserver so the dev binary can
// reuse it. Tests get the same API via short aliases below.

// ---------- helpers ----------

type runResult struct {
	Stdout   string
	Stderr   string
	ExitCode int
}

func run(t *testing.T, args []string, env []string, stdin string) runResult {
	t.Helper()
	return runWithTimeout(t, args, env, stdin, 10*time.Second)
}

func runWithTimeout(t *testing.T, args []string, env []string, stdin string, timeout time.Duration) runResult {
	t.Helper()
	cmd := exec.Command(greenlightBin, args...)

	// Start with a clean env, then add what the test needs
	baseEnv := []string{
		"HOME=" + os.Getenv("HOME"),
		"PATH=" + os.Getenv("PATH"),
		"TMPDIR=" + os.Getenv("TMPDIR"),
		"TERM=xterm-256color",
	}
	cmd.Env = append(baseEnv, env...)

	if stdin != "" {
		cmd.Stdin = strings.NewReader(stdin)
	}

	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	// Run with timeout
	done := make(chan error, 1)
	if err := cmd.Start(); err != nil {
		t.Fatalf("failed to start greenlight: %v", err)
	}
	go func() { done <- cmd.Wait() }()

	select {
	case err := <-done:
		code := 0
		if err != nil {
			if exitErr, ok := err.(*exec.ExitError); ok {
				code = exitErr.ExitCode()
			} else {
				t.Fatalf("unexpected error: %v", err)
			}
		}
		return runResult{
			Stdout:   stdout.String(),
			Stderr:   stderr.String(),
			ExitCode: code,
		}
	case <-time.After(timeout):
		cmd.Process.Kill()
		t.Fatalf("command timed out after %v; stdout=%q stderr=%q", timeout, stdout.String(), stderr.String())
		return runResult{}
	}
}

// ---------- TestMain ----------

func TestMain(m *testing.M) {
	tmpDir, err := os.MkdirTemp("", "greenlight-integration-*")
	if err != nil {
		fmt.Fprintf(os.Stderr, "failed to create temp dir: %v\n", err)
		os.Exit(1)
	}
	defer os.RemoveAll(tmpDir)

	// Start test server to get the address for the build
	ts := mockserver.New()
	defer ts.Close()
	testServerURL = ts

	// Build greenlight binary with the test server URL and version
	greenlightBin = filepath.Join(tmpDir, "greenlight")
	buildCmd := exec.Command("go", "build",
		"-ldflags", "-X main.wsURL="+ts.WSURL()+" -X main.version=0.0.0-test",
		"-o", greenlightBin,
		".",
	)
	buildEnv := []string{"CGO_ENABLED=0"}
	if runtime.GOOS == "darwin" {
		buildEnv = append(buildEnv, "MACOSX_DEPLOYMENT_TARGET=13.0")
	}
	buildCmd.Env = append(os.Environ(), buildEnv...)
	buildCmd.Dir = sourceDir()
	if out, err := buildCmd.CombinedOutput(); err != nil {
		fmt.Fprintf(os.Stderr, "failed to build greenlight:\n%s\n%v\n", out, err)
		os.Exit(1)
	}

	// Build mock claude binary. Default CGO_ENABLED (typically 1) is
	// required on Linux: the seccomp permission test relies on the
	// interpose .so loading via LD_PRELOAD, which only works for
	// dynamically-linked binaries.
	mockClaudeBin = filepath.Join(tmpDir, "claude")
	mockCmd := exec.Command("go", "build", "-o", mockClaudeBin, "./testdata/mock_claude.go")
	mockCmd.Env = os.Environ()
	mockCmd.Dir = sourceDir()
	if out, err := mockCmd.CombinedOutput(); err != nil {
		fmt.Fprintf(os.Stderr, "failed to build mock claude:\n%s\n%v\n", out, err)
		os.Exit(1)
	}

	os.Exit(m.Run())
}

// testServerURL is shared across tests
var testServerURL *mockserver.Server

func sourceDir() string {
	// This test file lives in the repo root
	dir, _ := os.Getwd()
	return dir
}

// ---------- CLI basics ----------

func TestIntegration_UnknownSubcommand(t *testing.T) {
	r := run(t, []string{"bogus"}, nil, "")
	if r.ExitCode == 0 {
		t.Error("expected non-zero exit code")
	}
	if !strings.Contains(r.Stderr, "unknown command") {
		t.Errorf("expected 'unknown command', got stderr=%q", r.Stderr)
	}
}

func TestIntegration_Help(t *testing.T) {
	for _, arg := range []string{"help", "--help", "-h"} {
		t.Run(arg, func(t *testing.T) {
			r := run(t, []string{arg}, nil, "")
			if r.ExitCode != 0 {
				t.Errorf("expected exit 0, got %d; stderr=%q", r.ExitCode, r.Stderr)
			}
			if !strings.Contains(r.Stderr, "Usage:") {
				t.Errorf("expected usage text, got stderr=%q", r.Stderr)
			}
			if !strings.Contains(r.Stderr, "0.0.0-test") {
				t.Errorf("expected version in usage text, got stderr=%q", r.Stderr)
			}
		})
	}
}

func TestIntegration_Version(t *testing.T) {
	for _, arg := range []string{"version", "--version", "-v"} {
		t.Run(arg, func(t *testing.T) {
			r := run(t, []string{arg}, nil, "")
			if r.ExitCode != 0 {
				t.Errorf("expected exit 0, got %d; stderr=%q", r.ExitCode, r.Stderr)
			}
			if !strings.Contains(r.Stderr, "greenlight 0.0.0-test") {
				t.Errorf("expected 'greenlight 0.0.0-test', got stderr=%q", r.Stderr)
			}
		})
	}
}

// ---------- register ----------

func TestIntegration_Register_Success(t *testing.T) {
	home := t.TempDir()
	id := "deadbeef-1234-5678-9abc-def012345678"
	r := run(t, []string{"register", id}, []string{"HOME=" + home}, "")
	if r.ExitCode != 0 {
		t.Fatalf("expected exit 0, got %d; stderr=%q", r.ExitCode, r.Stderr)
	}
	if !strings.Contains(r.Stderr, "Registered device "+id) {
		t.Errorf("expected confirmation, got stderr=%q", r.Stderr)
	}
	cfg, err := os.ReadFile(filepath.Join(home, ".greenlight", "config"))
	if err != nil {
		t.Fatalf("config file: %v", err)
	}
	if !strings.Contains(string(cfg), "device_id="+id) {
		t.Errorf("config missing device_id: %q", string(cfg))
	}
}

func TestIntegration_Register_InvalidUUID(t *testing.T) {
	home := t.TempDir()
	r := run(t, []string{"register", "not-a-uuid"}, []string{"HOME=" + home}, "")
	if r.ExitCode == 0 {
		t.Error("expected non-zero exit for invalid UUID")
	}
	if !strings.Contains(r.Stderr, "invalid device ID") {
		t.Errorf("expected 'invalid device ID', got stderr=%q", r.Stderr)
	}
}

func TestIntegration_Register_NoArgs(t *testing.T) {
	home := t.TempDir()
	r := run(t, []string{"register"}, []string{"HOME=" + home}, "")
	if r.ExitCode == 0 {
		t.Error("expected non-zero exit when no device ID")
	}
	if !strings.Contains(r.Stderr, "Usage:") {
		t.Errorf("expected usage text, got stderr=%q", r.Stderr)
	}
}

// ---------- agent ----------

func TestIntegration_Agent_DefaultClaude(t *testing.T) {
	home := t.TempDir()
	r := run(t, []string{"agent"}, []string{"HOME=" + home}, "")
	if r.ExitCode != 0 {
		t.Fatalf("expected exit 0, got %d; stderr=%q", r.ExitCode, r.Stderr)
	}
	if !strings.Contains(r.Stderr, "claude") {
		t.Errorf("expected default 'claude', got stderr=%q", r.Stderr)
	}
}

func TestIntegration_Agent_Set(t *testing.T) {
	home := t.TempDir()
	r := run(t, []string{"agent", "codex"}, []string{"HOME=" + home}, "")
	if r.ExitCode != 0 {
		t.Fatalf("expected exit 0, got %d; stderr=%q", r.ExitCode, r.Stderr)
	}
	if !strings.Contains(r.Stderr, "Default agent set to codex") {
		t.Errorf("expected confirmation, got stderr=%q", r.Stderr)
	}
	// Subsequent get should reflect the new default.
	r2 := run(t, []string{"agent"}, []string{"HOME=" + home}, "")
	if !strings.Contains(r2.Stderr, "codex") {
		t.Errorf("expected 'codex' after set, got stderr=%q", r2.Stderr)
	}
}

func TestIntegration_Agent_Unknown(t *testing.T) {
	home := t.TempDir()
	r := run(t, []string{"agent", "bogus"}, []string{"HOME=" + home}, "")
	if r.ExitCode == 0 {
		t.Error("expected non-zero exit for unknown agent")
	}
	if !strings.Contains(r.Stderr, "unknown agent") {
		t.Errorf("expected 'unknown agent', got stderr=%q", r.Stderr)
	}
}

// ---------- run ----------

func TestIntegration_Run_NoArgs(t *testing.T) {
	r := run(t, []string{"run"}, []string{"HOME=" + t.TempDir()}, "")
	if r.ExitCode == 0 {
		t.Error("expected non-zero exit when no command")
	}
	if !strings.Contains(r.Stderr, "Usage:") {
		t.Errorf("expected usage text, got stderr=%q", r.Stderr)
	}
}

func TestIntegration_Run_UnknownFlag(t *testing.T) {
	r := run(t, []string{"run", "--bogus", "echo"}, []string{"HOME=" + t.TempDir()}, "")
	if r.ExitCode == 0 {
		t.Error("expected non-zero exit for unknown flag")
	}
	if !strings.Contains(r.Stderr, "unknown flag") {
		t.Errorf("expected 'unknown flag', got stderr=%q", r.Stderr)
	}
}

func TestIntegration_Run_MissingEArg(t *testing.T) {
	r := run(t, []string{"run", "-e"}, []string{"HOME=" + t.TempDir()}, "")
	if r.ExitCode == 0 {
		t.Error("expected non-zero exit when -e has no value")
	}
	if !strings.Contains(r.Stderr, "-e requires an argument") {
		t.Errorf("expected '-e requires an argument', got stderr=%q", r.Stderr)
	}
}

func TestIntegration_Run_MissingCommand(t *testing.T) {
	r := run(t, []string{"run", "--"}, []string{"HOME=" + t.TempDir()}, "")
	if r.ExitCode == 0 {
		t.Error("expected non-zero exit when no command after --")
	}
	if !strings.Contains(r.Stderr, "missing command") {
		t.Errorf("expected 'missing command', got stderr=%q", r.Stderr)
	}
}

func TestIntegration_Run_RefusesPATHInjection(t *testing.T) {
	r := run(t, []string{"run", "-e", "PATH=SOMEKEY", "--", "echo"}, []string{
		"HOME=" + t.TempDir(),
		"GREENLIGHT_DAEMON_SOCK=/tmp/gl-test-no-daemon.sock",
	}, "")
	if r.ExitCode == 0 {
		t.Error("expected non-zero exit when injecting PATH")
	}
	if !strings.Contains(r.Stderr, "PATH") {
		t.Errorf("expected refusal mentioning PATH, got stderr=%q", r.Stderr)
	}
}

// ---------- update ----------

func TestIntegration_Update_UnexpectedArg(t *testing.T) {
	r := run(t, []string{"update", "extra"}, []string{"HOME=" + t.TempDir()}, "")
	if r.ExitCode == 0 {
		t.Error("expected non-zero exit for unexpected positional arg")
	}
	if !strings.Contains(r.Stderr, "unexpected argument") {
		t.Errorf("expected 'unexpected argument', got stderr=%q", r.Stderr)
	}
}

// ---------- daemon IPC: device_info ----------

func TestIntegration_Daemon_DeviceInfo(t *testing.T) {
	sockPath, _, cleanup := startTestDaemon(t,
		"GREENLIGHT_DEVICE_ID=device-info-probe",
	)
	defer cleanup()

	resp := daemonIPC(t, sockPath, ipcRequest{Type: "device_info"})
	if resp.Type != "device_info_response" {
		t.Errorf("expected device_info_response, got %q (msg=%s)", resp.Type, resp.Message)
	}
	if resp.DeviceID != "device-info-probe" {
		t.Errorf("expected device id 'device-info-probe', got %q", resp.DeviceID)
	}
}

// ---------- daemon IPC: ws_request error paths ----------

func TestIntegration_Daemon_WSRequest_MissingFields(t *testing.T) {
	testServerURL.ClearHandlers()
	hostID := enrollTestHost(t, "ws-request-probe")
	sockPath, _, cleanup := startTestDaemon(t,
		"GREENLIGHT_DEVICE_ID=ws-request-probe",
		"GREENLIGHT_DAEMON_SESSION_ID="+hostID,
	)
	defer cleanup()

	// ws_type and ws_request_id are both required. With a connected WS
	// and neither field set, the validation error fires.
	resp := daemonIPC(t, sockPath, ipcRequest{Type: "ws_request"})
	if resp.Type != "error" {
		t.Errorf("expected error response, got %q", resp.Type)
	}
	if !strings.Contains(resp.Message, "ws_type") {
		t.Errorf("expected error mentioning ws_type, got %q", resp.Message)
	}
}

func TestIntegration_Daemon_WSRequest_NoWebSocket(t *testing.T) {
	// With no enrolled session, the daemon's WS is disabled — ws_request
	// should surface the connectivity error rather than try to send.
	sockPath, _, cleanup := startTestDaemon(t)
	defer cleanup()

	resp := daemonIPC(t, sockPath, ipcRequest{
		Type:    "ws_request",
		WSType:  "probe",
		WSReqID: "rid",
	})
	if resp.Type != "error" {
		t.Errorf("expected error, got %q", resp.Type)
	}
	if !strings.Contains(resp.Message, "WebSocket") {
		t.Errorf("expected WebSocket error, got %q", resp.Message)
	}
}

// ---------- daemon IPC: ws_request happy path ----------

func TestIntegration_Daemon_WSRequest_RoundTrip(t *testing.T) {
	testServerURL.ClearHandlers()

	// Server-side responder: the daemon's SendRequest wraps the payload
	// in {"type":<wsType>,"relay_id":"","data":<payload>} where the
	// caller's request_id lives inside `data`. Pull it out and echo a
	// response keyed by that request_id at the top level so the
	// daemon's routePermissionResponse can dispatch it.
	respondedFor := make(chan string, 1)
	testServerURL.SetFrameHook(func(s *mockserver.Session, frame json.RawMessage) {
		var msg struct {
			Type string `json:"type"`
			Data struct {
				RequestID string `json:"request_id"`
			} `json:"data"`
		}
		if json.Unmarshal(frame, &msg) != nil || msg.Type != "probe" {
			return
		}
		if msg.Data.RequestID == "" {
			return
		}
		s.Send(map[string]any{
			"type":       "probe_response",
			"request_id": msg.Data.RequestID,
			"echo":       "hello-back",
		})
		select {
		case respondedFor <- msg.Data.RequestID:
		default:
		}
	})

	hostID := enrollTestHost(t, "ws-rt-dev")
	sockPath, _, cleanup := startTestDaemon(t,
		"GREENLIGHT_DEVICE_ID=ws-rt-dev",
		"GREENLIGHT_DAEMON_SESSION_ID="+hostID,
	)
	defer cleanup()

	reqID := "test-req-" + fmt.Sprint(time.Now().UnixNano())
	body, _ := json.Marshal(map[string]any{"request_id": reqID, "any": "payload"})
	resp := daemonIPC(t, sockPath, ipcRequest{
		Type:      "ws_request",
		WSType:    "probe",
		WSReqID:   reqID,
		WSData:    body,
		TimeoutMS: 5000,
	})
	if resp.Type != "ws_response" {
		t.Fatalf("expected ws_response, got %q (msg=%s)", resp.Type, resp.Message)
	}
	var payload struct {
		Type string `json:"type"`
		Echo string `json:"echo"`
	}
	if err := json.Unmarshal(resp.WSResponse, &payload); err != nil {
		t.Fatalf("parse ws response: %v (raw=%s)", err, string(resp.WSResponse))
	}
	if payload.Type != "probe_response" || payload.Echo != "hello-back" {
		t.Errorf("unexpected payload: %+v", payload)
	}

	select {
	case got := <-respondedFor:
		if got != reqID {
			t.Errorf("server saw request_id %q, want %q", got, reqID)
		}
	case <-time.After(2 * time.Second):
		t.Error("server-side hook never fired — daemon didn't relay the message")
	}
}

// ---------- secrets ----------

func TestIntegration_Secrets_NoArgs(t *testing.T) {
	r := run(t, []string{"secrets"}, []string{"HOME=" + t.TempDir()}, "")
	if r.ExitCode == 0 {
		t.Error("expected non-zero exit when no subcommand")
	}
	if !strings.Contains(r.Stderr, "Usage:") {
		t.Errorf("expected usage text, got stderr=%q", r.Stderr)
	}
}

func TestIntegration_Secrets_UnknownSubcommand(t *testing.T) {
	r := run(t, []string{"secrets", "bogus"}, []string{"HOME=" + t.TempDir()}, "")
	if r.ExitCode == 0 {
		t.Error("expected non-zero exit for unknown secrets subcommand")
	}
	if !strings.Contains(r.Stderr, "unknown command") {
		t.Errorf("expected 'unknown command', got stderr=%q", r.Stderr)
	}
}

func TestIntegration_Secrets_SetRequiresKey(t *testing.T) {
	r := run(t, []string{"secrets", "set"}, []string{"HOME=" + t.TempDir()}, "")
	if r.ExitCode == 0 {
		t.Error("expected non-zero exit when key is missing")
	}
	if !strings.Contains(r.Stderr, "secrets set KEY") {
		t.Errorf("expected usage hint, got stderr=%q", r.Stderr)
	}
}

func TestIntegration_Secrets_RmRequiresKey(t *testing.T) {
	r := run(t, []string{"secrets", "rm"}, []string{"HOME=" + t.TempDir()}, "")
	if r.ExitCode == 0 {
		t.Error("expected non-zero exit when key is missing")
	}
	if !strings.Contains(r.Stderr, "secrets rm KEY") {
		t.Errorf("expected usage hint, got stderr=%q", r.Stderr)
	}
}

// TestIntegration_Secrets_Init_RefusesOverwrite verifies the local
// half of secrets init: a private key is generated and written, then
// the second invocation without --rotate refuses to clobber it. We
// don't exercise the server-side pubkey upload here because it goes
// through the daemon IPC + WS round-trip; the daemon ListSkills test
// already covers that path.
func TestIntegration_Secrets_Init_RefusesOverwrite(t *testing.T) {
	home := t.TempDir()
	keyPath := filepath.Join(home, ".greenlight", "key")
	if err := os.MkdirAll(filepath.Dir(keyPath), 0700); err != nil {
		t.Fatal(err)
	}
	// Pre-populate a key so init's overwrite check trips before any
	// daemon round-trip.
	if err := os.WriteFile(keyPath, []byte("dummy"), 0600); err != nil {
		t.Fatal(err)
	}

	r := run(t, []string{"secrets", "init"}, []string{
		"HOME=" + home,
		// Point at a nonexistent daemon socket so a stray success path
		// can't reach the user's real daemon.
		"GREENLIGHT_DAEMON_SOCK=/tmp/gl-test-no-daemon.sock",
	}, "")
	if r.ExitCode == 0 {
		t.Error("expected refusal to overwrite without --rotate")
	}
	if !strings.Contains(r.Stderr, "key already exists") {
		t.Errorf("expected 'key already exists' message, got stderr=%q", r.Stderr)
	}
}

// TestIntegration_Secrets_List_NoDaemon verifies the CLI surfaces a
// helpful error when there's no daemon to talk to.
func TestIntegration_Secrets_List_NoDaemon(t *testing.T) {
	r := run(t, []string{"secrets", "list"}, []string{
		"HOME=" + t.TempDir(),
		"GREENLIGHT_DAEMON_SOCK=/tmp/gl-test-no-daemon.sock",
	}, "")
	if r.ExitCode == 0 {
		t.Error("expected non-zero exit when daemon unreachable")
	}
}

// ---------- stream — arg validation ----------

func TestIntegration_Stream_MissingTranscript(t *testing.T) {
	r := run(t, []string{"stream", "--session-id", "s1", "--bridge", "/tmp/b"}, nil, "")
	if r.ExitCode == 0 {
		t.Error("expected non-zero exit for missing --transcript")
	}
}

func TestIntegration_Stream_MissingSessionID(t *testing.T) {
	r := run(t, []string{"stream", "--transcript", "/tmp/t.jsonl", "--bridge", "/tmp/b"}, nil, "")
	if r.ExitCode == 0 {
		t.Error("expected non-zero exit for missing --session-id")
	}
}

func TestIntegration_Stream_MissingBridge(t *testing.T) {
	r := run(t, []string{"stream", "--transcript", "/tmp/t.jsonl", "--session-id", "s1"}, nil, "")
	if r.ExitCode == 0 {
		t.Error("expected non-zero exit for missing --bridge")
	}
}

// ---------- stream — bridge mode ----------

func TestIntegration_Stream_BridgeMode(t *testing.T) {
	tmpDir, err := os.MkdirTemp("", "greenlight-stream-*")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(tmpDir)

	transcriptPath := filepath.Join(tmpDir, "transcript.jsonl")
	bridgePath := filepath.Join(tmpDir, "bridge")

	// Create bridge file
	if err := os.WriteFile(bridgePath, nil, 0644); err != nil {
		t.Fatal(err)
	}

	// Write transcript lines before starting (streamer reads from beginning)
	lines := []string{
		`{"type":"message","content":"hello"}`,
		`{"type":"message","content":"world"}`,
	}
	if err := os.WriteFile(transcriptPath, []byte(strings.Join(lines, "\n")+"\n"), 0644); err != nil {
		t.Fatal(err)
	}

	// Start streamer
	cmd := exec.Command(greenlightBin, "stream",
		"--transcript", transcriptPath,
		"--session-id", "test-stream-1",
		"--relay-id", "relay-1",
		"--bridge", bridgePath,
	)
	cmd.Env = []string{
		"HOME=" + os.Getenv("HOME"),
		"PATH=" + os.Getenv("PATH"),
		"TMPDIR=" + os.TempDir(),
	}

	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}

	// Wait for the streamer to process the lines
	deadline := time.Now().Add(5 * time.Second)
	var bridgeContent string
	for time.Now().Before(deadline) {
		data, _ := os.ReadFile(bridgePath)
		bridgeContent = string(data)
		if strings.Contains(bridgeContent, "hello") && strings.Contains(bridgeContent, "world") {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}

	cmd.Process.Kill()
	cmd.Wait()

	if !strings.Contains(bridgeContent, "hello") {
		t.Errorf("expected 'hello' in bridge file, got %q", bridgeContent)
	}
	if !strings.Contains(bridgeContent, "world") {
		t.Errorf("expected 'world' in bridge file, got %q", bridgeContent)
	}
}

// ---------- embedded library extraction ----------

func TestIntegration_EmbeddedLib(t *testing.T) {
	p := extractEmbeddedLib()
	if p == "" {
		t.Fatal("extractEmbeddedLib returned empty path")
	}
	defer os.Remove(p)

	info, err := os.Stat(p)
	if err != nil {
		t.Fatalf("extracted file does not exist at %s: %v", p, err)
	}

	if info.Size() == 0 {
		t.Error("extracted library is empty")
	}
}

// enrollTestHost performs an HTTP enrollment against the test server using
// the given device ID and returns the assigned host (session) ID. Used by
// daemon tests to pre-enroll before the daemon's WebSocket connects.
func enrollTestHost(t *testing.T, deviceID string) string {
	t.Helper()
	hostID := generateUUID()
	body, _ := json.Marshal(map[string]string{
		"device_id":  deviceID,
		"session_id": hostID,
		"hostname":   "test-host",
	})
	resp, err := http.Post(testServerURL.BaseURL()+"/session/enroll", "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatalf("enroll: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("enroll: status %d", resp.StatusCode)
	}
	return hostID
}
