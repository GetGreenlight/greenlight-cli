//go:build integration

package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
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
		case "/request":
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprint(w, `{"behavior":"allow"}`)
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
	buildCmd.Env = append(os.Environ(),
		"GOOS=darwin",
		"GOARCH=arm64",
		"CGO_ENABLED=0",
		"MACOSX_DEPLOYMENT_TARGET=12.0",
	)
	buildCmd.Dir = sourceDir()
	if out, err := buildCmd.CombinedOutput(); err != nil {
		fmt.Fprintf(os.Stderr, "failed to build greenlight:\n%s\n%v\n", out, err)
		os.Exit(1)
	}

	// Build mock claude binary
	mockClaudeBin = filepath.Join(tmpDir, "claude")
	mockCmd := exec.Command("go", "build", "-o", mockClaudeBin, "./testdata/mock_claude.go")
	mockCmd.Env = append(os.Environ(),
		"GOOS=darwin",
		"GOARCH=arm64",
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
	r := run(t, []string{"connect"}, nil, "")
	if r.ExitCode == 0 {
		t.Error("expected non-zero exit code")
	}
	if !strings.Contains(r.Stderr, "device ID") {
		t.Errorf("expected device ID error, got stderr=%q", r.Stderr)
	}
}

func TestIntegration_Connect_MissingProject(t *testing.T) {
	r := run(t, []string{"connect", "--device-id", "test-device"}, nil, "")
	if r.ExitCode == 0 {
		t.Error("expected non-zero exit code")
	}
	if !strings.Contains(r.Stderr, "project") {
		t.Errorf("expected project error, got stderr=%q", r.Stderr)
	}
}

func TestIntegration_Connect_DeviceIDFromEnv(t *testing.T) {
	// Should get past device-id validation and fail on project
	r := run(t, []string{"connect"}, []string{"GREENLIGHT_DEVICE_ID=test-device"}, "")
	if r.ExitCode == 0 {
		t.Error("expected non-zero exit code")
	}
	if !strings.Contains(r.Stderr, "project") {
		t.Errorf("expected project error (past device-id), got stderr=%q", r.Stderr)
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

	// Should get past device-id validation and fail on project
	r := run(t, []string{"connect"}, []string{"HOME=" + home}, "")
	if r.ExitCode == 0 {
		t.Error("expected non-zero exit code")
	}
	if !strings.Contains(r.Stderr, "project") {
		t.Errorf("expected project error (past device-id from config), got stderr=%q", r.Stderr)
	}
}

func TestIntegration_Connect_ProjectFromEnv(t *testing.T) {
	// Should get past project validation and reach enrollment
	testServerURL.clearHandlers()
	r := run(t, []string{"connect"},
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

	cmd := exec.Command(greenlightBin, "connect", "--device-id", "test-dev", "--project", "test-proj")
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

	// Verify hooks were installed
	settingsPath := filepath.Join(workDir, ".claude", "settings.local.json")
	settingsData, err := os.ReadFile(settingsPath)
	if err != nil {
		t.Fatalf("expected settings file at %s: %v", settingsPath, err)
	}
	if !strings.Contains(string(settingsData), "greenlight") {
		t.Error("expected greenlight hook in settings")
	}
	if !strings.Contains(string(settingsData), "SessionStart") {
		t.Error("expected SessionStart hook in settings")
	}
	if !strings.Contains(string(settingsData), "PermissionRequest") {
		t.Error("expected PermissionRequest hook in settings")
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
	r := run(t, []string{"connect", "--device-id", "test-dev", "--project", "test-proj"},
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
		time.Sleep(500 * time.Millisecond)

		// Send a binary frame with a newline — the relay will convert
		// \n to \r and inject it into the PTY. The terminal driver
		// translates \r back to \n for the child's stdin.
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		err = conn.Write(ctx, websocket.MessageBinary, []byte("HELLO_FROM_SERVER\n"))
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

	cmd := exec.Command(greenlightBin, "connect", "--device-id", "test-dev", "--project", "test-proj")
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
	if !strings.Contains(wsOutput, "MOCK_CLAUDE_STARTED") {
		t.Errorf("expected 'MOCK_CLAUDE_STARTED' in WS output, got %q", wsOutput)
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

	cmd := exec.Command(greenlightBin, "connect", "--device-id", "test-dev", "--project", "test-proj")
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

// ---------- hook — SessionStart ----------

func TestIntegration_Hook_SessionStart(t *testing.T) {
	testServerURL.clearHandlers()

	// Clean up any enrollment marker from previous tests
	os.Remove(filepath.Join(os.TempDir(), "greenlight-enrolled-relay-123"))

	input := `{"hook_event_name":"SessionStart","session_id":"test-session-123","transcript_path":"/tmp/fake-transcript.jsonl"}`
	r := run(t, []string{"hook"},
		[]string{
			"GREENLIGHT_DEVICE_ID=test-dev",
			"GREENLIGHT_PROJECT=test-proj",
			"GREENLIGHT_SESSION_ID=relay-123",
		}, input)

	if r.ExitCode != 0 {
		t.Errorf("expected exit 0, got %d; stdout=%q stderr=%q", r.ExitCode, r.Stdout, r.Stderr)
	}

	// Give async activity POST a moment to arrive
	time.Sleep(200 * time.Millisecond)

	// Verify enrollment was attempted
	enrollReqs := testServerURL.getRequests("/session/enroll")
	if len(enrollReqs) == 0 {
		t.Error("expected enrollment request on SessionStart")
	}

	// Verify activity POST was sent
	activityReqs := testServerURL.getRequests("/activity")
	if len(activityReqs) == 0 {
		t.Error("expected activity request on SessionStart")
	} else {
		var body map[string]interface{}
		json.Unmarshal(activityReqs[0].Body, &body)
		if body["event"] != "session_start" {
			t.Errorf("expected event=session_start, got %v", body["event"])
		}
		if body["device_id"] != "test-dev" {
			t.Errorf("expected device_id=test-dev, got %v", body["device_id"])
		}
	}
}

func TestIntegration_Hook_MissingDeviceID(t *testing.T) {
	input := `{"hook_event_name":"PermissionRequest","tool_name":"Bash"}`
	r := run(t, []string{"hook"},
		[]string{
			"GREENLIGHT_PROJECT=test-proj",
		}, input)

	if r.ExitCode != 0 {
		t.Errorf("expected exit 0 (deny via JSON), got %d", r.ExitCode)
	}

	var output map[string]interface{}
	if err := json.Unmarshal([]byte(r.Stdout), &output); err != nil {
		t.Fatalf("failed to parse stdout JSON: %v; stdout=%q", err, r.Stdout)
	}

	hso := output["hookSpecificOutput"].(map[string]interface{})
	decision := hso["decision"].(map[string]interface{})
	if decision["behavior"] != "deny" {
		t.Errorf("expected deny, got %v", decision["behavior"])
	}
	msg := decision["message"].(string)
	if !strings.Contains(strings.ToLower(msg), "device id") {
		t.Errorf("expected device ID error message, got %q", msg)
	}
}

func TestIntegration_Hook_MissingProject(t *testing.T) {
	input := `{"hook_event_name":"PermissionRequest","tool_name":"Bash"}`
	r := run(t, []string{"hook"},
		[]string{
			"GREENLIGHT_DEVICE_ID=test-dev",
		}, input)

	if r.ExitCode != 0 {
		t.Errorf("expected exit 0, got %d", r.ExitCode)
	}

	var output map[string]interface{}
	json.Unmarshal([]byte(r.Stdout), &output)
	hso := output["hookSpecificOutput"].(map[string]interface{})
	decision := hso["decision"].(map[string]interface{})
	if decision["behavior"] != "deny" {
		t.Errorf("expected deny, got %v", decision["behavior"])
	}
	msg := decision["message"].(string)
	if !strings.Contains(strings.ToLower(msg), "project") {
		t.Errorf("expected project error message, got %q", msg)
	}
}

// ---------- hook — PermissionRequest ----------

func TestIntegration_Hook_PermissionRequest_Allow(t *testing.T) {
	testServerURL.clearHandlers()
	testServerURL.setHandler("/request", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"behavior":"allow"}`)
	})
	defer testServerURL.clearHandlers()

	input := `{"hook_event_name":"PermissionRequest","tool_name":"Bash","tool_input":{"command":"ls"},"session_id":"s1"}`
	r := run(t, []string{"hook"},
		[]string{
			"GREENLIGHT_DEVICE_ID=test-dev",
			"GREENLIGHT_PROJECT=test-proj",
			"GREENLIGHT_SESSION_ID=relay-1",
		}, input)

	if r.ExitCode != 0 {
		t.Errorf("expected exit 0, got %d; stderr=%q", r.ExitCode, r.Stderr)
	}

	var output map[string]interface{}
	if err := json.Unmarshal([]byte(r.Stdout), &output); err != nil {
		t.Fatalf("parse stdout: %v; stdout=%q", err, r.Stdout)
	}
	hso := output["hookSpecificOutput"].(map[string]interface{})
	decision := hso["decision"].(map[string]interface{})
	if decision["behavior"] != "allow" {
		t.Errorf("expected allow, got %v", decision["behavior"])
	}

	// Verify the request payload forwarded to server
	reqs := testServerURL.getRequests("/request")
	if len(reqs) == 0 {
		t.Fatal("expected /request POST")
	}
	var payload map[string]interface{}
	json.Unmarshal(reqs[0].Body, &payload)
	if payload["device_id"] != "test-dev" {
		t.Errorf("expected device_id=test-dev, got %v", payload["device_id"])
	}
	if payload["tool_name"] != "Bash" {
		t.Errorf("expected tool_name=Bash, got %v", payload["tool_name"])
	}
}

func TestIntegration_Hook_PermissionRequest_Deny(t *testing.T) {
	testServerURL.clearHandlers()
	testServerURL.setHandler("/request", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"behavior":"deny","message":"not allowed by test"}`)
	})
	defer testServerURL.clearHandlers()

	input := `{"hook_event_name":"PermissionRequest","tool_name":"Bash","session_id":"s1"}`
	r := run(t, []string{"hook"},
		[]string{
			"GREENLIGHT_DEVICE_ID=test-dev",
			"GREENLIGHT_PROJECT=test-proj",
			"GREENLIGHT_SESSION_ID=relay-1",
		}, input)

	var output map[string]interface{}
	json.Unmarshal([]byte(r.Stdout), &output)
	hso := output["hookSpecificOutput"].(map[string]interface{})
	decision := hso["decision"].(map[string]interface{})
	if decision["behavior"] != "deny" {
		t.Errorf("expected deny, got %v", decision["behavior"])
	}
	if decision["message"] != "not allowed by test" {
		t.Errorf("expected 'not allowed by test', got %v", decision["message"])
	}
}

func TestIntegration_Hook_PermissionRequest_AllowWithUpdatedInput(t *testing.T) {
	testServerURL.clearHandlers()
	testServerURL.setHandler("/request", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"behavior":"allow","updated_input":{"command":"echo safe"}}`)
	})
	defer testServerURL.clearHandlers()

	input := `{"hook_event_name":"PermissionRequest","tool_name":"Bash","tool_input":{"command":"rm -rf /"},"session_id":"s1"}`
	r := run(t, []string{"hook"},
		[]string{
			"GREENLIGHT_DEVICE_ID=test-dev",
			"GREENLIGHT_PROJECT=test-proj",
			"GREENLIGHT_SESSION_ID=relay-1",
		}, input)

	var output map[string]interface{}
	json.Unmarshal([]byte(r.Stdout), &output)
	hso := output["hookSpecificOutput"].(map[string]interface{})
	decision := hso["decision"].(map[string]interface{})
	if decision["behavior"] != "allow" {
		t.Errorf("expected allow, got %v", decision["behavior"])
	}
	updatedInput, ok := decision["updatedInput"].(map[string]interface{})
	if !ok {
		t.Fatalf("expected updatedInput map, got %v", decision["updatedInput"])
	}
	if updatedInput["command"] != "echo safe" {
		t.Errorf("expected updated command='echo safe', got %v", updatedInput["command"])
	}
}

func TestIntegration_Hook_PermissionRequest_DenyWithInterrupt(t *testing.T) {
	testServerURL.clearHandlers()
	testServerURL.setHandler("/request", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"behavior":"deny","message":"interrupted","interrupt":true}`)
	})
	defer testServerURL.clearHandlers()

	input := `{"hook_event_name":"PermissionRequest","tool_name":"Bash","session_id":"s1"}`
	r := run(t, []string{"hook"},
		[]string{
			"GREENLIGHT_DEVICE_ID=test-dev",
			"GREENLIGHT_PROJECT=test-proj",
			"GREENLIGHT_SESSION_ID=relay-1",
		}, input)

	var output map[string]interface{}
	json.Unmarshal([]byte(r.Stdout), &output)
	hso := output["hookSpecificOutput"].(map[string]interface{})
	decision := hso["decision"].(map[string]interface{})
	if decision["behavior"] != "deny" {
		t.Errorf("expected deny, got %v", decision["behavior"])
	}
	if decision["interrupt"] != true {
		t.Errorf("expected interrupt=true, got %v", decision["interrupt"])
	}
}

func TestIntegration_Hook_PermissionRequest_401Retry(t *testing.T) {
	testServerURL.clearHandlers()

	var requestCount int
	var requestMu sync.Mutex
	testServerURL.setHandler("/request", func(w http.ResponseWriter, r *http.Request) {
		requestMu.Lock()
		requestCount++
		count := requestCount
		requestMu.Unlock()

		if count == 1 {
			w.WriteHeader(401)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"behavior":"allow"}`)
	})
	defer testServerURL.clearHandlers()

	// Clear any enrollment markers from previous tests
	relayID := "retry-relay-1"
	os.Remove(filepath.Join(os.TempDir(), "greenlight-enrolled-"+relayID))

	input := `{"hook_event_name":"PermissionRequest","tool_name":"Bash","session_id":"s1"}`
	r := run(t, []string{"hook"},
		[]string{
			"GREENLIGHT_DEVICE_ID=test-dev",
			"GREENLIGHT_PROJECT=test-proj",
			"GREENLIGHT_SESSION_ID=" + relayID,
		}, input)

	var output map[string]interface{}
	json.Unmarshal([]byte(r.Stdout), &output)
	hso := output["hookSpecificOutput"].(map[string]interface{})
	decision := hso["decision"].(map[string]interface{})
	if decision["behavior"] != "allow" {
		t.Errorf("expected allow after retry, got %v; stdout=%q", decision["behavior"], r.Stdout)
	}

	// Should have made 2 requests to /request
	requestMu.Lock()
	if requestCount != 2 {
		t.Errorf("expected 2 requests to /request, got %d", requestCount)
	}
	requestMu.Unlock()
}

func TestIntegration_Hook_PermissionRequest_ServerError(t *testing.T) {
	testServerURL.clearHandlers()
	testServerURL.setHandler("/request", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(500)
		fmt.Fprint(w, "internal server error")
	})
	defer testServerURL.clearHandlers()

	input := `{"hook_event_name":"PermissionRequest","tool_name":"Bash","session_id":"s1"}`
	r := run(t, []string{"hook"},
		[]string{
			"GREENLIGHT_DEVICE_ID=test-dev",
			"GREENLIGHT_PROJECT=test-proj",
			"GREENLIGHT_SESSION_ID=relay-1",
		}, input)

	var output map[string]interface{}
	json.Unmarshal([]byte(r.Stdout), &output)
	hso := output["hookSpecificOutput"].(map[string]interface{})
	decision := hso["decision"].(map[string]interface{})
	if decision["behavior"] != "deny" {
		t.Errorf("expected deny on server error, got %v", decision["behavior"])
	}
	msg := decision["message"].(string)
	if !strings.Contains(msg, "500") {
		t.Errorf("expected HTTP 500 in message, got %q", msg)
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

// ---------- hook — unknown event ----------

func TestIntegration_Hook_UnknownEvent(t *testing.T) {
	input := `{"hook_event_name":"SomeUnknownEvent"}`
	r := run(t, []string{"hook"},
		[]string{
			"GREENLIGHT_DEVICE_ID=test-dev",
			"GREENLIGHT_PROJECT=test-proj",
		}, input)

	if r.ExitCode != 0 {
		t.Errorf("expected exit 0 for unknown event, got %d", r.ExitCode)
	}
}

// ---------- hook — invalid JSON ----------

func TestIntegration_Hook_InvalidJSON(t *testing.T) {
	r := run(t, []string{"hook"},
		[]string{
			"GREENLIGHT_DEVICE_ID=test-dev",
			"GREENLIGHT_PROJECT=test-proj",
		}, "this is not json")

	// Should output deny JSON
	var output map[string]interface{}
	if err := json.Unmarshal([]byte(r.Stdout), &output); err != nil {
		t.Fatalf("expected JSON output, got %q", r.Stdout)
	}
	hso := output["hookSpecificOutput"].(map[string]interface{})
	decision := hso["decision"].(map[string]interface{})
	if decision["behavior"] != "deny" {
		t.Errorf("expected deny for invalid JSON, got %v", decision["behavior"])
	}
}

// ---------- hook — default event type ----------

func TestIntegration_Hook_DefaultEventType(t *testing.T) {
	testServerURL.clearHandlers()
	testServerURL.setHandler("/request", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"behavior":"allow"}`)
	})
	defer testServerURL.clearHandlers()

	// Empty hook_event_name should default to PermissionRequest
	input := `{"tool_name":"Bash","session_id":"s1"}`
	r := run(t, []string{"hook"},
		[]string{
			"GREENLIGHT_DEVICE_ID=test-dev",
			"GREENLIGHT_PROJECT=test-proj",
			"GREENLIGHT_SESSION_ID=relay-1",
		}, input)

	var output map[string]interface{}
	json.Unmarshal([]byte(r.Stdout), &output)
	hso := output["hookSpecificOutput"].(map[string]interface{})
	decision := hso["decision"].(map[string]interface{})
	if decision["behavior"] != "allow" {
		t.Errorf("expected allow (default PermissionRequest), got %v; stdout=%q", decision["behavior"], r.Stdout)
	}
}
