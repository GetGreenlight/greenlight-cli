//go:build integration

package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

// startTestDaemon starts a daemon process with a unique socket path so it
// doesn't conflict with other tests or a real daemon. Returns the socket path,
// a TMPDIR for the test, and a cleanup function.
func startTestDaemon(t *testing.T, extraEnv ...string) (sockPath, tmpDir string, cleanup func()) {
	t.Helper()

	home := t.TempDir()
	tmpDir = t.TempDir()

	// Use a short, unique socket path (Unix sockets limited to ~104 bytes)
	sockPath = fmt.Sprintf("/tmp/gl-test-%d.sock", int64(os.Getpid())^time.Now().UnixNano())

	cmd := exec.Command(greenlightBin, "daemon", "start", "--foreground")
	env := []string{
		"HOME=" + home,
		"PATH=" + os.Getenv("PATH"),
		"TMPDIR=" + tmpDir,
		"GREENLIGHT_DAEMON_SOCK=" + sockPath,
	}
	env = append(env, extraEnv...)
	cmd.Env = env
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	var stderr bytes.Buffer
	cmd.Stderr = &stderr

	if err := cmd.Start(); err != nil {
		t.Fatalf("start daemon: %v", err)
	}

	daemonLogPath := filepath.Join(home, ".greenlight", "daemon.log")

	cleanup = func() {
		cmd.Process.Signal(syscall.SIGTERM)
		done := make(chan error, 1)
		go func() { done <- cmd.Wait() }()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			cmd.Process.Kill()
			cmd.Wait()
		}
		os.Remove(sockPath)
		// Print daemon log for debugging failed tests
		if t.Failed() {
			t.Logf("daemon log:\n%s", readFileOrEmpty(daemonLogPath))
		}
	}

	if !waitForSocket(t, sockPath, 5*time.Second) {
		cleanup()
		t.Fatalf("daemon socket did not appear; stderr=%q; log=%q",
			stderr.String(), readFileOrEmpty(daemonLogPath))
	}

	return sockPath, tmpDir, cleanup
}

// ---------- daemon start/stop/status ----------

func TestIntegration_Daemon_StartStop(t *testing.T) {
	sockPath, _, cleanup := startTestDaemon(t)
	defer cleanup()

	// Check status via IPC
	resp := daemonIPC(t, sockPath, ipcRequest{Type: "status"})
	if resp.Type != "status_response" {
		t.Errorf("expected status_response, got %q", resp.Type)
	}
	if len(resp.Sessions) != 0 {
		t.Errorf("expected 0 sessions, got %d", len(resp.Sessions))
	}

	// Stop via IPC
	resp = daemonIPC(t, sockPath, ipcRequest{Type: "stop"})
	if resp.Type != "ok" {
		t.Errorf("expected ok, got %q", resp.Type)
	}
}

func TestIntegration_Daemon_StatusNotRunning(t *testing.T) {
	r := run(t, []string{"daemon", "status"}, []string{
		"HOME=" + t.TempDir(),
		"GREENLIGHT_DAEMON_SOCK=/tmp/gl-test-nonexistent.sock",
	}, "")
	if !strings.Contains(r.Stderr, "not running") {
		t.Errorf("expected 'not running', got stderr=%q", r.Stderr)
	}
}

func TestIntegration_Daemon_StopNotRunning(t *testing.T) {
	r := run(t, []string{"daemon", "stop"}, []string{
		"HOME=" + t.TempDir(),
		"GREENLIGHT_DAEMON_SOCK=/tmp/gl-test-nonexistent.sock",
	}, "")
	if !strings.Contains(r.Stderr, "not running") {
		t.Errorf("expected 'not running', got stderr=%q", r.Stderr)
	}
}

// ---------- daemon connect flow ----------

func TestIntegration_Daemon_ConnectFlow(t *testing.T) {
	testServerURL.clearHandlers()

	sockPath, tmpDir, cleanup := startTestDaemon(t)
	defer cleanup()

	workDir := t.TempDir()
	pathWithMock := filepath.Dir(mockClaudeBin) + ":" + os.Getenv("PATH")

	// Allocate a PTY for the client so raw mode works
	master, slave, err := openPTY()
	if err != nil {
		t.Fatalf("openPTY: %v", err)
	}
	defer master.Close()
	setWinsize(slave.Fd(), &Winsize{Row: 24, Col: 80})

	// Run connect (which will talk to the daemon)
	client := exec.Command(greenlightBin, "connect",
		"--device-id", "test-dev",
		"--project", "test-proj",
	)
	client.Dir = workDir
	client.Env = []string{
		"HOME=" + t.TempDir(),
		"PATH=" + pathWithMock,
		"TMPDIR=" + tmpDir,
		"TERM=xterm-256color",
		"GREENLIGHT_DAEMON_SOCK=" + sockPath,
	}
	var clientStderr bytes.Buffer
	client.Stdin = slave
	client.Stdout = slave
	client.Stderr = &clientStderr

	done := make(chan error, 1)
	if err := client.Start(); err != nil {
		t.Fatalf("start client: %v", err)
	}
	slave.Close()
	go func() { done <- client.Wait() }()

	// Wait for client to exit — mock claude exits quickly.
	// The client writes to its stderr (captured) and PTY (master).
	select {
	case err := <-done:
		t.Logf("client exited: err=%v stderr=%q", err, clientStderr.String())
	case <-time.After(10 * time.Second):
		client.Process.Kill()
		t.Fatalf("client timed out; stderr=%q", clientStderr.String())
	}

	// Read whatever output came through the PTY
	master.SetReadDeadline(time.Now().Add(1 * time.Second))
	var ptyBuf bytes.Buffer
	tmp := make([]byte, 4096)
	for {
		n, err := master.Read(tmp)
		if n > 0 {
			ptyBuf.Write(tmp[:n])
		}
		if err != nil {
			break
		}
	}
	output := ptyBuf.String()
	t.Logf("PTY output: %q", output)

	// Verify enrollment happened
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

}

// ---------- daemon connect with input injection ----------

func TestIntegration_Daemon_InputInjection(t *testing.T) {
	testServerURL.clearHandlers()

	sockPath, tmpDir, cleanup := startTestDaemon(t)
	defer cleanup()

	workDir := t.TempDir()
	outputFile := filepath.Join(workDir, "claude-received.txt")
	pathWithMock := filepath.Dir(mockClaudeBin) + ":" + os.Getenv("PATH")

	master, slave, err := openPTY()
	if err != nil {
		t.Fatalf("openPTY: %v", err)
	}
	defer master.Close()
	setWinsize(slave.Fd(), &Winsize{Row: 24, Col: 80})

	client := exec.Command(greenlightBin, "connect",
		"--device-id", "test-dev",
		"--project", "test-proj",
	)
	client.Dir = workDir
	client.Env = []string{
		"HOME=" + t.TempDir(),
		"PATH=" + pathWithMock,
		"TMPDIR=" + tmpDir,
		"TERM=xterm-256color",
		"MOCK_CLAUDE_OUTPUT=" + outputFile,
		"GREENLIGHT_DAEMON_SOCK=" + sockPath,
	}
	client.Stdin = slave
	client.Stdout = slave
	client.Stderr = slave

	done := make(chan error, 1)
	if err := client.Start(); err != nil {
		t.Fatalf("start client: %v", err)
	}
	slave.Close()
	go func() { done <- client.Wait() }()

	// Wait for mock claude to start
	readPTYUntil(t, master, "MOCK_CLAUDE_STARTED", 10*time.Second)

	// Send input through the PTY master — this goes to the client,
	// which forwards it to the daemon, which writes to the agent PTY
	time.Sleep(500 * time.Millisecond)
	if _, err := master.Write([]byte("DAEMON_INPUT_TEST\n")); err != nil {
		t.Fatalf("write to PTY: %v", err)
	}

	// Wait for client to exit
	select {
	case <-done:
	case <-time.After(15 * time.Second):
		client.Process.Kill()
		t.Fatal("client timed out")
	}

	// Verify mock claude received the input
	data, err := os.ReadFile(outputFile)
	if err != nil {
		t.Fatalf("mock claude output not created: %v", err)
	}
	if !strings.Contains(string(data), "DAEMON_INPUT_TEST") {
		t.Errorf("expected DAEMON_INPUT_TEST, got %q", string(data))
	}

}

// ---------- daemon connect error handling ----------

func TestIntegration_Daemon_ConnectError(t *testing.T) {
	testServerURL.clearHandlers()

	sockPath, _, cleanup := startTestDaemon(t)
	defer cleanup()

	// Send a connect request with no device ID — should get an error response
	resp := daemonIPC(t, sockPath, ipcRequest{
		Type: "connect",
		Cwd:  t.TempDir(),
	})

	if resp.Type != "error" {
		t.Errorf("expected error response, got %q", resp.Type)
	}
	if resp.Message == "" {
		t.Error("expected non-empty error message")
	}

	// Verify daemon is still healthy after the error
	statusResp := daemonIPC(t, sockPath, ipcRequest{Type: "status"})
	if statusResp.Type != "status_response" {
		t.Errorf("daemon unhealthy after error: got %q", statusResp.Type)
	}
}

// ---------- helpers ----------

// waitForSocket waits for a Unix socket to become connectable.
func waitForSocket(t *testing.T, path string, timeout time.Duration) bool {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		conn, err := net.DialTimeout("unix", path, 500*time.Millisecond)
		if err == nil {
			conn.Close()
			return true
		}
		time.Sleep(100 * time.Millisecond)
	}
	return false
}

// daemonIPC sends a control message to the daemon and returns the response.
func daemonIPC(t *testing.T, sockPath string, req ipcRequest) ipcResponse {
	t.Helper()

	conn, err := net.DialTimeout("unix", sockPath, 2*time.Second)
	if err != nil {
		t.Fatalf("dial daemon: %v", err)
	}
	defer conn.Close()

	data, _ := json.Marshal(req)
	data = append(data, '\n')
	conn.SetWriteDeadline(time.Now().Add(2 * time.Second))
	if _, err := conn.Write(data); err != nil {
		t.Fatalf("write to daemon: %v", err)
	}

	conn.SetReadDeadline(time.Now().Add(30 * time.Second))
	reader := bufio.NewReader(conn)
	line, err := reader.ReadBytes('\n')
	if err != nil {
		t.Fatalf("read from daemon: %v", err)
	}

	var resp ipcResponse
	if err := json.Unmarshal(line, &resp); err != nil {
		t.Fatalf("parse daemon response: %v (raw: %s)", err, string(line))
	}
	return resp
}

// readFileOrEmpty reads a file and returns its contents, or empty string on error.
func readFileOrEmpty(path string) string {
	data, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	return string(data)
}

// readPTYUntil reads from a PTY master until the output contains the target
// string or the timeout expires.
func readPTYUntil(t *testing.T, master *os.File, target string, timeout time.Duration) string {
	t.Helper()

	var buf bytes.Buffer
	deadline := time.Now().Add(timeout)
	tmp := make([]byte, 4096)

	for time.Now().Before(deadline) {
		master.SetReadDeadline(time.Now().Add(500 * time.Millisecond))
		n, err := master.Read(tmp)
		if n > 0 {
			buf.Write(tmp[:n])
			if strings.Contains(buf.String(), target) {
				return buf.String()
			}
		}
		if err != nil && !os.IsTimeout(err) {
			break
		}
	}
	return buf.String()
}
