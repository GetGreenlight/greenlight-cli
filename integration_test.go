//go:build integration

package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"nhooyr.io/websocket"
)

// Paths set by TestMain
var (
	greenlightBin string // path to compiled greenlight binary
	mockClaudeBin string // path to mock claude binary
)

// ---------- test server ----------

type recordedRequest struct {
	Method string
	Path   string
	Body   []byte
}

type testServer struct {
	*httptest.Server

	mu       sync.Mutex
	requests []recordedRequest

	// per-endpoint response overrides (path → handler)
	handlers map[string]http.HandlerFunc

	// optional WebSocket handler for /ws/relay
	wsHandlerFn func(w http.ResponseWriter, r *http.Request)
}

func newTestServer() *testServer {
	ts := &testServer{
		handlers: make(map[string]http.HandlerFunc),
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		// WebSocket upgrade for /ws/relay
		if r.URL.Path == "/ws/relay" {
			ts.mu.Lock()
			wsh := ts.wsHandlerFn
			ts.mu.Unlock()
			if wsh != nil {
				wsh(w, r)
				return
			}
			w.WriteHeader(404)
			return
		}

		body, _ := io.ReadAll(r.Body)
		ts.mu.Lock()
		ts.requests = append(ts.requests, recordedRequest{
			Method: r.Method,
			Path:   r.URL.Path,
			Body:   body,
		})
		ts.mu.Unlock()

		ts.mu.Lock()
		h, ok := ts.handlers[r.URL.Path]
		ts.mu.Unlock()

		if ok {
			// re-create body for handler since we consumed it
			r.Body = io.NopCloser(bytes.NewReader(body))
			h(w, r)
			return
		}

		// defaults
		switch r.URL.Path {
		case "/session/enroll":
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprint(w, `{"approved":true}`)
		case "/activity":
			w.WriteHeader(200)
		case "/transcript":
			w.WriteHeader(200)
		default:
			w.WriteHeader(404)
		}
	})
	ts.Server = httptest.NewServer(mux)
	return ts
}

func (ts *testServer) setHandler(path string, h http.HandlerFunc) {
	ts.mu.Lock()
	ts.handlers[path] = h
	ts.mu.Unlock()
}

func (ts *testServer) setWSHandler(h func(w http.ResponseWriter, r *http.Request)) {
	ts.mu.Lock()
	ts.wsHandlerFn = h
	ts.mu.Unlock()
}

func (ts *testServer) clearHandlers() {
	ts.mu.Lock()
	ts.handlers = make(map[string]http.HandlerFunc)
	ts.wsHandlerFn = nil
	ts.requests = nil
	ts.mu.Unlock()
}

func (ts *testServer) getRequests(path string) []recordedRequest {
	ts.mu.Lock()
	defer ts.mu.Unlock()
	var out []recordedRequest
	for _, r := range ts.requests {
		if r.Path == path {
			out = append(out, r)
		}
	}
	return out
}

func (ts *testServer) allRequests() []recordedRequest {
	ts.mu.Lock()
	defer ts.mu.Unlock()
	out := make([]recordedRequest, len(ts.requests))
	copy(out, ts.requests)
	return out
}

// wsURL returns ws://host:port/ws/relay for use in -ldflags
func (ts *testServer) wsURL() string {
	return "ws://" + ts.Listener.Addr().String() + "/ws/relay"
}

// baseURL returns http://host:port
func (ts *testServer) baseURL() string {
	return "http://" + ts.Listener.Addr().String()
}

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
	ts := newTestServer()
	defer ts.Close()
	testServerURL = ts

	// Build greenlight binary with the test server URL and version
	greenlightBin = filepath.Join(tmpDir, "greenlight")
	buildCmd := exec.Command("go", "build",
		"-ldflags", "-X main.wsURL="+ts.wsURL()+" -X main.version=0.0.0-test",
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

	// Build mock claude binary
	mockClaudeBin = filepath.Join(tmpDir, "claude")
	mockCmd := exec.Command("go", "build", "-o", mockClaudeBin, "./testdata/mock_claude.go")
	mockCmd.Env = append(os.Environ(),
		"CGO_ENABLED=0",
	)
	mockCmd.Dir = sourceDir()
	if out, err := mockCmd.CombinedOutput(); err != nil {
		fmt.Fprintf(os.Stderr, "failed to build mock claude:\n%s\n%v\n", out, err)
		os.Exit(1)
	}

	os.Exit(m.Run())
}

// testServerURL is shared across tests
var testServerURL *testServer

func sourceDir() string {
	// This test file lives in the repo root
	dir, _ := os.Getwd()
	return dir
}

// ---------- CLI basics ----------

func TestIntegration_NoSubcommand(t *testing.T) {
	r := run(t, nil, nil, "")
	if r.ExitCode == 0 {
		t.Error("expected non-zero exit code")
	}
	if !strings.Contains(r.Stderr, "Usage:") {
		t.Errorf("expected usage text, got stderr=%q", r.Stderr)
	}
	if !strings.Contains(r.Stderr, "0.0.0-test") {
		t.Errorf("expected version in usage text, got stderr=%q", r.Stderr)
	}
	if !strings.Contains(r.Stderr, "relay:") {
		t.Errorf("expected relay URL in usage text, got stderr=%q", r.Stderr)
	}
}

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
			if !strings.Contains(r.Stderr, "relay:") {
				t.Errorf("expected relay URL in version output, got stderr=%q", r.Stderr)
			}
		})
	}
}

// ---------- connect arg validation ----------

func TestIntegration_Connect_MissingDeviceID(t *testing.T) {
	// Use a temp HOME so ~/.greenlight/config doesn't supply a device_id
	tmpHome := t.TempDir()
	r := run(t, []string{"connect", "--no-daemon"}, []string{"HOME=" + tmpHome}, "")
	if r.ExitCode == 0 {
		t.Error("expected non-zero exit code")
	}
	if !strings.Contains(r.Stderr, "device ID") {
		t.Errorf("expected device ID error, got stderr=%q", r.Stderr)
	}
}

func TestIntegration_Connect_MissingProject(t *testing.T) {
	// Project is optional (defaults to cwd basename), so with a device-id
	// and no relay server, it should get past arg validation and fail on enrollment.
	r := run(t, []string{"connect", "--no-daemon", "--device-id", "test-device"}, nil, "")
	if r.ExitCode == 0 {
		t.Error("expected non-zero exit code")
	}
	if strings.Contains(r.Stderr, "project name is required") {
		t.Errorf("project should default to cwd basename, got stderr=%q", r.Stderr)
	}
}

func TestIntegration_Connect_DeviceIDFromEnv(t *testing.T) {
	// Should get past device-id and project validation (project defaults to cwd basename)
	r := run(t, []string{"connect", "--no-daemon"}, []string{"GREENLIGHT_DEVICE_ID=test-device"}, "")
	if r.ExitCode == 0 {
		t.Error("expected non-zero exit code")
	}
	if strings.Contains(r.Stderr, "device ID is required") {
		t.Errorf("expected to get past device-id validation, got stderr=%q", r.Stderr)
	}
}

func TestIntegration_Connect_DeviceIDFromConfig(t *testing.T) {
	// Create a temporary config file
	home, err := os.MkdirTemp("", "greenlight-home-*")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(home)

	configDir := filepath.Join(home, ".greenlight")
	os.MkdirAll(configDir, 0755)
	os.WriteFile(filepath.Join(configDir, "config"), []byte("device_id=config-device\n"), 0644)

	// Should get past device-id (from config) and project (defaults to cwd basename)
	r := run(t, []string{"connect", "--no-daemon"}, []string{"HOME=" + home}, "")
	if r.ExitCode == 0 {
		t.Error("expected non-zero exit code")
	}
	if strings.Contains(r.Stderr, "device ID is required") {
		t.Errorf("expected device-id from config, got stderr=%q", r.Stderr)
	}
}

func TestIntegration_Connect_ProjectFromEnv(t *testing.T) {
	// Should get past project validation and reach enrollment
	testServerURL.clearHandlers()
	r := run(t, []string{"connect", "--no-daemon"},
		[]string{
			"GREENLIGHT_DEVICE_ID=test-device",
			"GREENLIGHT_PROJECT=test-project",
		}, "")
	// It should at least get past arg validation. The binary will try to run
	// claude which won't be in PATH, so it'll fail, but not on arg validation.
	if strings.Contains(r.Stderr, "project name is required") {
		t.Errorf("should have gotten past project validation, got stderr=%q", r.Stderr)
	}
}

// ---------- connect full flow ----------

func TestIntegration_Connect_FullFlow(t *testing.T) {
	testServerURL.clearHandlers()

	// Create a working directory with .claude for hook installation
	workDir, err := os.MkdirTemp("", "greenlight-connect-*")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(workDir)

	// Put mock claude on PATH
	pathWithMock := filepath.Dir(mockClaudeBin) + ":" + os.Getenv("PATH")

	cmd := exec.Command(greenlightBin, "connect", "--no-daemon", "--device-id", "test-dev", "--project", "test-proj")
	cmd.Dir = workDir
	cmd.Env = []string{
		"HOME=" + os.Getenv("HOME"),
		"PATH=" + pathWithMock,
		"TMPDIR=" + os.TempDir(),
		"TERM=xterm-256color",
	}
	cmd.Stdin = strings.NewReader("")

	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	done := make(chan error, 1)
	if err := cmd.Start(); err != nil {
		t.Fatalf("start: %v", err)
	}
	go func() { done <- cmd.Wait() }()

	select {
	case err := <-done:
		// We expect it to exit (mock claude exits immediately)
		_ = err
	case <-time.After(15 * time.Second):
		cmd.Process.Kill()
		t.Fatalf("connect timed out; stdout=%q stderr=%q", stdout.String(), stderr.String())
	}

	// Verify enrollment request was sent
	enrollReqs := testServerURL.getRequests("/session/enroll")
	if len(enrollReqs) == 0 {
		t.Fatal("expected enrollment request")
	}
	var enrollBody map[string]string
	if err := json.Unmarshal(enrollReqs[0].Body, &enrollBody); err != nil {
		t.Fatalf("parse enroll body: %v", err)
	}
	if enrollBody["device_id"] != "test-dev" {
		t.Errorf("expected device_id=test-dev, got %q", enrollBody["device_id"])
	}
	if enrollBody["project"] != "test-proj" {
		t.Errorf("expected project=test-proj, got %q", enrollBody["project"])
	}
	if enrollBody["session_id"] == "" {
		t.Error("expected non-empty session_id")
	}

}

func TestIntegration_Connect_EnrollmentRejected(t *testing.T) {
	testServerURL.clearHandlers()
	testServerURL.setHandler("/session/enroll", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"approved":false,"message":"rejected by test"}`)
	})
	defer testServerURL.clearHandlers()

	pathWithMock := filepath.Dir(mockClaudeBin) + ":" + os.Getenv("PATH")
	r := run(t, []string{"connect", "--no-daemon", "--device-id", "test-dev", "--project", "test-proj"},
		[]string{"PATH=" + pathWithMock}, "")
	if r.ExitCode == 0 {
		t.Error("expected non-zero exit code for rejected enrollment")
	}
	if !strings.Contains(r.Stderr, "enrollment") {
		t.Errorf("expected enrollment error, got stderr=%q", r.Stderr)
	}
}

// ---------- connect — WebSocket input injection ----------

func TestIntegration_Connect_WSInputInjection(t *testing.T) {
	testServerURL.clearHandlers()

	workDir, err := os.MkdirTemp("", "greenlight-wsinject-*")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(workDir)

	// File where mock claude will write the input it received
	outputFile := filepath.Join(workDir, "claude-received.txt")

	// Collects all binary frames (PTY output) received back from the relay.
	var wsReceived bytes.Buffer
	var wsReceivedMu sync.Mutex
	wsDone := make(chan struct{})

	// Set up WebSocket handler: accept connection, wait briefly for the
	// relay to be ready, then send a test message and collect responses.
	testServerURL.setWSHandler(func(w http.ResponseWriter, r *http.Request) {
		defer close(wsDone)
		conn, err := websocket.Accept(w, r, &websocket.AcceptOptions{
			InsecureSkipVerify: true,
		})
		if err != nil {
			t.Logf("ws accept error: %v", err)
			return
		}
		defer conn.Close(websocket.StatusNormalClosure, "done")

		// Give the relay a moment to start the child process
		time.Sleep(2 * time.Second)

		// Send a text frame with \r — the relay injects text frames
		// into the PTY as keystrokes.
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		err = conn.Write(ctx, websocket.MessageText, []byte("HELLO_FROM_SERVER\r"))
		if err != nil {
			t.Logf("ws write error: %v", err)
			return
		}

		// Read messages until the connection closes, collecting PTY output
		for {
			_, data, err := conn.Read(ctx)
			if err != nil {
				return
			}
			wsReceivedMu.Lock()
			wsReceived.Write(data)
			wsReceivedMu.Unlock()
		}
	})
	defer testServerURL.clearHandlers()

	// Allocate a PTY for the greenlight process so that its setRaw()
	// ioctl succeeds (it requires a real terminal on stdin).
	master, slave, err := openPTY()
	if err != nil {
		t.Fatalf("openPTY: %v", err)
	}
	defer master.Close()

	// Set a reasonable window size so syncWinsize doesn't complain
	setWinsize(slave.Fd(), &Winsize{Row: 24, Col: 80})

	pathWithMock := filepath.Dir(mockClaudeBin) + ":" + os.Getenv("PATH")

	cmd := exec.Command(greenlightBin, "connect", "--no-daemon", "--device-id", "test-dev", "--project", "test-proj")
	cmd.Dir = workDir
	cmd.Env = []string{
		"HOME=" + os.Getenv("HOME"),
		"PATH=" + pathWithMock,
		"TMPDIR=" + os.TempDir(),
		"TERM=xterm-256color",
		"MOCK_CLAUDE_OUTPUT=" + outputFile,
	}
	cmd.Stdin = slave
	cmd.Stdout = slave
	cmd.Stderr = slave

	done := make(chan error, 1)
	if err := cmd.Start(); err != nil {
		t.Fatalf("start: %v", err)
	}
	// Close slave in parent after child inherits it
	slave.Close()

	go func() { done <- cmd.Wait() }()

	select {
	case <-done:
		// process exited
	case <-time.After(15 * time.Second):
		cmd.Process.Kill()
		t.Fatal("connect timed out")
	}

	// Check that mock claude received the server's input
	data, err := os.ReadFile(outputFile)
	if err != nil {
		t.Fatalf("mock claude output file not created: %v", err)
	}
	received := string(data)
	if !strings.Contains(received, "HELLO_FROM_SERVER") {
		t.Errorf("expected mock claude to receive 'HELLO_FROM_SERVER', got %q", received)
	}

	// Wait for the WS handler to finish collecting PTY output
	select {
	case <-wsDone:
	case <-time.After(5 * time.Second):
		t.Log("WS handler did not finish in time")
	}

	// Verify that PTY output was sent back to the server over WebSocket.
	// The PTY echoes input in cooked mode, so "HELLO_FROM_SERVER" should
	// appear in the binary frames. Mock claude's "MOCK_CLAUDE_STARTED"
	// output should also be present.
	wsReceivedMu.Lock()
	wsOutput := wsReceived.String()
	wsReceivedMu.Unlock()

	if !strings.Contains(wsOutput, "HELLO_FROM_SERVER") {
		t.Errorf("expected 'HELLO_FROM_SERVER' in WS output (PTY echo), got %q", wsOutput)
	}
}

// ---------- connect — suspend/resume (Ctrl-Z) ----------

func TestIntegration_Connect_SuspendResume(t *testing.T) {
	testServerURL.clearHandlers()

	workDir, err := os.MkdirTemp("", "greenlight-suspend-*")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(workDir)

	// File where mock claude writes the input it received
	outputFile := filepath.Join(workDir, "claude-received.txt")

	// Allocate a PTY for greenlight's stdin/stdout
	master, slave, err := openPTY()
	if err != nil {
		t.Fatalf("openPTY: %v", err)
	}
	defer master.Close()
	setWinsize(slave.Fd(), &Winsize{Row: 24, Col: 80})

	pathWithMock := filepath.Dir(mockClaudeBin) + ":" + os.Getenv("PATH")

	cmd := exec.Command(greenlightBin, "connect", "--no-daemon", "--device-id", "test-dev", "--project", "test-proj")
	cmd.Dir = workDir
	cmd.Env = []string{
		"HOME=" + os.Getenv("HOME"),
		"PATH=" + pathWithMock,
		"TMPDIR=" + os.TempDir(),
		"TERM=xterm-256color",
		"MOCK_CLAUDE_OUTPUT=" + outputFile,
	}
	cmd.Stdin = slave
	cmd.Stdout = slave
	cmd.Stderr = slave
	// Put greenlight in its own process group so that its
	// Kill(0, SIGTSTP) doesn't stop the test runner.
	cmd.SysProcAttr = &syscall.SysProcAttr{
		Setpgid: true,
	}

	done := make(chan error, 1)
	if err := cmd.Start(); err != nil {
		t.Fatalf("start: %v", err)
	}
	slave.Close()
	go func() { done <- cmd.Wait() }()

	// Wait for mock claude to start up inside the PTY
	time.Sleep(1 * time.Second)

	// Send Ctrl-Z — greenlight should intercept and suspend itself
	if _, err := master.Write([]byte{0x1a}); err != nil {
		t.Fatalf("write Ctrl-Z: %v", err)
	}

	// Give greenlight time to stop
	time.Sleep(500 * time.Millisecond)

	// Resume greenlight (simulates shell "fg")
	syscall.Kill(cmd.Process.Pid, syscall.SIGCONT)

	// Give greenlight time to re-enter raw mode
	time.Sleep(500 * time.Millisecond)

	// Send input after resume — mock claude should receive this
	if _, err := master.Write([]byte("AFTER_RESUME\n")); err != nil {
		t.Fatalf("write after resume: %v", err)
	}

	select {
	case <-done:
		// process exited
	case <-time.After(15 * time.Second):
		cmd.Process.Kill()
		t.Fatal("connect timed out")
	}

	// Verify mock claude received input after the suspend/resume cycle
	data, err := os.ReadFile(outputFile)
	if err != nil {
		t.Fatalf("mock claude output file not created: %v", err)
	}
	if !strings.Contains(string(data), "AFTER_RESUME") {
		t.Errorf("expected 'AFTER_RESUME' after suspend/resume, got %q", string(data))
	}
}

// ---------- connect — transcript relay pipeline ----------

func TestIntegration_Connect_TranscriptRelay(t *testing.T) {
	testServerURL.clearHandlers()

	workDir, err := os.MkdirTemp("", "greenlight-transcript-*")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(workDir)

	transcriptPath := filepath.Join(workDir, "transcript.jsonl")

	// Collect text frames (transcript data) from the WebSocket.
	// tailBridge sends: {"type":"transcript","data":<line>}
	var wsTextFrames []string
	var wsTextMu sync.Mutex
	wsDone := make(chan struct{})

	testServerURL.setWSHandler(func(w http.ResponseWriter, r *http.Request) {
		defer close(wsDone)
		conn, err := websocket.Accept(w, r, &websocket.AcceptOptions{
			InsecureSkipVerify: true,
		})
		if err != nil {
			t.Logf("ws accept error: %v", err)
			return
		}
		defer conn.Close(websocket.StatusNormalClosure, "done")

		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()

		for {
			msgType, data, err := conn.Read(ctx)
			if err != nil {
				return
			}
			if msgType == websocket.MessageText {
				// Auto-approve permission requests so interpose doesn't block
				var msg struct {
					Type string                 `json:"type"`
					Data map[string]interface{} `json:"data"`
				}
				if json.Unmarshal(data, &msg) == nil && msg.Type == "permission_request" {
					requestID, _ := msg.Data["request_id"].(string)
					resp, _ := json.Marshal(map[string]interface{}{
						"type":       "permission_response",
						"request_id": requestID,
						"behavior":   "allow",
					})
					conn.Write(ctx, websocket.MessageText, resp)
					continue
				}
				wsTextMu.Lock()
				wsTextFrames = append(wsTextFrames, string(data))
				wsTextMu.Unlock()
			}
		}
	})
	defer testServerURL.clearHandlers()

	// Allocate a PTY for the greenlight process
	master, slave, err := openPTY()
	if err != nil {
		t.Fatalf("openPTY: %v", err)
	}
	defer master.Close()
	setWinsize(slave.Fd(), &Winsize{Row: 24, Col: 80})

	pathWithMock := filepath.Dir(mockClaudeBin) + ":" + os.Getenv("PATH")

	cmd := exec.Command(greenlightBin, "connect", "--no-daemon", "--device-id", "test-dev", "--project", "test-proj")
	cmd.Dir = workDir
	cmd.Env = []string{
		"HOME=" + os.Getenv("HOME"),
		"PATH=" + pathWithMock,
		"TMPDIR=" + os.TempDir(),
		"TERM=xterm-256color",
		"MOCK_CLAUDE_TRANSCRIPT=" + transcriptPath,
	}
	cmd.Stdin = slave
	cmd.Stdout = slave
	cmd.Stderr = slave

	done := make(chan error, 1)
	if err := cmd.Start(); err != nil {
		t.Fatalf("start: %v", err)
	}
	slave.Close()
	go func() { done <- cmd.Wait() }()

	select {
	case <-done:
	case <-time.After(15 * time.Second):
		cmd.Process.Kill()
		t.Fatal("connect timed out")
	}

	// Wait for WS handler to finish
	select {
	case <-wsDone:
	case <-time.After(5 * time.Second):
		t.Log("WS handler did not finish in time")
	}

	// Verify that transcript text frames were received by the server.
	// Each frame should be: {"type":"transcript","data":<jsonl-line>}
	wsTextMu.Lock()
	frames := make([]string, len(wsTextFrames))
	copy(frames, wsTextFrames)
	wsTextMu.Unlock()

	var foundLine1, foundLine2 bool
	for _, frame := range frames {
		if strings.Contains(frame, "TRANSCRIPT_TEST_LINE_1") {
			foundLine1 = true
		}
		if strings.Contains(frame, "TRANSCRIPT_TEST_LINE_2") {
			foundLine2 = true
		}
	}

	if !foundLine1 {
		t.Errorf("expected text frame containing 'TRANSCRIPT_TEST_LINE_1', got %d frames: %v", len(frames), frames)
	}
	if !foundLine2 {
		t.Errorf("expected text frame containing 'TRANSCRIPT_TEST_LINE_2', got %d frames: %v", len(frames), frames)
	}

	// Verify frames have the expected wrapper structure
	if len(frames) > 0 {
		var wrapper map[string]interface{}
		if err := json.Unmarshal([]byte(frames[0]), &wrapper); err != nil {
			t.Errorf("expected JSON text frame, got %q: %v", frames[0], err)
		} else {
			if wrapper["type"] != "transcript" {
				t.Errorf("expected type=transcript, got %v", wrapper["type"])
			}
			if wrapper["data"] == nil {
				t.Error("expected data field in transcript frame")
			}
		}
	}
}

// ---------- connect — incremental transcript relay with disconnection ----------

func TestIntegration_Connect_TranscriptRelayIncremental(t *testing.T) {
	testServerURL.clearHandlers()

	workDir, err := os.MkdirTemp("", "greenlight-transcript-incr-*")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(workDir)

	transcriptPath := filepath.Join(workDir, "transcript.jsonl")

	const totalLines = 10

	// Collect text frames from the WebSocket across multiple connections.
	// The server will close the first connection after a few frames to
	// simulate a brief disconnection. The client should reconnect and
	// re-deliver any messages that failed during the gap.
	var wsTextFrames []string
	var wsTextMu sync.Mutex
	var connCount int32
	wsDone := make(chan struct{})

	testServerURL.setWSHandler(func(w http.ResponseWriter, r *http.Request) {
		conn, err := websocket.Accept(w, r, &websocket.AcceptOptions{
			InsecureSkipVerify: true,
		})
		if err != nil {
			t.Logf("ws accept error: %v", err)
			return
		}

		wsTextMu.Lock()
		connCount++
		thisConn := connCount
		wsTextMu.Unlock()

		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()

		frameCount := 0
		for {
			msgType, data, err := conn.Read(ctx)
			if err != nil {
				conn.CloseNow()
				if thisConn == 1 {
					// First connection was intentionally closed; continue
					// accepting reconnections by returning (the HTTP
					// handler will be called again for the next WS dial).
					return
				}
				// Final connection: signal done
				close(wsDone)
				return
			}
			if msgType == websocket.MessageText {
				// Auto-approve permission requests so interpose doesn't block
				var msg struct {
					Type string                 `json:"type"`
					Data map[string]interface{} `json:"data"`
				}
				if json.Unmarshal(data, &msg) == nil && msg.Type == "permission_request" {
					requestID, _ := msg.Data["request_id"].(string)
					resp, _ := json.Marshal(map[string]interface{}{
						"type":       "permission_response",
						"request_id": requestID,
						"behavior":   "allow",
					})
					conn.Write(ctx, websocket.MessageText, resp)
					continue
				}

				wsTextMu.Lock()
				wsTextFrames = append(wsTextFrames, string(data))
				wsTextMu.Unlock()

				frameCount++
				// After receiving 3 text frames on the first connection,
				// close it abruptly to simulate a network disruption.
				// The client should reconnect and the queued messages
				// from the gap should eventually arrive.
				if thisConn == 1 && frameCount >= 3 {
					conn.Close(websocket.StatusGoingAway, "simulated disruption")
					return
				}
			}
		}
	})
	defer testServerURL.clearHandlers()

	// Allocate a PTY for the greenlight process
	master, slave, err := openPTY()
	if err != nil {
		t.Fatalf("openPTY: %v", err)
	}
	defer master.Close()
	setWinsize(slave.Fd(), &Winsize{Row: 24, Col: 80})

	pathWithMock := filepath.Dir(mockClaudeBin) + ":" + os.Getenv("PATH")

	cmd := exec.Command(greenlightBin, "connect", "--no-daemon", "--device-id", "test-dev", "--project", "test-proj")
	cmd.Dir = workDir
	cmd.Env = []string{
		"HOME=" + os.Getenv("HOME"),
		"PATH=" + pathWithMock,
		"TMPDIR=" + os.TempDir(),
		"TERM=xterm-256color",
		"MOCK_CLAUDE_TRANSCRIPT_INCREMENTAL=" + transcriptPath,
	}
	cmd.Stdin = slave
	cmd.Stdout = slave
	cmd.Stderr = slave

	done := make(chan error, 1)
	if err := cmd.Start(); err != nil {
		t.Fatalf("start: %v", err)
	}
	slave.Close()
	go func() { done <- cmd.Wait() }()

	select {
	case <-done:
	case <-time.After(30 * time.Second):
		cmd.Process.Kill()
		t.Fatal("connect timed out")
	}

	// Wait for WS handler to finish
	select {
	case <-wsDone:
	case <-time.After(5 * time.Second):
		t.Log("WS handler did not finish in time")
	}

	// Verify that ALL incremental transcript lines were received,
	// even those sent during/after the disconnection.
	wsTextMu.Lock()
	frames := make([]string, len(wsTextFrames))
	copy(frames, wsTextFrames)
	wsTextMu.Unlock()

	for i := 1; i <= totalLines; i++ {
		marker := fmt.Sprintf("INCREMENTAL_LINE_%d", i)
		found := false
		for _, frame := range frames {
			if strings.Contains(frame, marker) {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("missing transcript line %q in %d received frames", marker, len(frames))
		}
	}

	if t.Failed() {
		t.Logf("Received %d text frames:", len(frames))
		for i, f := range frames {
			t.Logf("  frame[%d]: %s", i, f)
		}
	}
}

// Hook tests were removed — the hook subcommand was removed.
// All permission requests now go through interpose → WS, tested via TestIntegration_WSPermission_*.

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

func TestIntegration_Stream_MissingServerOrBridge(t *testing.T) {
	r := run(t, []string{"stream", "--transcript", "/tmp/t.jsonl", "--session-id", "s1", "--device-id", "d1"}, nil, "")
	if r.ExitCode == 0 {
		t.Error("expected non-zero exit for missing --server/--bridge")
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

// ---------- stream — HTTP mode ----------

func TestIntegration_Stream_HTTPMode(t *testing.T) {
	testServerURL.clearHandlers()

	tmpDir, err := os.MkdirTemp("", "greenlight-stream-http-*")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(tmpDir)

	transcriptPath := filepath.Join(tmpDir, "transcript.jsonl")

	// Write transcript lines
	lines := []string{
		`{"type":"message","content":"line1"}`,
		`{"type":"message","content":"line2"}`,
	}
	if err := os.WriteFile(transcriptPath, []byte(strings.Join(lines, "\n")+"\n"), 0644); err != nil {
		t.Fatal(err)
	}

	cmd := exec.Command(greenlightBin, "stream",
		"--transcript", transcriptPath,
		"--session-id", "test-http-1",
		"--device-id", "test-dev",
		"--project", "test-proj",
		"--relay-id", "relay-http-1",
		"--server", testServerURL.baseURL(),
	)
	cmd.Env = []string{
		"HOME=" + os.Getenv("HOME"),
		"PATH=" + os.Getenv("PATH"),
		"TMPDIR=" + os.TempDir(),
	}

	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}

	// Wait for transcript POSTs
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		reqs := testServerURL.getRequests("/transcript")
		if len(reqs) >= 2 {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}

	cmd.Process.Kill()
	cmd.Wait()

	reqs := testServerURL.getRequests("/transcript")
	if len(reqs) < 2 {
		t.Fatalf("expected at least 2 transcript POSTs, got %d", len(reqs))
	}

	// Check payload structure
	var payload map[string]interface{}
	json.Unmarshal(reqs[0].Body, &payload)
	if payload["device_id"] != "test-dev" {
		t.Errorf("expected device_id=test-dev, got %v", payload["device_id"])
	}
	if payload["session_id"] != "test-http-1" {
		t.Errorf("expected session_id=test-http-1, got %v", payload["session_id"])
	}
	if payload["data"] == nil {
		t.Error("expected data field in transcript POST")
	}
}

func TestIntegration_Stream_HTTPMode_FatalError(t *testing.T) {
	testServerURL.clearHandlers()
	testServerURL.setHandler("/transcript", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(400)
	})
	defer testServerURL.clearHandlers()

	tmpDir, err := os.MkdirTemp("", "greenlight-stream-fatal-*")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(tmpDir)

	transcriptPath := filepath.Join(tmpDir, "transcript.jsonl")
	os.WriteFile(transcriptPath, []byte(`{"type":"msg"}`+"\n"), 0644)

	cmd := exec.Command(greenlightBin, "stream",
		"--transcript", transcriptPath,
		"--session-id", "test-fatal-1",
		"--device-id", "test-dev",
		"--project", "test-proj",
		"--relay-id", "relay-fatal-1",
		"--server", testServerURL.baseURL(),
	)
	cmd.Env = []string{
		"HOME=" + os.Getenv("HOME"),
		"PATH=" + os.Getenv("PATH"),
		"TMPDIR=" + os.TempDir(),
	}

	done := make(chan error, 1)
	cmd.Start()
	go func() { done <- cmd.Wait() }()

	// Should exit on its own due to 400 error
	select {
	case <-done:
		// good, streamer exited
	case <-time.After(5 * time.Second):
		cmd.Process.Kill()
		t.Error("streamer did not exit on fatal 400 error")
	}
}


// ---------- WS permission request/response ----------

// wsPermissionHelper sets up a connect session with a WS handler that processes
// permission_request messages and responds with permission_response.
// Returns a channel that receives each permission_request's data payload.
// wsPermissionHelper sets up a connect session with a WS handler that processes
// permission_request messages. It returns the relay ID extracted from the WS
// connection URL, so the caller can connect to the interpose socket directly.
// respondFn is called for each permission_request and should return the response.
// receivedCh receives each permission_request's data payload.
func wsPermissionHelper(t *testing.T, respondFn func(data map[string]interface{}) map[string]interface{}, receivedCh chan map[string]interface{}) (workDir string, master *os.File, cmd *exec.Cmd, done chan error) {
	t.Helper()
	testServerURL.clearHandlers()

	workDir, err := os.MkdirTemp("", "greenlight-wsperm-*")
	if err != nil {
		t.Fatal(err)
	}

	// Channel to capture the relay ID from the WS connection URL
	relayIDCh := make(chan string, 1)

	testServerURL.setWSHandler(func(w http.ResponseWriter, r *http.Request) {
		// Extract relay_id from query params
		if rid := r.URL.Query().Get("relay_id"); rid != "" {
			select {
			case relayIDCh <- rid:
			default:
			}
		}

		conn, err := websocket.Accept(w, r, &websocket.AcceptOptions{
			InsecureSkipVerify: true,
		})
		if err != nil {
			t.Logf("ws accept error: %v", err)
			return
		}
		defer conn.Close(websocket.StatusNormalClosure, "done")

		ctx := context.Background()
		for {
			msgType, msgData, err := conn.Read(ctx)
			if err != nil {
				return
			}
			if msgType != websocket.MessageText {
				continue
			}
			var msg struct {
				Type string                 `json:"type"`
				Data map[string]interface{} `json:"data"`
			}
			if json.Unmarshal(msgData, &msg) != nil {
				continue
			}
			if msg.Type == "permission_request" {
				if receivedCh != nil {
					select {
					case receivedCh <- msg.Data:
					default:
					}
				}
				resp := respondFn(msg.Data)
				respBytes, _ := json.Marshal(resp)
				conn.Write(ctx, websocket.MessageText, respBytes)
			}
		}
	})

	master, slave, err := openPTY()
	if err != nil {
		t.Fatalf("openPTY: %v", err)
	}
	setWinsize(slave.Fd(), &Winsize{Row: 24, Col: 80})

	pathWithMock := filepath.Dir(mockClaudeBin) + ":" + os.Getenv("PATH")

	cmd = exec.Command(greenlightBin, "connect", "--no-daemon", "--device-id", "test-dev", "--project", "test-proj")
	cmd.Dir = workDir
	cmd.Env = []string{
		"HOME=" + os.Getenv("HOME"),
		"PATH=" + pathWithMock,
		"TMPDIR=" + os.TempDir(),
		"TERM=xterm-256color",
	}
	cmd.Stdin = slave
	cmd.Stdout = slave
	cmd.Stderr = slave

	done = make(chan error, 1)
	if err := cmd.Start(); err != nil {
		t.Fatalf("start: %v", err)
	}
	slave.Close()
	go func() { done <- cmd.Wait() }()

	// Wait for WS connection to get the relay ID, then send a fake
	// interpose request to the Unix socket to trigger the WS flow.
	go func() {
		var relayID string
		select {
		case relayID = <-relayIDCh:
		case <-time.After(10 * time.Second):
			return
		}

		// Give the interpose socket a moment to start
		time.Sleep(200 * time.Millisecond)

		sockPath := "/tmp/gl-" + relayID[:8] + ".sock"
		conn, err := net.Dial("unix", sockPath)
		if err != nil {
			t.Logf("dial interpose socket: %v", err)
			return
		}
		defer conn.Close()

		// Send a fake read request (same format as the C interpose library)
		req := `{"type":"read","path":"/tmp/test-file.txt","pid":1234}` + "\n"
		conn.Write([]byte(req))

		// Read the response (blocks until permission is resolved)
		buf := make([]byte, 4096)
		conn.SetReadDeadline(time.Now().Add(10 * time.Second))
		conn.Read(buf)
	}()

	return workDir, master, cmd, done
}

// sendInterposeRequest sends a fake interpose request to the Unix socket
// and returns the response.
func sendInterposeRequest(t *testing.T, relayID string, reqJSON string) string {
	t.Helper()
	sockPath := "/tmp/gl-" + relayID[:8] + ".sock"
	conn, err := net.Dial("unix", sockPath)
	if err != nil {
		t.Fatalf("dial interpose socket %s: %v", sockPath, err)
	}
	defer conn.Close()

	conn.Write([]byte(reqJSON + "\n"))

	buf := make([]byte, 4096)
	conn.SetReadDeadline(time.Now().Add(10 * time.Second))
	n, err := conn.Read(buf)
	if err != nil {
		t.Fatalf("read interpose response: %v", err)
	}
	return string(buf[:n])
}

func TestIntegration_WSPermission_AutoApprove(t *testing.T) {
	receivedCh := make(chan map[string]interface{}, 10)

	workDir, master, cmd, done := wsPermissionHelper(t, func(data map[string]interface{}) map[string]interface{} {
		requestID, _ := data["request_id"].(string)
		return map[string]interface{}{
			"type":       "permission_response",
			"request_id": requestID,
			"behavior":   "allow",
		}
	}, receivedCh)
	defer os.RemoveAll(workDir)
	defer master.Close()
	defer testServerURL.clearHandlers()

	select {
	case <-done:
	case <-time.After(15 * time.Second):
		cmd.Process.Kill()
		t.Fatal("connect timed out")
	}

	// The helper sends a fake interpose request via the Unix socket,
	// which triggers a WS permission_request.
	select {
	case first := <-receivedCh:
		if first["request_id"] == nil || first["request_id"] == "" {
			t.Error("expected non-empty request_id")
		}
		if first["tool_name"] != "Read" {
			t.Errorf("expected tool_name=Read, got %v", first["tool_name"])
		}
		if first["hook_event_name"] != "PermissionRequest" {
			t.Errorf("expected hook_event_name=PermissionRequest, got %v", first["hook_event_name"])
		}
	default:
		t.Error("expected at least one permission_request via WebSocket")
	}
}

func TestIntegration_WSPermission_Deny(t *testing.T) {
	receivedCh := make(chan map[string]interface{}, 10)

	workDir, master, cmd, done := wsPermissionHelper(t, func(data map[string]interface{}) map[string]interface{} {
		requestID, _ := data["request_id"].(string)
		return map[string]interface{}{
			"type":       "permission_response",
			"request_id": requestID,
			"behavior":   "deny",
			"message":    "denied by test",
		}
	}, receivedCh)
	defer os.RemoveAll(workDir)
	defer master.Close()
	defer testServerURL.clearHandlers()

	select {
	case <-done:
	case <-time.After(15 * time.Second):
		cmd.Process.Kill()
		t.Fatal("connect timed out")
	}

	// Verify the request was received and the session completed
	// without hanging (deny response handled correctly).
	select {
	case req := <-receivedCh:
		if req["tool_name"] != "Read" {
			t.Errorf("expected tool_name=Read, got %v", req["tool_name"])
		}
	default:
		t.Error("expected at least one permission_request via WebSocket")
	}
}

func TestIntegration_WSPermission_NoDeviceIDInPayload(t *testing.T) {
	// Verify that device_id, project, relay_id are NOT sent in the payload
	// (server has them from the WS connection).
	receivedCh := make(chan map[string]interface{}, 10)

	workDir, master, cmd, done := wsPermissionHelper(t, func(data map[string]interface{}) map[string]interface{} {
		requestID, _ := data["request_id"].(string)
		return map[string]interface{}{
			"type":       "permission_response",
			"request_id": requestID,
			"behavior":   "allow",
		}
	}, receivedCh)
	defer os.RemoveAll(workDir)
	defer master.Close()
	defer testServerURL.clearHandlers()

	select {
	case <-done:
	case <-time.After(15 * time.Second):
		cmd.Process.Kill()
		t.Fatal("connect timed out")
	}

	select {
	case first := <-receivedCh:
		if first["device_id"] != nil {
			t.Errorf("device_id should not be in WS payload, got %v", first["device_id"])
		}
		if first["project"] != nil {
			t.Errorf("project should not be in WS payload, got %v", first["project"])
		}
		if first["relay_id"] != nil {
			t.Errorf("relay_id should not be in WS payload, got %v", first["relay_id"])
		}
	default:
		t.Fatal("expected at least one permission_request")
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
