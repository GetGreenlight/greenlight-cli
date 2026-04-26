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
	"runtime"
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
		// WebSocket upgrade for /ws/relay or /ws/daemon
		if r.URL.Path == "/ws/relay" || r.URL.Path == "/ws/daemon" {
			ts.mu.Lock()
			wsh := ts.wsHandlerFn
			ts.mu.Unlock()
			if wsh != nil {
				wsh(w, r)
				return
			}
			// Default: accept the upgrade, ACK any session_start messages
			// (so newSession in the daemon doesn't time out), and read
			// until the client closes.
			conn, err := websocket.Accept(w, r, &websocket.AcceptOptions{
				InsecureSkipVerify: true,
			})
			if err != nil {
				return
			}
			defer conn.Close(websocket.StatusNormalClosure, "done")
			ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
			defer cancel()
			for {
				msgType, data, err := conn.Read(ctx)
				if err != nil {
					return
				}
				if msgType != websocket.MessageText {
					continue
				}
				var msg struct {
					Type    string `json:"type"`
					RelayID string `json:"relay_id"`
				}
				if json.Unmarshal(data, &msg) != nil {
					continue
				}
				if msg.Type == "session_start" {
					ack, _ := json.Marshal(map[string]interface{}{
						"type":     "session_started",
						"relay_id": msg.RelayID,
					})
					conn.Write(ctx, websocket.MessageText, ack)
				}
			}
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
	resp, err := http.Post(testServerURL.baseURL()+"/session/enroll", "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatalf("enroll: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("enroll: status %d", resp.StatusCode)
	}
	return hostID
}
