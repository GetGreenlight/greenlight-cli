//go:build integration

package main

import (
	"bufio"
	"bytes"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"testing"
	"time"

	"greenlight/internal/mockserver"
)

// startTestDaemon starts a daemon process with a unique socket path so it
// doesn't conflict with other tests or a real daemon. Returns the socket path,
// a TMPDIR for the test, and a cleanup function.
//
// If a deviceID is provided (via the GREENLIGHT_DEVICE_ID env override in
// extraEnv), the test should pre-enroll a host with the test server and pass
// GREENLIGHT_DAEMON_SESSION_ID so the daemon's WebSocket can connect.
func startTestDaemon(t *testing.T, extraEnv ...string) (sockPath, tmpDir string, cleanup func()) {
	sockPath, tmpDir, _, cleanup = startTestDaemonWithHome(t, t.TempDir(), extraEnv...)
	return
}

// startTestDaemonWithHome is like startTestDaemon but lets the caller
// supply the daemon's HOME so it can pre-populate paths the daemon will
// scan (e.g. ~/.claude/projects/... for transcript tests).
func startTestDaemonWithHome(t *testing.T, home string, extraEnv ...string) (sockPath, tmpDir, daemonHome string, cleanup func()) {
	t.Helper()
	daemonHome = home
	tmpDir = t.TempDir()

	// Use a short, unique socket path (Unix sockets limited to ~104 bytes)
	sockPath = fmt.Sprintf("/tmp/gl-test-%d.sock", int64(os.Getpid())^time.Now().UnixNano())

	cmd := exec.Command(greenlightBin, "daemon", "start", "--foreground")
	// Put mock_claude's directory at the front of PATH so the daemon
	// resolves it (rather than a real claude on the dev machine) when it
	// looks up the agent binary for entitlement checks. The client's
	// PATH already prefers mock_claude, but the daemon does its own
	// resolution before the spawn.
	daemonPath := filepath.Dir(mockClaudeBin) + ":" + os.Getenv("PATH")
	env := []string{
		"HOME=" + home,
		"PATH=" + daemonPath,
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

	return
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

func TestIntegration_Daemon_Restart(t *testing.T) {
	testServerURL.ClearHandlers()

	// Pre-enroll a host so the initial daemon's WebSocket can connect.
	hostID := enrollTestHost(t, "test-dev")

	// Start an initial daemon bound to a unique socket.
	sockPath, tmpDir, daemonHome, cleanup := startTestDaemonWithHome(t, t.TempDir(),
		"GREENLIGHT_DEVICE_ID=test-dev",
		"GREENLIGHT_DAEMON_SESSION_ID="+hostID,
	)
	defer cleanup()

	pidPath := filepath.Join(daemonHome, ".greenlight", "daemon.pid")
	origPID := strings.TrimSpace(readFileOrEmpty(pidPath))
	if origPID == "" {
		t.Fatal("expected initial daemon to write a pid file")
	}

	// Restart stops the running daemon and starts a fresh one on the same socket.
	r := run(t, []string{"daemon", "restart", "--device-id", "test-dev"}, []string{
		"HOME=" + daemonHome,
		"TMPDIR=" + tmpDir,
		"GREENLIGHT_DAEMON_SOCK=" + sockPath,
		"GREENLIGHT_DEVICE_ID=test-dev",
	}, "")
	if !strings.Contains(r.Stderr, "daemon restarted") {
		t.Fatalf("expected 'daemon restarted', got stderr=%q exit=%d", r.Stderr, r.ExitCode)
	}

	// The new daemon should answer on the same socket.
	if !waitForSocket(t, sockPath, 5*time.Second) {
		t.Fatal("daemon socket did not reappear after restart")
	}
	resp := daemonIPC(t, sockPath, ipcRequest{Type: "status"})
	if resp.Type != "status_response" {
		t.Errorf("expected status_response after restart, got %q", resp.Type)
	}

	// ...and it should be a different process than the original.
	newPID := strings.TrimSpace(readFileOrEmpty(pidPath))
	if newPID == "" || newPID == origPID {
		t.Errorf("expected a new daemon pid after restart, orig=%q new=%q", origPID, newPID)
	}

	// Stop the restarted (detached) daemon so it doesn't linger past the test.
	daemonIPC(t, sockPath, ipcRequest{Type: "stop"})
}

// TestIntegration_Daemon_StopWedged verifies that `daemon stop` doesn't hang
// forever against a wedged daemon — one that accepts the connection but never
// replies. The client reads carry a deadline, so the command must return.
func TestIntegration_Daemon_StopWedged(t *testing.T) {
	sockPath := fmt.Sprintf("/tmp/gl-test-wedged-%d.sock", int64(os.Getpid())^time.Now().UnixNano())
	ln, err := net.Listen("unix", sockPath)
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer ln.Close()
	defer os.Remove(sockPath)

	// Accept connections and hold them open without ever replying.
	go func() {
		var conns []net.Conn
		defer func() {
			for _, c := range conns {
				c.Close()
			}
		}()
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			conns = append(conns, conn)
		}
	}()

	start := time.Now()
	r := runWithTimeout(t, []string{"daemon", "stop", "--force"}, []string{
		"HOME=" + t.TempDir(),
		"GREENLIGHT_DAEMON_SOCK=" + sockPath,
	}, "", 20*time.Second)
	elapsed := time.Since(start)

	// It should give up on the unresponsive daemon (best-effort stop), not hang.
	if !strings.Contains(r.Stderr, "daemon stopped") {
		t.Errorf("expected 'daemon stopped', got stderr=%q", r.Stderr)
	}
	if elapsed > 15*time.Second {
		t.Errorf("stop took too long against wedged daemon: %v", elapsed)
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
	testServerURL.ClearHandlers()

	// Pre-enroll a host so the daemon's WebSocket can connect with a
	// known device_id / session_id pair.
	hostID := enrollTestHost(t, "test-dev")

	sockPath, tmpDir, cleanup := startTestDaemon(t,
		"GREENLIGHT_DEVICE_ID=test-dev",
		"GREENLIGHT_DAEMON_SESSION_ID="+hostID,
		"GREENLIGHT_DISABLE_INTERPOSE=1",
	)
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
	clientLog := filepath.Join(tmpDir, "client.log")
	client.Env = []string{
		"HOME=" + t.TempDir(),
		"PATH=" + pathWithMock,
		"TMPDIR=" + tmpDir,
		"TERM=xterm-256color",
		"GREENLIGHT_DAEMON_SOCK=" + sockPath,
		"GREENLIGHT_LOG=" + clientLog,
	}
	var clientStderr bytes.Buffer
	client.Stdin = slave
	client.Stdout = slave
	client.Stderr = &clientStderr
	defer func() {
		if t.Failed() {
			t.Logf("client log:\n%s", readFileOrEmpty(clientLog))
		}
	}()

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

	// Verify host enrollment happened (test setup pre-enrolls so the daemon
	// WebSocket can connect). Per-session info travels over the daemon WS,
	// not via /session/enroll.
	enrollReqs := testServerURL.Requests("/session/enroll")
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
}

// ---------- daemon connect with input injection ----------

func TestIntegration_Daemon_InputInjection(t *testing.T) {
	testServerURL.ClearHandlers()

	hostID := enrollTestHost(t, "test-dev")

	sockPath, tmpDir, cleanup := startTestDaemon(t,
		"GREENLIGHT_DEVICE_ID=test-dev",
		"GREENLIGHT_DAEMON_SESSION_ID="+hostID,
		"GREENLIGHT_DISABLE_INTERPOSE=1",
	)
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
	clientLog := filepath.Join(tmpDir, "client.log")
	client.Env = []string{
		"HOME=" + t.TempDir(),
		"PATH=" + pathWithMock,
		"TMPDIR=" + tmpDir,
		"TERM=xterm-256color",
		"MOCK_CLAUDE_OUTPUT=" + outputFile,
		"GREENLIGHT_DAEMON_SOCK=" + sockPath,
		"GREENLIGHT_LOG=" + clientLog,
	}
	client.Stdin = slave
	client.Stdout = slave
	client.Stderr = slave
	defer func() {
		if t.Failed() {
			t.Logf("client log:\n%s", readFileOrEmpty(clientLog))
		}
	}()

	done := make(chan error, 1)
	if err := client.Start(); err != nil {
		t.Fatalf("start client: %v", err)
	}
	slave.Close()
	go func() { done <- client.Wait() }()

	// Wait for mock claude to start
	got := readPTYUntil(t, master, "MOCK_CLAUDE_STARTED", 10*time.Second)
	if !strings.Contains(got, "MOCK_CLAUDE_STARTED") {
		// Client may have exited early — surface that and bail rather than
		// write to a closed PTY.
		select {
		case err := <-done:
			t.Fatalf("client exited before mock claude started: err=%v pty=%q", err, got)
		default:
			t.Fatalf("mock claude did not start within 10s, pty=%q", got)
		}
	}

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

// ---------- autopilot initial-prompt injection (#145) ----------

// TestIntegration_Daemon_InitialPromptInjection verifies that an autopilot
// stage session (launched with GREENLIGHT_INITIAL_PROMPT set) reliably injects
// AND submits the stage prompt as the first user message — without any human
// typing. mock_claude's readStdinToFile uses a bufio.Scanner that only returns
// a line once a terminator arrives, so a successful read proves both halves:
// the prompt text was injected and a separate submit (\r) followed it. If the
// prompt were injected without a submit, the scanner would block and record
// "TIMEOUT" instead. This is the autopilot analog of the relay_input injection
// path (both now go through submitInput).
func TestIntegration_Daemon_InitialPromptInjection(t *testing.T) {
	testServerURL.ClearHandlers()

	hostID := enrollTestHost(t, "test-dev")

	sockPath, tmpDir, cleanup := startTestDaemon(t,
		"GREENLIGHT_DEVICE_ID=test-dev",
		"GREENLIGHT_DAEMON_SESSION_ID="+hostID,
		"GREENLIGHT_DISABLE_INTERPOSE=1",
	)
	defer cleanup()

	workDir := t.TempDir()
	outputFile := filepath.Join(workDir, "claude-received.txt")
	pathWithMock := filepath.Dir(mockClaudeBin) + ":" + os.Getenv("PATH")

	const initialPrompt = "AUTOPILOT_PROMPT_TEST"

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
	clientLog := filepath.Join(tmpDir, "client.log")
	client.Env = []string{
		"HOME=" + t.TempDir(),
		"PATH=" + pathWithMock,
		"TMPDIR=" + tmpDir,
		"TERM=xterm-256color",
		"MOCK_CLAUDE_OUTPUT=" + outputFile,
		"GREENLIGHT_DAEMON_SOCK=" + sockPath,
		"GREENLIGHT_LOG=" + clientLog,
		// Drives Session.injectInitialPrompt — no human keystrokes are sent.
		"GREENLIGHT_INITIAL_PROMPT=" + initialPrompt,
	}
	client.Stdin = slave
	client.Stdout = slave
	client.Stderr = slave
	defer func() {
		if t.Failed() {
			t.Logf("client log:\n%s", readFileOrEmpty(clientLog))
		}
	}()

	done := make(chan error, 1)
	if err := client.Start(); err != nil {
		t.Fatalf("start client: %v", err)
	}
	slave.Close()
	go func() { done <- client.Wait() }()

	// Drain PTY output so the master read loop keeps the agent's first output
	// flowing (which is what closes readyCh and releases the injection).
	go io.Copy(io.Discard, master)

	// Wait for the client to exit — mock_claude returns from readStdinToFile as
	// soon as it scans a full line, i.e. once the prompt + submit arrive.
	select {
	case <-done:
	case <-time.After(20 * time.Second):
		client.Process.Kill()
		t.Fatal("client timed out waiting for initial-prompt injection")
	}

	data, err := os.ReadFile(outputFile)
	if err != nil {
		t.Fatalf("mock claude output not created: %v", err)
	}
	if strings.Contains(string(data), "TIMEOUT") {
		t.Fatalf("initial prompt never submitted (mock claude saw no line terminator): %q", string(data))
	}
	if !strings.Contains(string(data), initialPrompt) {
		t.Errorf("expected initial prompt %q, got %q", initialPrompt, string(data))
	}
}

// TestIntegration_Daemon_InitialPromptFileInjection is the file-handoff (#4)
// analog of the test above: the prompt is delivered via a $TMPDIR temp file
// referenced by GREENLIGHT_INITIAL_PROMPT_FILE (the path the daemon types into
// the spawn command), not the inline var. It asserts the prompt is injected AND
// submitted (same mock_claude scanner proof) AND that the daemon unlinks the
// file after consuming it. The prompt deliberately carries the special chars
// that broke inline quoting (apostrophe, backtick, em-dash, parens).
func TestIntegration_Daemon_InitialPromptFileInjection(t *testing.T) {
	testServerURL.ClearHandlers()

	hostID := enrollTestHost(t, "test-dev")

	sockPath, tmpDir, cleanup := startTestDaemon(t,
		"GREENLIGHT_DEVICE_ID=test-dev",
		"GREENLIGHT_DAEMON_SESSION_ID="+hostID,
		"GREENLIGHT_DISABLE_INTERPOSE=1",
	)
	defer cleanup()

	workDir := t.TempDir()
	outputFile := filepath.Join(workDir, "claude-received.txt")
	pathWithMock := filepath.Dir(mockClaudeBin) + ":" + os.Getenv("PATH")

	// A single line (mock_claude scans line-by-line) carrying the quoting-hostile
	// classes from #4. No newline so the whole thing is one injected message.
	const initialPrompt = "AUTOPILOT_FILE_TEST don't `run` it — check (a,b)"
	promptFile := filepath.Join(workDir, "stage-prompt.txt")
	if err := os.WriteFile(promptFile, []byte(initialPrompt), 0644); err != nil {
		t.Fatalf("write prompt file: %v", err)
	}

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
	clientLog := filepath.Join(tmpDir, "client.log")
	client.Env = []string{
		"HOME=" + t.TempDir(),
		"PATH=" + pathWithMock,
		"TMPDIR=" + tmpDir,
		"TERM=xterm-256color",
		"MOCK_CLAUDE_OUTPUT=" + outputFile,
		"GREENLIGHT_DAEMON_SOCK=" + sockPath,
		"GREENLIGHT_LOG=" + clientLog,
		// File handoff (#4): only the path is in the env; the daemon reads it.
		"GREENLIGHT_INITIAL_PROMPT_FILE=" + promptFile,
	}
	client.Stdin = slave
	client.Stdout = slave
	client.Stderr = slave
	defer func() {
		if t.Failed() {
			t.Logf("client log:\n%s", readFileOrEmpty(clientLog))
		}
	}()

	done := make(chan error, 1)
	if err := client.Start(); err != nil {
		t.Fatalf("start client: %v", err)
	}
	slave.Close()
	go func() { done <- client.Wait() }()

	go io.Copy(io.Discard, master)

	select {
	case <-done:
	case <-time.After(20 * time.Second):
		client.Process.Kill()
		t.Fatal("client timed out waiting for initial-prompt-file injection")
	}

	data, err := os.ReadFile(outputFile)
	if err != nil {
		t.Fatalf("mock claude output not created: %v", err)
	}
	if strings.Contains(string(data), "TIMEOUT") {
		t.Fatalf("initial prompt never submitted (mock claude saw no line terminator): %q", string(data))
	}
	if !strings.Contains(string(data), initialPrompt) {
		t.Errorf("expected initial prompt %q, got %q", initialPrompt, string(data))
	}
	// The daemon must unlink the prompt file once it has read it.
	if _, err := os.Stat(promptFile); !os.IsNotExist(err) {
		t.Errorf("prompt file %q was not unlinked after consumption (err=%v)", promptFile, err)
	}
}

// ---------- daemon connect error handling ----------

func TestIntegration_Daemon_ConnectError(t *testing.T) {
	testServerURL.ClearHandlers()

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

// TestIntegration_Daemon_ListSkills exercises the full list_skills control
// frame round-trip: server pushes list_skills over /ws/daemon, the daemon
// scans the session's cwd for installed skills, and replies with
// skills_listed. This is the canonical template for any test that needs
// to drive the daemon via a server-originated control frame.
func TestIntegration_Daemon_ListSkills(t *testing.T) {
	testServerURL.ClearHandlers()
	cs, cleanup := startConnectSession(t)
	defer cleanup()

	// Pre-install a skill into the session's cwd so listSkills finds it.
	writeTestSkill(t, cs.Workdir, "demo-skill")

	if err := cs.Sess.SendBinary(map[string]any{
		"type":     "list_skills",
		"relay_id": cs.Sess.RelayID,
	}); err != nil {
		t.Fatalf("send list_skills: %v", err)
	}

	reply := awaitSkillsListed(t, cs.Sess, 5*time.Second)
	if reply.RelayID != cs.Sess.RelayID {
		t.Errorf("relay_id mismatch: got %q want %q", reply.RelayID, cs.Sess.RelayID)
	}
	if len(reply.Skills) != 1 || reply.Skills[0] != "demo-skill" {
		t.Errorf("skills mismatch: got %v want [demo-skill]", reply.Skills)
	}

	cs.Wait(10 * time.Second)
}

// TestIntegration_Daemon_SuggestionGet exercises the composer suggestion tap
// (#38) end to end: the daemon maintains a VT screen model from the agent's PTY
// output; mock_claude paints a fake composer line with an italic ghost
// suggestion; the server pushes a suggestion_get control frame; the daemon greps
// the composer line and replies suggested_prompt with the extracted text.
func TestIntegration_Daemon_SuggestionGet(t *testing.T) {
	testServerURL.ClearHandlers()
	// Paint: clear screen, position to row 10, draw the ❯ prompt, then the
	// ghost suggestion in italic (SGR 3) — which vt10x tracks.
	composer := `\033[2J\033[10;1H❯ \033[3mTry \"add a test for the parser\"\033[0m`
	cs, cleanup := startConnectSession(t, connectOpts{
		AgentEnv: []string{"MOCK_CLAUDE_RAW=" + composer},
	})
	defer cleanup()

	// Deliver exactly as the production server does: a text frame wrapping
	// {"type":"control","data":base64({"type":"suggestion_get"})}, which the CLI
	// decodes and routes via handleTextFrame's "control" branch (never the PTY
	// injection path). This validates the real wire path end to end.
	if err := cs.Sess.Send(map[string]any{
		"relay_id": cs.Sess.RelayID,
		"type":     "control",
		"data":     base64.StdEncoding.EncodeToString([]byte(`{"type":"suggestion_get"}`)),
	}); err != nil {
		t.Fatalf("send suggestion_get: %v", err)
	}

	reply := awaitSuggestedPrompt(t, cs.Sess, 5*time.Second)
	if reply.RelayID != cs.Sess.RelayID {
		t.Errorf("relay_id mismatch: got %q want %q", reply.RelayID, cs.Sess.RelayID)
	}
	if reply.Text != `Try "add a test for the parser"` {
		t.Errorf("suggestion mismatch: got %q", reply.Text)
	}

	cs.Wait(10 * time.Second)
}

// TestIntegration_Daemon_SuggestionGet_NoComposer confirms that when no composer
// suggestion is on screen (no marker / ghost text painted), the daemon still
// replies — with empty text — so a caller is never left waiting. The screen tap
// is always on now; an empty reply means "nothing to extract", not "feature off".
func TestIntegration_Daemon_SuggestionGet_NoComposer(t *testing.T) {
	testServerURL.ClearHandlers()
	cs, cleanup := startConnectSession(t)
	defer cleanup()

	if err := cs.Sess.SendBinary(map[string]any{
		"type":     "suggestion_get",
		"relay_id": cs.Sess.RelayID,
	}); err != nil {
		t.Fatalf("send suggestion_get: %v", err)
	}

	reply := awaitSuggestedPrompt(t, cs.Sess, 5*time.Second)
	if reply.Text != "" {
		t.Errorf("expected empty text with no composer on screen, got %q", reply.Text)
	}

	cs.Wait(10 * time.Second)
}

// TestIntegration_Daemon_Config exercises the config_get/config_set control
// frames end to end: the server pushes them over the daemon WS, the daemon
// applies/reads its config file, and replies config_set_result / config_loaded.
// Covers host vs project scope, project-overrides-host resolution, and the
// device_id / enum validation rejections.
func TestIntegration_Daemon_Config(t *testing.T) {
	testServerURL.ClearHandlers()
	cs, cleanup := startConnectSession(t)
	defer cleanup()

	// Host-scope batch: a known enum key + an arbitrary secret name.
	if err := cs.Sess.SendBinary(map[string]any{
		"type":       "config_set",
		"request_id": "c1",
		"scope":      "host",
		"set":        map[string]string{"agent": "codex", "tickets_secret": "HOST_TOK"},
	}); err != nil {
		t.Fatalf("send config_set host: %v", err)
	}
	if r := awaitConfigSetResult(t, cs.Sess, "c1", 5*time.Second); !r.Success {
		t.Fatalf("host config_set failed: error=%q", r.Error)
	}

	// Project-scope override of the same enum key.
	if err := cs.Sess.SendBinary(map[string]any{
		"type":       "config_set",
		"request_id": "c2",
		"scope":      "project",
		"project":    "test-proj",
		"set":        map[string]string{"agent": "gemini"},
	}); err != nil {
		t.Fatalf("send config_set project: %v", err)
	}
	if r := awaitConfigSetResult(t, cs.Sess, "c2", 5*time.Second); !r.Success {
		t.Fatalf("project config_set failed: error=%q", r.Error)
	}

	// config_get with a project returns both scopes; project overrides host.
	if err := cs.Sess.SendBinary(map[string]any{
		"type":       "config_get",
		"request_id": "c3",
		"project":    "test-proj",
	}); err != nil {
		t.Fatalf("send config_get: %v", err)
	}
	loaded := awaitConfigLoaded(t, cs.Sess, "c3", 5*time.Second)
	if loaded.Config.Host["agent"] != "codex" {
		t.Errorf("host.agent = %q, want codex", loaded.Config.Host["agent"])
	}
	if loaded.Config.Host["tickets_secret"] != "HOST_TOK" {
		t.Errorf("host.tickets_secret = %q, want HOST_TOK", loaded.Config.Host["tickets_secret"])
	}
	if loaded.Config.Host["device_id"] != "" {
		t.Errorf("device_id leaked into host config: %q", loaded.Config.Host["device_id"])
	}
	if loaded.Config.Project["agent"] != "gemini" {
		t.Errorf("project.agent = %q, want gemini", loaded.Config.Project["agent"])
	}

	// device_id is forbidden.
	cs.Sess.SendBinary(map[string]any{
		"type": "config_set", "request_id": "c4", "scope": "host",
		"set": map[string]string{"device_id": "evil"},
	})
	if r := awaitConfigSetResult(t, cs.Sess, "c4", 5*time.Second); r.Success || r.Error != "device_id_forbidden" {
		t.Errorf("device_id set: got success=%v error=%q, want device_id_forbidden", r.Success, r.Error)
	}

	// Unknown agent is rejected.
	cs.Sess.SendBinary(map[string]any{
		"type": "config_set", "request_id": "c5", "scope": "host",
		"set": map[string]string{"agent": "vim"},
	})
	if r := awaitConfigSetResult(t, cs.Sess, "c5", 5*time.Second); r.Success || r.Error != "invalid_agent" {
		t.Errorf("bad agent: got success=%v error=%q, want invalid_agent", r.Success, r.Error)
	}

	// Syntactically bad custom key is rejected.
	cs.Sess.SendBinary(map[string]any{
		"type": "config_set", "request_id": "c6", "scope": "host",
		"set": map[string]string{"bad key": "v"},
	})
	if r := awaitConfigSetResult(t, cs.Sess, "c6", 5*time.Second); r.Success || r.Error != "invalid_key" {
		t.Errorf("bad key: got success=%v error=%q, want invalid_key", r.Success, r.Error)
	}

	// project scope without a project name is rejected.
	cs.Sess.SendBinary(map[string]any{
		"type": "config_set", "request_id": "c7", "scope": "project",
		"set": map[string]string{"agent": "pi"},
	})
	if r := awaitConfigSetResult(t, cs.Sess, "c7", 5*time.Second); r.Success || r.Error != "missing_project" {
		t.Errorf("missing project: got success=%v error=%q, want missing_project", r.Success, r.Error)
	}

	// Unknown scope is rejected.
	cs.Sess.SendBinary(map[string]any{
		"type": "config_set", "request_id": "c8", "scope": "galaxy",
		"set": map[string]string{"agent": "pi"},
	})
	if r := awaitConfigSetResult(t, cs.Sess, "c8", 5*time.Second); r.Success || r.Error != "invalid_scope" {
		t.Errorf("bad scope: got success=%v error=%q, want invalid_scope", r.Success, r.Error)
	}

	// unset over the wire removes the project override set earlier (c2).
	cs.Sess.SendBinary(map[string]any{
		"type": "config_set", "request_id": "c9", "scope": "project", "project": "test-proj",
		"unset": []string{"agent"},
	})
	if r := awaitConfigSetResult(t, cs.Sess, "c9", 5*time.Second); !r.Success {
		t.Fatalf("unset failed: error=%q", r.Error)
	}

	// Host-only config_get (no project) returns the host scope and an empty
	// project map.
	cs.Sess.SendBinary(map[string]any{"type": "config_get", "request_id": "c10"})
	hostOnly := awaitConfigLoaded(t, cs.Sess, "c10", 5*time.Second)
	if hostOnly.Config.Host["agent"] != "codex" {
		t.Errorf("host-only get: host.agent = %q, want codex", hostOnly.Config.Host["agent"])
	}
	if len(hostOnly.Config.Project) != 0 {
		t.Errorf("host-only get: project map should be empty, got %v", hostOnly.Config.Project)
	}

	// And the project override is gone after the unset.
	cs.Sess.SendBinary(map[string]any{"type": "config_get", "request_id": "c11", "project": "test-proj"})
	afterUnset := awaitConfigLoaded(t, cs.Sess, "c11", 5*time.Second)
	if _, ok := afterUnset.Config.Project["agent"]; ok {
		t.Errorf("project override still present after unset: %v", afterUnset.Config.Project)
	}

	cs.Wait(10 * time.Second)
}

// TestIntegration_Daemon_ListSkills_Empty verifies the daemon replies
// with an empty array (not null) when no skills are installed. The
// distinction matters on the wire — clients distinguish "session has no
// skills" from "session unknown" by reply presence.
func TestIntegration_Daemon_ListSkills_Empty(t *testing.T) {
	testServerURL.ClearHandlers()
	cs, cleanup := startConnectSession(t)
	defer cleanup()

	if err := cs.Sess.SendBinary(map[string]any{
		"type":     "list_skills",
		"relay_id": cs.Sess.RelayID,
	}); err != nil {
		t.Fatalf("send list_skills: %v", err)
	}

	reply := awaitSkillsListed(t, cs.Sess, 5*time.Second)
	if reply.Skills == nil {
		t.Fatal("expected empty slice, got nil — wire form must be [] not null")
	}
	if len(reply.Skills) != 0 {
		t.Errorf("expected no skills, got %v", reply.Skills)
	}

	cs.Wait(10 * time.Second)
}

// TestIntegration_Daemon_SkillInstall verifies the daemon writes skills
// delivered via the session_started ack to disk and that listSkills
// finds them afterwards. Round-trip: SetSessionStartHook returns a skill
// in the ack → daemon installs → server queries via list_skills →
// reply contains the installed skill name.
func TestIntegration_Daemon_SkillInstall(t *testing.T) {
	testServerURL.ClearHandlers()
	testServerURL.SetSessionStartHook(func(relayID string, _ json.RawMessage) any {
		return map[string]any{
			"type":     "session_started",
			"relay_id": relayID,
			"skills": []map[string]any{
				{
					"name":     "delivered-skill",
					"skill_md": "---\nname: delivered-skill\ndescription: server-delivered\n---\nbody\n",
				},
			},
		}
	})

	cs, cleanup := startConnectSession(t)
	defer cleanup()

	// The skill should be on disk under the agent's skills root.
	skillMD := filepath.Join(cs.Workdir, ".claude", "skills", "_greenlight", "delivered-skill", "SKILL.md")
	if _, err := os.Stat(skillMD); err != nil {
		t.Fatalf("expected delivered skill on disk at %s: %v", skillMD, err)
	}

	// And listSkills should see it.
	if err := cs.Sess.SendBinary(map[string]any{
		"type":     "list_skills",
		"relay_id": cs.Sess.RelayID,
	}); err != nil {
		t.Fatalf("send list_skills: %v", err)
	}
	reply := awaitSkillsListed(t, cs.Sess, 5*time.Second)
	if len(reply.Skills) != 1 || reply.Skills[0] != "delivered-skill" {
		t.Errorf("skills mismatch: got %v want [delivered-skill]", reply.Skills)
	}

	cs.Wait(10 * time.Second)

	// And cleaned up after the session ends (no other sessions share the
	// workdir, so the cleanup guard allows removal).
	if _, err := os.Stat(skillMD); !os.IsNotExist(err) {
		t.Errorf("expected %s to be removed after session end, got err=%v", skillMD, err)
	}
}

// TestIntegration_Daemon_Kill verifies the server can terminate a
// running session by sending a kill control frame as a binary
// WebSocket message. The frame routes through the daemon's controlFunc
// → routeControlFrame → per-session killFunc, terminating the agent
// process group.
func TestIntegration_Daemon_Kill(t *testing.T) {
	testServerURL.ClearHandlers()
	cs, cleanup := startConnectSession(t)
	defer cleanup()

	if err := cs.Sess.SendBinary(map[string]any{
		"type":     "kill",
		"relay_id": cs.Sess.RelayID,
	}); err != nil {
		t.Fatalf("send kill: %v", err)
	}

	if err := cs.WaitDone(10 * time.Second); err != nil {
		t.Fatalf("client did not exit after kill: %v", err)
	}
}

// TestIntegration_Daemon_Permission_Allow exercises the full
// interpose→daemon→server permission round-trip with an "allow"
// decision. mock_claude exec()s a non-safe-list binary, the C library
// intercepts the spawn, the request flows over /ws/daemon as
// permission_request, the mock server replies allow keyed by
// request_id, the spawn proceeds, and mock_claude reports "ok".
//
// macOS-only: on Linux, the Go runtime's direct syscall use evades
// LD_PRELOAD for many operations, and the spawn-helper-based gating
// path differs in ways the existing seccomp/agent_test.py harness
// already covers.
func TestIntegration_Daemon_Permission_Allow(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("interpose spawn interception via DYLD is macOS-specific in this harness")
	}
	runPermissionTest(t, "allow")
}

// TestIntegration_Daemon_Permission_Deny is the deny counterpart. The
// mock server denies the spawn; the C library writes
// "[GREENLIGHT] Permission denied." to stderr (which appears on the
// client PTY in daemon mode) and returns an error to mock_claude, which
// reports "err: ...".
func TestIntegration_Daemon_Permission_Deny(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("interpose spawn interception via DYLD is macOS-specific in this harness")
	}
	runPermissionTest(t, "deny")
}

// runPermissionTest is the shared body for the allow/deny variants.
func runPermissionTest(t *testing.T, decision string) {
	t.Helper()
	testServerURL.ClearHandlers()

	workDir := t.TempDir()
	resultPath := filepath.Join(workDir, "exec-result")
	interposeLog := filepath.Join(t.TempDir(), "interpose.log")
	cs, cleanup := startConnectSession(t, connectOpts{
		EnableInterpose:      true,
		SkipMockClaudeOutput: true,
		AgentEnv: []string{
			// /bin/sleep is not in the C library's safe-spawn list, so it
			// triggers a permission request. 0.01s is fast enough not to
			// stretch the test if the spawn is allowed.
			"MOCK_CLAUDE_EXEC=/bin/sleep 0.01",
			"MOCK_CLAUDE_EXEC_RESULT=" + resultPath,
			"GREENLIGHT_INTERPOSE_LOG=" + interposeLog,
		},
	})
	defer cleanup()
	defer func() {
		if t.Failed() {
			t.Logf("interpose log:\n%s", readFileOrEmpty(interposeLog))
		}
	}()

	stop := startPermissionAutoResponder(t, cs.Sess, func(pr permissionRequest) string {
		return decision
	})

	// Drain the PTY in the background to keep the master from blocking
	// on a full buffer; we don't assert on its contents because the
	// agent's stderr (where the [GREENLIGHT] denial banner is written)
	// may not be relayed onto the connect-side PTY in daemon mode.
	go drainPTY(cs.Master, 15*time.Second)

	if err := cs.WaitDone(15 * time.Second); err != nil {
		t.Fatalf("client did not exit: %v", err)
	}
	seen := stop()

	if !sawSpawnOf("sleep", seen) {
		t.Errorf("expected a Bash permission_request invoking sleep, got %v",
			summarizeRequests(seen))
	}

	result := readFileOrEmpty(resultPath)
	switch decision {
	case "allow":
		if !strings.HasPrefix(result, "ok") {
			t.Errorf("expected exec ok, got %q", result)
		}
	case "deny":
		if !strings.HasPrefix(result, "err") {
			t.Errorf("expected exec err (spawn rejected by interpose), got %q", result)
		}
	}
}

// TestIntegration_Daemon_Seccomp_Allow exercises the Linux seccomp
// supervisor permission round-trip with an "allow" decision. mock_claude
// (built with CGO so the interpose .so loads via LD_PRELOAD) opens a
// file for writing under the user's real $HOME — outside the
// supervisor's auto-allow zones (tmp, system, dotfile, agent-internal).
// The kernel triggers SECCOMP_RET_USER_NOTIF; the supervisor reads the
// path from /proc/<pid>/mem, classifies it as a Write tool call, and
// long-polls the daemon WS. The mock server auto-allows; the supervisor
// continues the syscall; mock_claude reports "ok".
//
// Linux-only: the seccomp USER_NOTIF mechanism requires kernel 5.9+.
// macOS uses the dyld interpose path tested by Permission_Allow/Deny.
func TestIntegration_Daemon_Seccomp_Allow(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("seccomp supervisor is Linux-only")
	}
	runSeccompTest(t, "allow")
}

// TestIntegration_Daemon_Seccomp_Deny is the deny counterpart. The mock
// server denies; the supervisor returns ENOSYS for the openat; mock_claude
// reports an error.
func TestIntegration_Daemon_Seccomp_Deny(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("seccomp supervisor is Linux-only")
	}
	runSeccompTest(t, "deny")
}

// runSeccompTest is the shared body for the allow/deny variants.
func runSeccompTest(t *testing.T, decision string) {
	t.Helper()
	testServerURL.ClearHandlers()

	// The write target must live outside /tmp (which the seccomp
	// supervisor auto-allows). Create a fresh dir under the real user
	// HOME — the test's HOME override is itself under /tmp, which won't
	// trip the filter.
	realHome, err := os.UserHomeDir()
	if err != nil || realHome == "" {
		t.Skip("cannot determine real user HOME for seccomp write target")
	}
	writeDir, err := os.MkdirTemp(realHome, "greenlight-test-seccomp-*")
	if err != nil {
		t.Fatalf("mkdir under HOME: %v", err)
	}
	defer os.RemoveAll(writeDir)
	writeFile := filepath.Join(writeDir, "out.txt")
	// Result file lives under /tmp so it bypasses the seccomp filter —
	// otherwise a "deny" decision would also block the result write,
	// leaving us unable to observe the outcome.
	resultFile := filepath.Join(t.TempDir(), "result.txt")

	cs, cleanup := startConnectSession(t, connectOpts{
		EnableInterpose:      true,
		SkipMockClaudeOutput: true,
		AgentEnv: []string{
			"MOCK_CLAUDE_WRITE_FILE=" + writeFile,
			"MOCK_CLAUDE_WRITE_RESULT=" + resultFile,
		},
	})
	defer cleanup()

	stop := startPermissionAutoResponder(t, cs.Sess, func(pr permissionRequest) string {
		return decision
	})

	// Drain the PTY in the background so the buffer doesn't fill.
	go drainPTY(cs.Master, 15*time.Second)

	if err := cs.WaitDone(15 * time.Second); err != nil {
		t.Fatalf("client did not exit: %v", err)
	}
	seen := stop()

	// Look for a Write request targeting our file.
	if !sawWriteOf(writeFile, seen) {
		t.Errorf("expected a Write permission_request for %s, got %v",
			writeFile, summarizeRequests(seen))
	}

	result := readFileOrEmpty(resultFile)
	switch decision {
	case "allow":
		if !strings.HasPrefix(result, "ok") {
			t.Errorf("expected write ok, got %q", result)
		}
		if _, err := os.Stat(writeFile); err != nil {
			t.Errorf("expected target file written: %v", err)
		}
	case "deny":
		if !strings.HasPrefix(result, "err") {
			t.Errorf("expected write err (rejected by seccomp), got %q", result)
		}
	}
}

// sawWriteOf returns true if any observed permission request was a
// Write/Edit on the given file path.
func sawWriteOf(path string, reqs []permissionRequest) bool {
	for _, r := range reqs {
		if r.Data.Tool != "Write" && r.Data.Tool != "Edit" {
			continue
		}
		if fp, _ := r.Data.Input["file_path"].(string); fp == path {
			return true
		}
	}
	return false
}

// sawSpawnOf returns true if any observed permission request was a Bash
// invocation whose command contains the given binary name.
func sawSpawnOf(name string, reqs []permissionRequest) bool {
	for _, r := range reqs {
		if r.Data.Tool != "Bash" {
			continue
		}
		if cmd, _ := r.Data.Input["command"].(string); strings.Contains(cmd, name) {
			return true
		}
	}
	return false
}

func summarizeRequests(reqs []permissionRequest) []string {
	out := make([]string, len(reqs))
	for i, r := range reqs {
		out[i] = fmt.Sprintf("%s(%v)", r.Data.Tool, r.Data.Input)
	}
	return out
}

// drainPTY reads from a PTY master for up to timeout, accumulating
// whatever bytes arrive. Returns when the read times out (no more data
// for one polling interval) or the timeout elapses overall.
func drainPTY(master *os.File, timeout time.Duration) string {
	var buf bytes.Buffer
	deadline := time.Now().Add(timeout)
	tmp := make([]byte, 4096)
	for time.Now().Before(deadline) {
		master.SetReadDeadline(time.Now().Add(150 * time.Millisecond))
		n, err := master.Read(tmp)
		if n > 0 {
			buf.Write(tmp[:n])
		}
		if err != nil {
			if os.IsTimeout(err) {
				if buf.Len() > 0 {
					return buf.String()
				}
				continue
			}
			break
		}
	}
	return buf.String()
}

// TestIntegration_Daemon_SessionTranscript exercises the
// session_transcript control frame end-to-end:
//
//   - Plant a Claude-format JSONL at the path the daemon's transcript
//     scanner would find (~/.claude/projects/<dir>/<sessionID>.jsonl
//     under the daemon's HOME, with a matching `cwd` field).
//   - Wait for startTranscriptStreamer to discover it and persist the
//     conversation→relay mapping.
//   - Send session_transcript over /ws/daemon, assert the reply carries
//     the entries we wrote, in order.
//
// Per-agent transcript-format normalization (codex/copilot/cursor/pi
// transformers) is exercised separately in stream_test.go.
func TestIntegration_Daemon_SessionTranscript(t *testing.T) {
	testServerURL.ClearHandlers()
	cs, cleanup := startConnectSession(t)
	defer cleanup()

	// Plant a Claude transcript at ~/.claude/projects/<anything>/<convID>.jsonl
	// in the daemon's HOME. The basename (minus ext) becomes the
	// conversation ID once startTranscriptStreamer notices the file.
	projDir := filepath.Join(cs.DaemonHome, ".claude", "projects", "test-proj")
	if err := os.MkdirAll(projDir, 0755); err != nil {
		t.Fatal(err)
	}
	convID := "abcd1234-5678-90ab-cdef-1234567890ab"
	transcriptPath := filepath.Join(projDir, convID+".jsonl")
	lines := []string{
		`{"type":"summary","summary":"Test session","cwd":"` + cs.Workdir + `"}`,
		`{"type":"user","message":{"role":"user","content":"hello"},"cwd":"` + cs.Workdir + `"}`,
		`{"type":"assistant","message":{"role":"assistant","content":"hi"},"cwd":"` + cs.Workdir + `"}`,
	}
	if err := os.WriteFile(transcriptPath,
		[]byte(strings.Join(lines, "\n")+"\n"), 0644); err != nil {
		t.Fatal(err)
	}

	// Directly seed the conversation_id → relay_id mapping the daemon's
	// handler reads via lookupConversationID. The transcript streamer
	// would normally populate this once it discovered the agent's real
	// transcript, but mock_claude doesn't write one, so we write it
	// ourselves to get the same effect.
	seedRelayMapping(t, cs.DaemonHome, convID, cs.Sess.RelayID)

	if err := cs.Sess.SendBinary(map[string]any{
		"type":     "session_transcript",
		"relay_id": cs.Sess.RelayID,
	}); err != nil {
		t.Fatalf("send session_transcript: %v", err)
	}

	matched := cs.Sess.WaitForFrame(func(raw json.RawMessage) bool {
		var hdr struct {
			Type string `json:"type"`
		}
		return json.Unmarshal(raw, &hdr) == nil && hdr.Type == "session_transcript_response"
	}, 5*time.Second)
	if matched == nil {
		t.Fatal("did not receive session_transcript_response within 5s")
	}

	var reply struct {
		Type    string            `json:"type"`
		RelayID string            `json:"relay_id"`
		Agent   string            `json:"agent"`
		Message string            `json:"message"`
		Entries []json.RawMessage `json:"entries"`
	}
	if err := json.Unmarshal(matched, &reply); err != nil {
		t.Fatalf("parse session_transcript_response: %v", err)
	}
	if reply.Message != "" {
		t.Fatalf("unexpected error message: %q", reply.Message)
	}
	if reply.Agent != "claude-code" {
		t.Errorf("agent mismatch: got %q want claude-code", reply.Agent)
	}
	if len(reply.Entries) != 3 {
		t.Errorf("entries count: got %d want 3 (raw=%s)", len(reply.Entries), string(matched))
	}
	if len(reply.Entries) >= 2 {
		var entry struct {
			Type string `json:"type"`
		}
		if err := json.Unmarshal(reply.Entries[1], &entry); err != nil || entry.Type != "user" {
			t.Errorf("entry[1] parse: type=%q err=%v", entry.Type, err)
		}
	}

	cs.Wait(10 * time.Second)
}

// TestIntegration_Daemon_AwaitUser drives the ticket-handoff IPC verb end to end:
// an `await_user` IPC for a live session must make the daemon emit a
// session_await_user envelope (relay_id-tagged) on the daemon WS. This is the new
// wire path that lets `greenlight ticket submit/approve/reject/merge/close` flip
// the session to "waiting" and suppress its idle push.
func TestIntegration_Daemon_AwaitUser(t *testing.T) {
	testServerURL.ClearHandlers()
	cs, cleanup := startConnectSession(t)
	defer cleanup()

	resp := daemonIPC(t, cs.SockPath, ipcRequest{Type: "await_user", RelayID: cs.Sess.RelayID})
	if resp.Type != "ok" {
		t.Fatalf("await_user ipc response = %q, want ok (msg=%q)", resp.Type, resp.Message)
	}

	matched := cs.Sess.WaitForFrame(func(raw json.RawMessage) bool {
		var hdr struct {
			Type    string `json:"type"`
			RelayID string `json:"relay_id"`
		}
		return json.Unmarshal(raw, &hdr) == nil &&
			hdr.Type == "session_await_user" && hdr.RelayID == cs.Sess.RelayID
	}, 5*time.Second)
	if matched == nil {
		t.Fatalf("did not receive session_await_user for relay %q within 5s", cs.Sess.RelayID)
	}

	cs.Wait(10 * time.Second)
}

// TestIntegration_Daemon_ScratchpadRoot exercises issue #182 end to end with the
// real daemon + transcript streamer: with scratch_auto=false (so the blanket
// /private/tmp root is NOT reported), planting a Claude transcript must make the
// daemon discover the session UUID and push a session_roots frame carrying the
// session's scratchpad as a kind:"scratch" root — derived solely from the
// transcript path, scoped to this session's UUID.
func TestIntegration_Daemon_ScratchpadRoot(t *testing.T) {
	testServerURL.ClearHandlers()
	// Have mock_claude dump its argv so we can recover the daemon-generated
	// --session-id (Claude names both the transcript file and the scratchpad
	// dir by it), then plant the transcript where the streamer scans.
	argsFile := filepath.Join(t.TempDir(), "claude-args.txt")
	cs, cleanup := startConnectSession(t, connectOpts{
		ConfigSeed: map[string]string{"scratch_auto": "false"},
		AgentEnv:   []string{"MOCK_CLAUDE_ARGS_FILE=" + argsFile},
	})
	defer cleanup()

	// Recover the session id from the dumped argv.
	var sessID string
	for deadline := time.Now().Add(5 * time.Second); time.Now().Before(deadline) && sessID == ""; {
		data, err := os.ReadFile(argsFile)
		if err != nil {
			time.Sleep(100 * time.Millisecond)
			continue
		}
		fields := strings.Split(string(data), "\n")
		for i, f := range fields {
			if f == "--session-id" && i+1 < len(fields) {
				sessID = strings.TrimSpace(fields[i+1])
			}
		}
	}
	if sessID == "" {
		t.Fatalf("could not recover --session-id from mock_claude argv (%q)", readFileOrEmpty(argsFile))
	}

	// Plant the transcript at the path the daemon's by-ID scanner looks for:
	// <daemonHome>/.claude/projects/<dir>/<sessID>.jsonl.
	projDir := filepath.Join(cs.DaemonHome, ".claude", "projects", "scratch-proj")
	if err := os.MkdirAll(projDir, 0755); err != nil {
		t.Fatal(err)
	}
	transcriptPath := filepath.Join(projDir, sessID+".jsonl")
	if err := os.WriteFile(transcriptPath,
		[]byte(`{"type":"summary","summary":"s","cwd":"`+cs.Workdir+`"}`+"\n"), 0644); err != nil {
		t.Fatal(err)
	}

	// The path the CLI should report — derived the same way it does internally.
	wantScratch := claudeScratchpadDir("claude", transcriptPath)
	if wantScratch == "" {
		t.Fatal("claudeScratchpadDir returned empty for the planted transcript")
	}
	if !strings.HasSuffix(wantScratch, "/"+sessID+"/scratchpad") {
		t.Fatalf("derived scratchpad %q is not scoped to the session UUID %q", wantScratch, sessID)
	}

	// Wait for a session_roots frame in the daemon inbox carrying it as scratch.
	deadline := time.Now().Add(10 * time.Second)
	var found bool
	var seenTypes []string
	for time.Now().Before(deadline) && !found {
		for _, raw := range cs.Sess.Inbox() {
			var env struct {
				Type string `json:"type"`
				Data struct {
					Roots []SessionRoot `json:"roots"`
				} `json:"data"`
			}
			if json.Unmarshal(raw, &env) != nil {
				continue
			}
			if env.Type != "session_roots" {
				continue
			}
			for _, r := range env.Data.Roots {
				if r.Path == wantScratch && r.Kind == cliRootKindScratch {
					found = true
				}
			}
		}
		if !found {
			time.Sleep(200 * time.Millisecond)
		}
	}
	if !found {
		for _, raw := range cs.Sess.Inbox() {
			var hdr struct {
				Type string `json:"type"`
			}
			if json.Unmarshal(raw, &hdr) == nil {
				seenTypes = append(seenTypes, hdr.Type)
			}
		}
		t.Fatalf("no session_roots frame carried scratchpad root %q; inbox frame types=%v", wantScratch, seenTypes)
	}

	cs.Wait(10 * time.Second)
}

// seedRelayMapping writes a conversation_id → relay_id entry into the
// daemon's sessions.json (the file that lookupConversationID reads).
// Used by transcript tests to skip the live transcript-discovery path
// and get straight to the handler under test.
// seedDaemonConfig appends key=value entries to the daemon's config file. The
// daemon writes its own keys (agent, host_id) at startup, and the file ends with
// a newline, so appending is safe — the connect session reads config fresh.
func seedDaemonConfig(t *testing.T, daemonHome string, kv map[string]string) {
	t.Helper()
	dir := filepath.Join(daemonHome, ".greenlight")
	if err := os.MkdirAll(dir, 0755); err != nil {
		t.Fatal(err)
	}
	f, err := os.OpenFile(filepath.Join(dir, "config"), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0644)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	for k, v := range kv {
		if _, err := fmt.Fprintf(f, "%s=%s\n", k, v); err != nil {
			t.Fatal(err)
		}
	}
}

func seedRelayMapping(t *testing.T, daemonHome, convID, relayID string) {
	t.Helper()
	dir := filepath.Join(daemonHome, ".greenlight")
	if err := os.MkdirAll(dir, 0755); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "sessions.json")
	m := map[string]string{convID: relayID}
	if existing, err := os.ReadFile(path); err == nil {
		json.Unmarshal(existing, &m)
		m[convID] = relayID
	}
	data, _ := json.Marshal(m)
	if err := os.WriteFile(path, data, 0644); err != nil {
		t.Fatal(err)
	}
}

// TestIntegration_Daemon_SessionTranscript_NoFile verifies the
// graceful-error path when the daemon can't find a transcript for the
// requested relay_id. Reply should carry a non-empty message and zero
// entries (rather than crashing or omitting the response).
func TestIntegration_Daemon_SessionTranscript_NoFile(t *testing.T) {
	testServerURL.ClearHandlers()
	cs, cleanup := startConnectSession(t)
	defer cleanup()

	// No transcript file planted — daemon's scan should return empty.
	if err := cs.Sess.SendBinary(map[string]any{
		"type":     "session_transcript",
		"relay_id": cs.Sess.RelayID,
	}); err != nil {
		t.Fatalf("send session_transcript: %v", err)
	}

	matched := cs.Sess.WaitForFrame(func(raw json.RawMessage) bool {
		var hdr struct {
			Type string `json:"type"`
		}
		return json.Unmarshal(raw, &hdr) == nil && hdr.Type == "session_transcript_response"
	}, 5*time.Second)
	if matched == nil {
		t.Fatal("expected session_transcript_response (with error) within 5s")
	}

	var reply struct {
		Message string            `json:"message"`
		Entries []json.RawMessage `json:"entries"`
	}
	if err := json.Unmarshal(matched, &reply); err != nil {
		t.Fatalf("parse: %v", err)
	}
	if reply.Message == "" {
		t.Error("expected error message, got empty")
	}

	cs.Wait(10 * time.Second)
}

// TestIntegration_Daemon_HistoryEntry_Roundtrip pushes a history_entry
// control frame to record a permission outcome, then queries via
// project_history and asserts the entry comes back.
func TestIntegration_Daemon_HistoryEntry_Roundtrip(t *testing.T) {
	testServerURL.ClearHandlers()
	cs, cleanup := startConnectSession(t)
	defer cleanup()

	entry := map[string]any{
		"request_id":   "req-1",
		"tool_name":    "Bash",
		"tool_input":   map[string]any{"command": "ls"},
		"outcome":      "allowed",
		"agent":        "claude",
		"responded_at": "2026-05-09T00:00:00Z",
	}
	if err := cs.Sess.SendBinary(map[string]any{
		"type":    "history_entry",
		"project": "test-proj",
		"entry":   entry,
	}); err != nil {
		t.Fatalf("send history_entry: %v", err)
	}

	// Give the daemon a tick to flush the entry to disk.
	time.Sleep(150 * time.Millisecond)

	if err := cs.Sess.SendBinary(map[string]any{
		"type":    "project_history",
		"project": "test-proj",
	}); err != nil {
		t.Fatalf("send project_history: %v", err)
	}

	matched := cs.Sess.WaitForFrame(func(raw json.RawMessage) bool {
		var hdr struct {
			Type string `json:"type"`
		}
		return json.Unmarshal(raw, &hdr) == nil && hdr.Type == "project_history_response"
	}, 5*time.Second)
	if matched == nil {
		t.Fatal("did not receive project_history_response within 5s")
	}

	var reply struct {
		Type    string `json:"type"`
		Project string `json:"project"`
		Entries []struct {
			RequestID string `json:"request_id"`
			ToolName  string `json:"tool_name"`
			Outcome   string `json:"outcome"`
		} `json:"entries"`
	}
	if err := json.Unmarshal(matched, &reply); err != nil {
		t.Fatalf("parse project_history_response: %v (raw=%s)", err, string(matched))
	}
	if reply.Project != "test-proj" {
		t.Errorf("project mismatch: got %q", reply.Project)
	}
	found := false
	for _, e := range reply.Entries {
		if e.RequestID == "req-1" && e.ToolName == "Bash" && e.Outcome == "allowed" {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("did not find req-1 in entries: %+v", reply.Entries)
	}

	cs.Wait(10 * time.Second)
}

// TestIntegration_Daemon_SessionHistory_ControlFrame validates the
// control-frame variant of session_history (distinct from the IPC-based
// daemon status query). The reply lists persisted sessionRecord
// entries; we don't assert on contents (the live test session may or
// may not produce a record by the time we ask), only on the wire shape.
func TestIntegration_Daemon_SessionHistory_ControlFrame(t *testing.T) {
	testServerURL.ClearHandlers()
	cs, cleanup := startConnectSession(t)
	defer cleanup()

	if err := cs.Sess.SendBinary(map[string]any{
		"type": "session_history",
	}); err != nil {
		t.Fatalf("send session_history: %v", err)
	}

	matched := cs.Sess.WaitForFrame(func(raw json.RawMessage) bool {
		var hdr struct {
			Type string `json:"type"`
		}
		return json.Unmarshal(raw, &hdr) == nil && hdr.Type == "session_history_response"
	}, 5*time.Second)
	if matched == nil {
		t.Fatal("did not receive session_history_response within 5s")
	}

	var reply struct {
		Type    string            `json:"type"`
		Entries []json.RawMessage `json:"entries"`
	}
	if err := json.Unmarshal(matched, &reply); err != nil {
		t.Fatalf("parse session_history_response: %v (raw=%s)", err, string(matched))
	}
	if reply.Entries == nil {
		// Empty slice is fine; nil suggests the daemon serialized null
		// where it should have used [].
		t.Errorf("entries was nil — should be [] on the wire")
	}

	cs.Wait(10 * time.Second)
}

// TestIntegration_Daemon_DeleteSession_Unknown verifies the daemon
// gracefully handles delete_session for a relay_id with no persisted
// record (the common case when the session never produced a transcript
// — saveSessionRecord is gated on transcript availability). The daemon
// should log and ignore, not crash or reply.
func TestIntegration_Daemon_DeleteSession_Unknown(t *testing.T) {
	testServerURL.ClearHandlers()
	cs, cleanup := startConnectSession(t)
	defer cleanup()

	if err := cs.Sess.SendBinary(map[string]any{
		"type":     "delete_session",
		"relay_id": "00000000-0000-0000-0000-deadbeef0000",
	}); err != nil {
		t.Fatalf("send delete_session: %v", err)
	}

	// Give the daemon a moment to handle the frame, then confirm it's
	// still alive by hitting it with a list_skills round-trip we already
	// know works.
	time.Sleep(150 * time.Millisecond)
	if err := cs.Sess.SendBinary(map[string]any{
		"type":     "list_skills",
		"relay_id": cs.Sess.RelayID,
	}); err != nil {
		t.Fatalf("send list_skills: %v", err)
	}
	awaitSkillsListed(t, cs.Sess, 5*time.Second)

	cs.Wait(10 * time.Second)
}

// ---------- skill helpers ----------

func writeTestSkill(t *testing.T, workdir, name string) {
	t.Helper()
	dir := filepath.Join(workdir, ".claude", "skills", "_greenlight", name)
	if err := os.MkdirAll(dir, 0755); err != nil {
		t.Fatal(err)
	}
	body := "---\nname: " + name + "\ndescription: test\n---\nbody\n"
	if err := os.WriteFile(filepath.Join(dir, "SKILL.md"), []byte(body), 0644); err != nil {
		t.Fatal(err)
	}
}

type skillsListedReply struct {
	Type    string   `json:"type"`
	RelayID string   `json:"relay_id"`
	Skills  []string `json:"skills"`
}

// permissionRequest is the payload of a permission_request frame as
// observed by the mock server. Only the fields the tests care about are
// modeled; any others are ignored.
type permissionRequest struct {
	Type string `json:"type"`
	Data struct {
		RequestID string                 `json:"request_id"`
		Tool      string                 `json:"tool_name"`
		Input     map[string]interface{} `json:"tool_input"`
	} `json:"data"`
}

// startPermissionAutoResponder runs a goroutine that watches the
// session's inbox for permission_request frames and sends a
// permission_response back. The decide callback returns "allow" or
// "deny" given the request. Calling the returned stop func waits for
// the goroutine and returns the requests it answered (in order).
func startPermissionAutoResponder(
	t *testing.T,
	sess *mockserver.Session,
	decide func(permissionRequest) string,
) func() []permissionRequest {
	t.Helper()
	stopCh := make(chan struct{})
	doneCh := make(chan []permissionRequest, 1)

	go func() {
		var seen []permissionRequest
		handled := make(map[string]bool)
		ticker := time.NewTicker(20 * time.Millisecond)
		defer ticker.Stop()
		for {
			for _, raw := range sess.Inbox() {
				var hdr struct {
					Type string `json:"type"`
				}
				if json.Unmarshal(raw, &hdr) != nil || hdr.Type != "permission_request" {
					continue
				}
				var pr permissionRequest
				if json.Unmarshal(raw, &pr) != nil || pr.Data.RequestID == "" || handled[pr.Data.RequestID] {
					continue
				}
				handled[pr.Data.RequestID] = true
				seen = append(seen, pr)
				behavior := decide(pr)
				_ = sess.Send(map[string]interface{}{
					"type":       "permission_response",
					"request_id": pr.Data.RequestID,
					"behavior":   behavior,
					"message":    "test " + behavior,
				})
			}
			select {
			case <-stopCh:
				doneCh <- seen
				return
			case <-ticker.C:
			}
		}
	}()

	return func() []permissionRequest {
		close(stopCh)
		return <-doneCh
	}
}

func awaitSkillsListed(t *testing.T, sess *mockserver.Session, timeout time.Duration) skillsListedReply {
	t.Helper()
	matched := sess.WaitForFrame(func(raw json.RawMessage) bool {
		var hdr struct {
			Type string `json:"type"`
		}
		return json.Unmarshal(raw, &hdr) == nil && hdr.Type == "skills_listed"
	}, timeout)
	if matched == nil {
		t.Fatalf("did not receive skills_listed within %v", timeout)
	}
	var reply skillsListedReply
	if err := json.Unmarshal(matched, &reply); err != nil {
		t.Fatalf("parse skills_listed: %v (raw=%s)", err, string(matched))
	}
	return reply
}

type suggestedPromptReply struct {
	Type    string `json:"type"`
	RelayID string `json:"relay_id"`
	Text    string `json:"text"`
}

func awaitSuggestedPrompt(t *testing.T, sess *mockserver.Session, timeout time.Duration) suggestedPromptReply {
	t.Helper()
	matched := sess.WaitForFrame(func(raw json.RawMessage) bool {
		var hdr struct {
			Type string `json:"type"`
		}
		return json.Unmarshal(raw, &hdr) == nil && hdr.Type == "suggested_prompt"
	}, timeout)
	if matched == nil {
		t.Fatalf("did not receive suggested_prompt within %v", timeout)
	}
	var reply suggestedPromptReply
	if err := json.Unmarshal(matched, &reply); err != nil {
		t.Fatalf("parse suggested_prompt: %v (raw=%s)", err, string(matched))
	}
	return reply
}

type configSetResultReply struct {
	Type      string `json:"type"`
	RequestID string `json:"request_id"`
	Success   bool   `json:"success"`
	Error     string `json:"error"`
}

type configLoadedReply struct {
	Type      string `json:"type"`
	RequestID string `json:"request_id"`
	Config    struct {
		Host    map[string]string `json:"host"`
		Project map[string]string `json:"project"`
	} `json:"config"`
	Error string `json:"error"`
}

func awaitConfigSetResult(t *testing.T, sess *mockserver.Session, requestID string, timeout time.Duration) configSetResultReply {
	t.Helper()
	matched := sess.WaitForFrame(func(raw json.RawMessage) bool {
		var hdr struct {
			Type      string `json:"type"`
			RequestID string `json:"request_id"`
		}
		return json.Unmarshal(raw, &hdr) == nil && hdr.Type == "config_set_result" && hdr.RequestID == requestID
	}, timeout)
	if matched == nil {
		t.Fatalf("did not receive config_set_result for %q within %v", requestID, timeout)
	}
	var reply configSetResultReply
	if err := json.Unmarshal(matched, &reply); err != nil {
		t.Fatalf("parse config_set_result: %v (raw=%s)", err, string(matched))
	}
	return reply
}

func awaitConfigLoaded(t *testing.T, sess *mockserver.Session, requestID string, timeout time.Duration) configLoadedReply {
	t.Helper()
	matched := sess.WaitForFrame(func(raw json.RawMessage) bool {
		var hdr struct {
			Type      string `json:"type"`
			RequestID string `json:"request_id"`
		}
		return json.Unmarshal(raw, &hdr) == nil && hdr.Type == "config_loaded" && hdr.RequestID == requestID
	}, timeout)
	if matched == nil {
		t.Fatalf("did not receive config_loaded for %q within %v", requestID, timeout)
	}
	var reply configLoadedReply
	if err := json.Unmarshal(matched, &reply); err != nil {
		t.Fatalf("parse config_loaded: %v (raw=%s)", err, string(matched))
	}
	return reply
}

// connectSession is the live state needed to drive a `greenlight connect`
// session in a test: the running client process, the PTY master used to
// inject input, the agent workdir, the daemon's home dir (for tests
// that need to pre-populate scanned paths like ~/.claude/projects), and
// the registered mock-server session.
type connectSession struct {
	Client     *exec.Cmd
	Master     *os.File
	Workdir    string
	DaemonHome string
	DaemonTmp  string // daemon's TMPDIR; per-session shim dir lives here
	SockPath   string // daemon IPC socket, for driving subcommand verbs
	Sess       *mockserver.Session
	done       chan error
}

// Wait sends "done\n" to mock_claude (which exits on stdin input) and
// waits for the client to exit, with a fallback kill on timeout.
func (cs *connectSession) Wait(timeout time.Duration) {
	cs.Master.Write([]byte("done\n"))
	select {
	case <-cs.done:
	case <-time.After(timeout):
		cs.Client.Process.Kill()
		<-cs.done
	}
}

// WaitDone blocks until the client exits (used after externally-driven
// shutdown like a kill control frame). Returns nil if the client exited
// (regardless of exit code — kill makes nonzero codes expected) and an
// error only on timeout.
func (cs *connectSession) WaitDone(timeout time.Duration) error {
	select {
	case <-cs.done:
		return nil
	case <-time.After(timeout):
		cs.Client.Process.Kill()
		<-cs.done
		return fmt.Errorf("timeout after %v", timeout)
	}
}

// connectOpts tunes startConnectSession. Zero value is the default
// (interpose disabled, mock_claude waits on stdin).
type connectOpts struct {
	// EnableInterpose runs the agent with the real interpose library
	// loaded. macOS only — Linux Go binaries make raw syscalls that
	// LD_PRELOAD can't catch.
	EnableInterpose bool
	// AgentEnv is appended to the connect client's env. Use this to set
	// MOCK_CLAUDE_* variables.
	AgentEnv []string
	// SkipMockClaudeOutput omits MOCK_CLAUDE_OUTPUT, letting mock_claude
	// run to completion instead of blocking on stdin.
	SkipMockClaudeOutput bool
	// ConfigSeed writes key=value entries into the daemon's ~/.greenlight/config
	// before the connect session starts (e.g. tickets_secret for shim tests).
	ConfigSeed map[string]string
	// DaemonEnv is appended to the daemon process's env (e.g. to enable
	// experimental features gated behind an env var).
	DaemonEnv []string
}

// startConnectSession boots a daemon (with a fresh enrolled host),
// launches `greenlight connect` against mock_claude, and waits for the
// agent to start and the mock-server session to register. Returns the
// live state plus the test cleanup func to defer.
//
// The caller is responsible for resetting any mock-server hooks BEFORE
// invoking this — handlers set after this returns won't apply to the
// session_started ack.
func startConnectSession(t *testing.T, opts ...connectOpts) (*connectSession, func()) {
	t.Helper()
	var opt connectOpts
	if len(opts) > 0 {
		opt = opts[0]
	}

	hostID := enrollTestHost(t, "test-dev")
	daemonEnv := []string{
		"GREENLIGHT_DEVICE_ID=test-dev",
		"GREENLIGHT_DAEMON_SESSION_ID=" + hostID,
	}
	if !opt.EnableInterpose {
		daemonEnv = append(daemonEnv, "GREENLIGHT_DISABLE_INTERPOSE=1")
	}
	daemonEnv = append(daemonEnv, opt.DaemonEnv...)
	sockPath, tmpDir, daemonHome, daemonCleanup := startTestDaemonWithHome(t, t.TempDir(), daemonEnv...)

	if len(opt.ConfigSeed) > 0 {
		seedDaemonConfig(t, daemonHome, opt.ConfigSeed)
	}

	workDir := t.TempDir()
	pathWithMock := filepath.Dir(mockClaudeBin) + ":" + os.Getenv("PATH")

	master, slave, err := openPTY()
	if err != nil {
		daemonCleanup()
		t.Fatalf("openPTY: %v", err)
	}
	setWinsize(slave.Fd(), &Winsize{Row: 24, Col: 80})

	clientLog := filepath.Join(tmpDir, "client.log")
	client := exec.Command(greenlightBin, "connect",
		"--device-id", "test-dev",
		"--project", "test-proj",
	)
	client.Dir = workDir
	clientEnv := []string{
		"HOME=" + t.TempDir(),
		"PATH=" + pathWithMock,
		"TMPDIR=" + tmpDir,
		"TERM=xterm-256color",
		"GREENLIGHT_DAEMON_SOCK=" + sockPath,
		"GREENLIGHT_LOG=" + clientLog,
	}
	if !opt.SkipMockClaudeOutput {
		clientEnv = append(clientEnv, "MOCK_CLAUDE_OUTPUT="+filepath.Join(workDir, "claude-stdin.txt"))
	}
	clientEnv = append(clientEnv, opt.AgentEnv...)
	client.Env = clientEnv
	client.Stdin = slave
	client.Stdout = slave
	client.Stderr = slave

	done := make(chan error, 1)
	if err := client.Start(); err != nil {
		master.Close()
		slave.Close()
		daemonCleanup()
		t.Fatalf("start client: %v", err)
	}
	slave.Close()
	// Close done after Wait so multiple receivers (test + cleanup) don't
	// deadlock when one of them has already drained the value.
	go func() {
		err := client.Wait()
		done <- err
		close(done)
	}()

	cleanup := func() {
		// If the client is still running, prod it. <-done is safe even
		// when already drained: the channel is closed and yields zero.
		if client.ProcessState == nil {
			client.Process.Kill()
		}
		<-done
		master.Close()
		if t.Failed() {
			t.Logf("client log:\n%s", readFileOrEmpty(clientLog))
		}
		daemonCleanup()
	}

	got := readPTYUntil(t, master, "MOCK_CLAUDE_STARTED", 10*time.Second)
	if !strings.Contains(got, "MOCK_CLAUDE_STARTED") {
		cleanup()
		t.Fatalf("mock claude did not start; pty=%q", got)
	}

	sess := waitForOneSession(t, testServerURL, 5*time.Second)

	return &connectSession{
		Client:     client,
		Master:     master,
		Workdir:    workDir,
		DaemonHome: daemonHome,
		DaemonTmp:  tmpDir,
		SockPath:   sockPath,
		Sess:       sess,
		done:       done,
	}, cleanup
}

// waitForOneSession blocks until the mock server has exactly one
// registered session and returns it. Fails the test on timeout.
func waitForOneSession(t *testing.T, srv *mockserver.Server, timeout time.Duration) *mockserver.Session {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		sessions := srv.Sessions()
		if len(sessions) == 1 {
			return sessions[0]
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("expected 1 session within %v, got %d", timeout, len(srv.Sessions()))
	return nil
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

// TestIntegration_Daemon_TicketStage drives the full CLI stage round-trip:
// `greenlight ticket stage` → daemon IPC → daemon WS → (mock) server → back.
// Exercises the wire shapes of ticket_stage_get/set plus get/set/clear. The
// mock server keeps an in-memory scalar store standing in for ticket_stages.
func TestIntegration_Daemon_TicketStage(t *testing.T) {
	testServerURL.ClearHandlers()
	hostID := enrollTestHost(t, "test-dev")
	sockPath, _, daemonHome, cleanup := startTestDaemonWithHome(t, t.TempDir(),
		"GREENLIGHT_DEVICE_ID=test-dev",
		"GREENLIGHT_DAEMON_SESSION_ID="+hostID,
	)
	defer cleanup()
	// resolveStageTarget needs only the provider; the repo comes from the cwd's
	// origin remote (no provider token — stages never touch the provider API).
	seedDaemonConfig(t, daemonHome, map[string]string{"tickets_provider": "github"})

	repo := t.TempDir()
	for _, gitArgs := range [][]string{
		{"init"},
		{"remote", "add", "origin", "https://github.com/acme/widget.git"},
	} {
		cmd := exec.Command("git", gitArgs...)
		cmd.Dir = repo
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", gitArgs, err, out)
		}
	}

	env := append(os.Environ(),
		"HOME="+daemonHome,
		"GREENLIGHT_DAEMON_SOCK="+sockPath,
		"GREENLIGHT_PROJECT=test-proj",
	)
	runStage := func(args ...string) string {
		cmd := exec.Command(greenlightBin, append([]string{"ticket", "stage"}, args...)...)
		cmd.Dir = repo
		cmd.Env = env
		var stdout, stderr bytes.Buffer
		cmd.Stdout = &stdout
		cmd.Stderr = &stderr
		if err := cmd.Run(); err != nil {
			t.Fatalf("ticket stage %v: %v\nstderr: %s", args, err, stderr.String())
		}
		return strings.TrimSpace(stdout.String())
	}

	// Set the stage.
	if out := runStage("5", "in-progress"); out != "in-progress" {
		t.Fatalf("set stage output = %q, want in-progress", out)
	}
	// Bare read reflects the stored scalar.
	if out := runStage("5"); out != "in-progress" {
		t.Fatalf("get stage output = %q, want in-progress", out)
	}
	// Setting again replaces (single value).
	if out := runStage("5", "in-review"); out != "in-review" {
		t.Fatalf("replace stage output = %q, want in-review", out)
	}
	// Clear empties the stage (stdout has no stage line).
	if out := runStage("5", "--clear"); out != "" {
		t.Fatalf("--clear should print no stage, got: %q", out)
	}
}

func TestIntegration_Daemon_TicketTags(t *testing.T) {
	testServerURL.ClearHandlers()
	hostID := enrollTestHost(t, "test-dev")
	sockPath, _, daemonHome, cleanup := startTestDaemonWithHome(t, t.TempDir(),
		"GREENLIGHT_DEVICE_ID=test-dev",
		"GREENLIGHT_DAEMON_SESSION_ID="+hostID,
	)
	defer cleanup()
	// resolveTagTarget needs only the provider; the repo comes from the cwd's
	// origin remote (no provider token — tags never touch the provider API).
	seedDaemonConfig(t, daemonHome, map[string]string{"tickets_provider": "github"})

	// A git repo whose origin gives resolveTagTarget its repo_key.
	repo := t.TempDir()
	for _, gitArgs := range [][]string{
		{"init"},
		{"remote", "add", "origin", "https://github.com/acme/widget.git"},
	} {
		cmd := exec.Command("git", gitArgs...)
		cmd.Dir = repo
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", gitArgs, err, out)
		}
	}

	env := append(os.Environ(),
		"HOME="+daemonHome,
		"GREENLIGHT_DAEMON_SOCK="+sockPath,
		"GREENLIGHT_PROJECT=test-proj",
	)
	runTag := func(args ...string) string {
		cmd := exec.Command(greenlightBin, append([]string{"ticket", "tag"}, args...)...)
		cmd.Dir = repo
		cmd.Env = env
		var stdout, stderr bytes.Buffer
		cmd.Stdout = &stdout
		cmd.Stderr = &stderr
		if err := cmd.Run(); err != nil {
			t.Fatalf("ticket tag %v: %v\nstderr: %s", args, err, stderr.String())
		}
		return stdout.String()
	}

	// This test exercises the transport + client-side delta resolution; tag
	// normalization is server-side logic, unit-tested in the permit-cloud
	// package (the mock here just echoes the stored set), so we use
	// already-normalized tags.

	// Replace-set over the daemon WS.
	out := runTag("5", "--set", "blocked,foo")
	if !strings.Contains(out, "blocked") || !strings.Contains(out, "foo") {
		t.Fatalf("--set output missing tags: %q", out)
	}

	// Incremental: the CLI does get → merge (+urgent, -blocked) → set.
	out = runTag("5", "+urgent", "-blocked")
	if !strings.Contains(out, "urgent") || !strings.Contains(out, "foo") || strings.Contains(out, "blocked") {
		t.Fatalf("incremental delta resolution wrong: %q", out)
	}

	// Bare list reflects the stored set {foo, urgent}.
	out = runTag("5")
	if !strings.Contains(out, "foo") || !strings.Contains(out, "urgent") {
		t.Fatalf("list output wrong: %q", out)
	}

	// Clear empties the set (stdout has no tag lines).
	out = runTag("5", "--clear")
	if strings.TrimSpace(out) != "" {
		t.Fatalf("--clear should print no tags, got: %q", out)
	}
}

// TestIntegration_Daemon_TicketRejectTag drives the reject↔tag interaction
// (#106): `greenlight ticket reject` on a *-in-review ticket sets the
// "<phase>-rejected" tag, and a subsequent `submit` from *-in-progress clears
// it. A no-op reject (not in-review) touches no tags. Behavior is observed
// through the CLI's own stage/tag readers against the mock server's in-memory
// stores.
func TestIntegration_Daemon_TicketRejectTag(t *testing.T) {
	testServerURL.ClearHandlers()
	hostID := enrollTestHost(t, "test-dev")
	sockPath, _, daemonHome, cleanup := startTestDaemonWithHome(t, t.TempDir(),
		"GREENLIGHT_DEVICE_ID=test-dev",
		"GREENLIGHT_DAEMON_SESSION_ID="+hostID,
	)
	defer cleanup()
	seedDaemonConfig(t, daemonHome, map[string]string{"tickets_provider": "github"})

	repo := t.TempDir()
	for _, gitArgs := range [][]string{
		{"init"},
		{"remote", "add", "origin", "https://github.com/acme/widget.git"},
	} {
		cmd := exec.Command("git", gitArgs...)
		cmd.Dir = repo
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", gitArgs, err, out)
		}
	}

	env := append(os.Environ(),
		"HOME="+daemonHome,
		"GREENLIGHT_DAEMON_SOCK="+sockPath,
		"GREENLIGHT_PROJECT=test-proj",
	)
	run := func(args ...string) string {
		cmd := exec.Command(greenlightBin, append([]string{"ticket"}, args...)...)
		cmd.Dir = repo
		cmd.Env = env
		var stdout, stderr bytes.Buffer
		cmd.Stdout = &stdout
		cmd.Stderr = &stderr
		if err := cmd.Run(); err != nil {
			t.Fatalf("ticket %v: %v\nstderr: %s", args, err, stderr.String())
		}
		return strings.TrimSpace(stdout.String())
	}

	// Put the ticket at spec-in-review, then reject it.
	if out := run("stage", "5", "spec-in-review"); out != "spec-in-review" {
		t.Fatalf("seed stage = %q, want spec-in-review", out)
	}
	if out := run("reject", "5"); out != "spec-in-progress" {
		t.Fatalf("reject stage = %q, want spec-in-progress", out)
	}
	// Reject from *-in-review tags the bounce.
	if out := run("tag", "5"); !strings.Contains(out, "spec-rejected") {
		t.Fatalf("reject should add spec-rejected tag, got: %q", out)
	}

	// Submit from *-in-progress clears the bounce tag and re-enters review.
	if out := run("submit", "5"); out != "spec-in-review" {
		t.Fatalf("submit stage = %q, want spec-in-review", out)
	}
	if out := run("tag", "5"); strings.Contains(out, "spec-rejected") {
		t.Fatalf("submit should clear spec-rejected tag, got: %q", out)
	}

	// A no-op reject (ticket is in-review again, so reject moves it; instead
	// test the genuine no-op: reject an in-progress ticket touches no tags).
	if out := run("stage", "8", "code-in-progress"); out != "code-in-progress" {
		t.Fatalf("seed stage = %q, want code-in-progress", out)
	}
	if out := run("reject", "8"); out != "code-in-progress" {
		t.Fatalf("no-op reject stage = %q, want code-in-progress (unchanged)", out)
	}
	if out := run("tag", "8"); strings.TrimSpace(out) != "" {
		t.Fatalf("no-op reject should add no tag, got: %q", out)
	}
}
